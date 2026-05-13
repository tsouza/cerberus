package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubPinger implements Pinger with a swappable error + atomic call
// counter so tests can observe coalescing.
type stubPinger struct {
	mu    sync.Mutex
	err   error
	calls atomic.Int64
}

func (s *stubPinger) Ping(_ context.Context) error {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *stubPinger) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// TestHealthz_AlwaysOK confirms the liveness probe is dependency-free.
func TestHealthz_AlwaysOK(t *testing.T) {
	h := New(Options{
		Pinger: &stubPinger{err: errors.New("CH unreachable")},
	})
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("body = %q; want %q", got, "ok")
	}
}

// TestReadyz_OK exercises the happy path: CH ping passes and the
// schema startup hook reports ready.
func TestReadyz_OK(t *testing.T) {
	h := New(Options{
		Pinger:      &stubPinger{},
		SchemaReady: func() bool { return true },
		CacheTTL:    -1, // disable caching
	})

	rec := serveReadyz(t, h)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if resp["clickhouse"] != "ok" {
		t.Errorf("clickhouse = %q; want ok", resp["clickhouse"])
	}
	if resp["schema"] != "ready" {
		t.Errorf("schema = %q; want ready", resp["schema"])
	}
}

// TestReadyz_CHFailure: CH ping returns an error → 503 with the error
// reason embedded in the clickhouse field.
func TestReadyz_CHFailure(t *testing.T) {
	pinger := &stubPinger{err: errors.New("connection refused")}
	h := New(Options{
		Pinger:   pinger,
		CacheTTL: -1,
	})

	rec := serveReadyz(t, h)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if !strings.Contains(resp["clickhouse"], "connection refused") {
		t.Errorf("clickhouse = %q; want substring 'connection refused'", resp["clickhouse"])
	}
}

// TestReadyz_SchemaPending: CH ok but schema not finished bootstrapping.
func TestReadyz_SchemaPending(t *testing.T) {
	h := New(Options{
		Pinger:      &stubPinger{},
		SchemaReady: func() bool { return false },
		CacheTTL:    -1,
	})

	rec := serveReadyz(t, h)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if resp["clickhouse"] != "ok" {
		t.Errorf("clickhouse = %q; want ok", resp["clickhouse"])
	}
	if resp["schema"] != "pending" {
		t.Errorf("schema = %q; want pending", resp["schema"])
	}
}

// TestReadyz_NilPinger: defensive — if startup forgot the client, fail
// closed (503), don't false-positive.
func TestReadyz_NilPinger(t *testing.T) {
	h := New(Options{CacheTTL: -1})
	rec := serveReadyz(t, h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
}

// TestReadyz_TTLCaches verifies repeated probes inside the cache window
// don't trigger fresh Ping() calls, and that a forward clock advance
// past the TTL re-runs the check.
func TestReadyz_TTLCaches(t *testing.T) {
	pinger := &stubPinger{}
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }

	h := New(Options{
		Pinger:   pinger,
		CacheTTL: 2 * time.Second,
		Now:      clock,
	})

	// First probe: warms the cache.
	rec := serveReadyz(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe 1 status = %d; want 200", rec.Code)
	}
	if got := pinger.calls.Load(); got != 1 {
		t.Fatalf("ping calls after probe 1 = %d; want 1", got)
	}

	// Advance 1s — inside the TTL window: cached, no new ping.
	now = now.Add(time.Second)
	rec = serveReadyz(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("probe 2 status = %d; want 200", rec.Code)
	}
	if got := pinger.calls.Load(); got != 1 {
		t.Errorf("ping calls after probe 2 = %d; want 1 (cached)", got)
	}

	// Switch the upstream to failing — cached response still wins.
	pinger.setErr(errors.New("boom"))
	rec = serveReadyz(t, h)
	if rec.Code != http.StatusOK {
		t.Errorf("probe 3 status = %d; want 200 (still cached)", rec.Code)
	}

	// Advance past the TTL — fresh probe, surfaces the failure.
	now = now.Add(3 * time.Second)
	rec = serveReadyz(t, h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("probe 4 status = %d; want 503 (cache expired)", rec.Code)
	}
	if got := pinger.calls.Load(); got != 2 {
		t.Errorf("ping calls after probe 4 = %d; want 2 (one cached, one fresh)", got)
	}
}

// slowPinger blocks until released so the timeout test deterministically
// triggers context deadline.
type slowPinger struct {
	release chan struct{}
}

func (s *slowPinger) Ping(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return nil
	}
}

// TestReadyz_PingTimeout: the configured PingTimeout caps how long the
// probe waits on a slow CH.
func TestReadyz_PingTimeout(t *testing.T) {
	pinger := &slowPinger{release: make(chan struct{})}
	t.Cleanup(func() { close(pinger.release) })

	h := New(Options{
		Pinger:      pinger,
		PingTimeout: 20 * time.Millisecond,
		CacheTTL:    -1,
	})

	start := time.Now()
	rec := serveReadyz(t, h)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
	if elapsed > time.Second {
		t.Errorf("elapsed = %s; expected to honor 20ms timeout", elapsed)
	}
}

// TestMount_RoutesRegistered confirms both endpoints land on the mux.
func TestMount_RoutesRegistered(t *testing.T) {
	h := New(Options{Pinger: &stubPinger{}, CacheTTL: -1})
	mux := http.NewServeMux()
	h.Mount(mux)

	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s: route not registered (got 404)", path)
		}
	}
}

func serveReadyz(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.Mount(mux)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

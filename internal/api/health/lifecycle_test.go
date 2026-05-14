package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestReadyz_OKBodyShape pins the JSON shape /readyz returns on the
// happy path. Grafana and load balancers rely on the {clickhouse,schema}
// keys.
func TestReadyz_OKBodyShape(t *testing.T) {
	h := New(Options{
		Pinger:      &stubPinger{},
		SchemaReady: func() bool { return true },
		CacheTTL:    -1,
	})

	rec := serveReadyz(t, h)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Exactly two keys; no extra fields that would surprise dashboards.
	if len(body) != 2 {
		t.Errorf("body keys = %v; want exactly clickhouse + schema", body)
	}
	if body["clickhouse"] != "ok" || body["schema"] != "ready" {
		t.Errorf("body = %v; want {clickhouse:ok, schema:ready}", body)
	}
}

// TestReadyz_CHDownSurfacesPingTimeout: a pinger that returns immediately
// with an error reaches the 503 response within the (CacheTTL +
// PingTimeout) envelope. Deterministic without a clock injection: the
// stub does not block.
func TestReadyz_CHDownSurfacesPingTimeout(t *testing.T) {
	pinger := &stubPinger{err: errors.New("dial tcp: i/o timeout")}
	h := New(Options{
		Pinger:      pinger,
		PingTimeout: 50 * time.Millisecond,
		CacheTTL:    -1,
	})

	start := time.Now()
	rec := serveReadyz(t, h)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v; expected sub-second 503 from a synchronous failure", elapsed)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(body["clickhouse"], "i/o timeout") {
		t.Errorf("clickhouse = %q; want substring 'i/o timeout'", body["clickhouse"])
	}
}

// TestReadyz_SchemaPendingBodyShape covers the mixed state: CH up, schema
// still bootstrapping. Body must report "ok"/"pending" with 503 — Grafana
// surfaces "schema not ready" rather than a bogus "CH down" alert.
func TestReadyz_SchemaPendingBodyShape(t *testing.T) {
	h := New(Options{
		Pinger:      &stubPinger{},
		SchemaReady: func() bool { return false },
		CacheTTL:    -1,
	})
	rec := serveReadyz(t, h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["clickhouse"] != "ok" {
		t.Errorf("clickhouse = %q; want ok", body["clickhouse"])
	}
	if body["schema"] != "pending" {
		t.Errorf("schema = %q; want pending (auto-create still running)", body["schema"])
	}
}

// TestReadyz_NilSchemaReadyTreatedAsReady documents the documented
// fallback: when SchemaReady is nil the probe ignores it and only gates
// on the CH ping. Matches main.go's wiring when AutoCreateSchema is
// false.
func TestReadyz_NilSchemaReadyTreatedAsReady(t *testing.T) {
	h := New(Options{
		Pinger:      &stubPinger{},
		SchemaReady: nil, // intentionally unset
		CacheTTL:    -1,
	})
	rec := serveReadyz(t, h)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 when SchemaReady is nil", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["schema"] != "ready" {
		t.Errorf("schema = %q; want ready", body["schema"])
	}
}

// TestReadyz_CacheCoalesces_ConcurrentProbes hammers /readyz from N
// goroutines inside a single TTL window and confirms exactly one
// underlying CH ping is issued. The mutex around the cache + ping path
// is what makes this hold — without it the cache would still coalesce
// the *result* but multiple pings could race in.
func TestReadyz_CacheCoalesces_ConcurrentProbes(t *testing.T) {
	pinger := &stubPinger{}
	now := time.Unix(1_700_000_100, 0)
	clock := func() time.Time { return now }

	h := New(Options{
		Pinger:   pinger,
		CacheTTL: time.Second,
		Now:      clock,
	})

	const N = 32
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = serveReadyz(t, h)
		}()
	}
	wg.Wait()

	if got := pinger.calls.Load(); got != 1 {
		t.Errorf("Ping calls = %d; want exactly 1 (cache must coalesce concurrent probes)", got)
	}
}

// TestReadyz_CacheInvalidatesAfterTTL: after the TTL elapses, a probe
// triggers a fresh CH ping. Combined with the previous test this pins
// the cache-window semantics.
func TestReadyz_CacheInvalidatesAfterTTL(t *testing.T) {
	pinger := &stubPinger{}
	now := time.Unix(1_700_000_200, 0)
	clock := func() time.Time { return now }

	h := New(Options{
		Pinger:   pinger,
		CacheTTL: 100 * time.Millisecond,
		Now:      clock,
	})

	_ = serveReadyz(t, h)
	if got := pinger.calls.Load(); got != 1 {
		t.Fatalf("initial ping count = %d; want 1", got)
	}
	now = now.Add(150 * time.Millisecond)
	_ = serveReadyz(t, h)
	if got := pinger.calls.Load(); got != 2 {
		t.Errorf("ping count after TTL expiry = %d; want 2 (cache evicted)", got)
	}
}

// TestHealthz_NeverTouchesCH proves the liveness probe is downstream-
// agnostic: a CH that always errors must not produce a non-200 here.
func TestHealthz_NeverTouchesCH(t *testing.T) {
	pinger := &stubPinger{err: errors.New("permanent failure")}
	h := New(Options{Pinger: pinger})

	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200 even with CH down", rec.Code)
	}
	if got := pinger.calls.Load(); got != 0 {
		t.Errorf("Ping calls = %d; want 0 (healthz must not touch CH)", got)
	}
}

// TestHealthz_BodyOK pins the exact response body "ok". Some uptime
// probes assert on it.
func TestHealthz_BodyOK(t *testing.T) {
	h := New(Options{Pinger: &stubPinger{}})
	mux := http.NewServeMux()
	h.Mount(mux)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("body = %q; want ok", got)
	}
}

// TestProbes_Concurrent_NoPanics fires /healthz and /readyz from many
// goroutines simultaneously, confirming nothing panics, racey, or
// deadlocks. Stress test for the cache mutex + the response writer paths.
func TestProbes_Concurrent_NoPanics(t *testing.T) {
	pinger := &stubPinger{}
	h := New(Options{Pinger: pinger, CacheTTL: 10 * time.Millisecond})

	mux := http.NewServeMux()
	h.Mount(mux)

	const N = 200
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
		}()
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
		}()
	}
	wg.Wait()
}

// TestReadyz_PingTimeoutDefaults documents the default 1-second
// PingTimeout when the option is left zero — protects against a future
// change that silently drops the safety net.
func TestReadyz_PingTimeoutDefaults(t *testing.T) {
	h := New(Options{Pinger: &stubPinger{}, CacheTTL: -1})
	if h.pingTimeout != time.Second {
		t.Errorf("pingTimeout default = %v; want 1s", h.pingTimeout)
	}
}

// TestReadyz_CacheTTLDefaults documents the default 2-second CacheTTL
// when the option is left zero.
func TestReadyz_CacheTTLDefaults(t *testing.T) {
	h := New(Options{Pinger: &stubPinger{}})
	if h.cacheTTL != 2*time.Second {
		t.Errorf("cacheTTL default = %v; want 2s", h.cacheTTL)
	}
}

// TestReadyz_NegativeCacheTTLDisablesCaching: a probe -> error -> probe
// sequence must surface the recovered state on the third probe when
// caching is disabled. Anchors the "tests can opt out of coalescing"
// contract.
func TestReadyz_NegativeCacheTTLDisablesCaching(t *testing.T) {
	pinger := &stubPinger{err: errors.New("first")}
	h := New(Options{Pinger: pinger, CacheTTL: -1})

	rec := serveReadyz(t, h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first probe: status = %d; want 503", rec.Code)
	}
	pinger.setErr(nil)
	rec = serveReadyz(t, h)
	if rec.Code != http.StatusOK {
		t.Errorf("second probe: status = %d; want 200 (cache disabled)", rec.Code)
	}
}

// TestReadyz_ContextCancelledPropagates: a cancelled request context
// must propagate into the ping call so callers can shed load fast.
type cancelObservingPinger struct{}

func (p *cancelObservingPinger) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func TestReadyz_ContextCancelledPropagates(t *testing.T) {
	pinger := &cancelObservingPinger{}
	h := New(Options{
		Pinger:   pinger,
		CacheTTL: -1,
	})

	mux := http.NewServeMux()
	h.Mount(mux)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d; want 503 when request context was cancelled", rec.Code)
	}
}

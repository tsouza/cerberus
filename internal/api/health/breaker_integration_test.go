package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// Layer 11 — /readyz integration with the chclient circuit breaker.
//
// The readiness probe consumes Pinger, which the chclient.Client
// satisfies. When the breaker is OPEN, chclient.Client.Ping returns
// ErrCircuitOpen instantly without dialling — so /readyz reports red
// within the cache TTL of the trip, not after a full CH dial timeout.
//
// We exercise that contract here through a stub Pinger that emulates
// the chclient behaviour (immediate ErrCircuitOpen) without needing
// a live ClickHouse or the full chclient.Client.

// breakerPinger is a Pinger that returns ErrCircuitOpen instantly.
// Models the contract chclient.Client.Ping honours when its embedded
// breaker is OPEN.
type breakerPinger struct {
	open bool
}

func (p *breakerPinger) Ping(_ context.Context) error {
	if p.open {
		return errors.New("chclient: ping: " + chclient.ErrCircuitOpen.Error())
	}
	return nil
}

// TestReadyz_ReturnsRedWhenBreakerOpen — pins /readyz behavior when
// the chclient breaker is OPEN. Three guarantees:
//
//  1. Status is 503 (the upstream-dependency is unreachable).
//  2. Response surfaces within milliseconds (no dial timeout), and
//     critically faster than any reasonable CH dial.
//  3. The clickhouse field embeds the breaker-open signal so on-call
//     can identify the cause from a single /readyz dump.
func TestReadyz_ReturnsRedWhenBreakerOpen(t *testing.T) {
	t.Parallel()
	pinger := &breakerPinger{open: true}
	h := New(Options{
		Pinger:   pinger,
		CacheTTL: -1, // disable cache for deterministic per-call behavior
	})

	start := time.Now()
	rec := serveReadyz(t, h)
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 when breaker is open", rec.Code)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("elapsed = %s; want fast-fail under 100ms (no dial timeout)", elapsed)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body["clickhouse"], "circuit") {
		t.Errorf("clickhouse field = %q; want substring 'circuit' so on-call sees breaker-open signal",
			body["clickhouse"])
	}
}

// TestReadyz_RecoversWhenBreakerCloses — once the breaker transitions
// back to CLOSED (CH recovers), /readyz must flip green again. We
// model the transition by flipping the pinger's open flag.
func TestReadyz_RecoversWhenBreakerCloses(t *testing.T) {
	t.Parallel()
	pinger := &breakerPinger{open: true}
	h := New(Options{
		Pinger:      pinger,
		SchemaReady: func() bool { return true },
		CacheTTL:    -1,
	})

	rec := serveReadyz(t, h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status before recovery = %d; want 503", rec.Code)
	}

	// CH recovers (probe success closed the breaker).
	pinger.open = false

	rec = serveReadyz(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status after recovery = %d; want 200", rec.Code)
	}
}

// TestHealthz_IndependentOfBreaker — /healthz is process-only liveness;
// the breaker's OPEN state must NOT affect it. This guards against a
// future regression that tries to "fix" /healthz to probe CH — which
// would cause Kubernetes to restart pods during a CH outage, taking
// down all replicas at once.
func TestHealthz_IndependentOfBreaker(t *testing.T) {
	t.Parallel()
	pinger := &breakerPinger{open: true}
	h := New(Options{
		Pinger:   pinger,
		CacheTTL: -1,
	})
	mux := http.NewServeMux()
	h.Mount(mux)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d; want 200 (liveness must not depend on CH)", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("/healthz body = %q; want %q", got, "ok")
	}
}

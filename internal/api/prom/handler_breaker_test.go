package prom_test

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// Layer 11 — handler-level wire contract for the chclient circuit
// breaker. When the breaker is OPEN, chclient methods return
// ErrCircuitOpen; the Prom handler must translate that into HTTP 503
// with a `Retry-After: 5` header so Grafana / Prometheus clients back
// off instead of hammering the gateway during a CH outage.

// TestHandler_BreakerOpenReturns503WithRetryAfter — the canonical
// breaker → wire test. We use a stub querier whose err is the
// breaker sentinel, then assert the handler emits 503 + Retry-After: 5
// + the `unavailable` Prom error type.
func TestHandler_BreakerOpenReturns503WithRetryAfter(t *testing.T) {
	t.Parallel()

	// Wrap the sentinel through fmt.Errorf to mirror the production
	// shape — chclient methods return `chclient: query: %w` with
	// ErrCircuitOpen as the wrapped target. The handler's errors.Is
	// check should still recover the sentinel.
	wrapped := fmt.Errorf("chclient: query: %w", chclient.ErrCircuitOpen)

	q := &stubQuerier{err: wrapped}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up&time=2026-05-14T12:00:00Z")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503\nbody=%s", resp.StatusCode, string(body))
	}
	if got := resp.Header.Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q; want %q", got, "5")
	}
	// The envelope must signal the right errorType so dashboards
	// pivoting on it surface the breaker-open state separately
	// from other 5xx classes.
	if !strings.Contains(string(body), "\"errorType\":\"unavailable\"") {
		t.Errorf("body missing errorType:unavailable — got %s", string(body))
	}
}

// TestHandler_BreakerOpenReturns503OnRangeQuery — same contract on the
// /api/v1/query_range endpoint. Grafana hits this with every panel
// refresh, so the breaker-open wire shape matters here too.
func TestHandler_BreakerOpenReturns503OnRangeQuery(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("chclient: query: %w", chclient.ErrCircuitOpen)
	q := &stubQuerier{err: wrapped}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	v := url.Values{}
	v.Set("query", "up")
	v.Set("start", "2026-05-14T12:00:00Z")
	v.Set("end", "2026-05-14T12:05:00Z")
	v.Set("step", "30s")
	resp, err := http.Get(srv.URL + "/api/v1/query_range?" + v.Encode())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q; want 5", got)
	}
}

// TestHandler_BreakerOpenReturns503OnLabels — metadata endpoints
// (/api/v1/labels, /api/v1/label/{name}/values, /api/v1/series) all
// touch CH via Client.QueryStrings / QueryLabelSets; they must also
// surface 503 + Retry-After when the breaker is open.
func TestHandler_BreakerOpenReturns503OnLabels(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("chclient: query: %w", chclient.ErrCircuitOpen)
	q := &stubQuerier{err: wrapped}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/labels")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q; want 5", got)
	}
}

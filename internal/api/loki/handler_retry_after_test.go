package loki_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// Layer 11 — the Loki head sizes a breaker-open 503's `Retry-After` from the
// tripped breaker's own recovery interval. Per-head breakers are independently
// tunable, so the Loki head must advertise the LOKI breaker's interval rather
// than a shared literal.

// TestHandler_RetryAfterTracksBreakerInterval asserts the derived header on the
// query endpoints Grafana refreshes.
func TestHandler_RetryAfterTracksBreakerInterval(t *testing.T) {
	t.Parallel()

	rangeQuery := url.Values{}
	rangeQuery.Set("query", `{job="api"}`)
	rangeQuery.Set("start", "1747051200000000000")
	rangeQuery.Set("end", "1747054800000000000")

	for _, path := range []string{
		`/loki/api/v1/query?query=` + url.QueryEscape(`{job="api"}`),
		"/loki/api/v1/query_range?" + rangeQuery.Encode(),
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			// 45s is deliberately NOT the package default, so a handler that
			// still writes a fixed value cannot pass by coincidence.
			open := chclient.NewCircuitOpenError("chclient: query", 45*time.Second)
			srv := newServer(&stubQuerier{err: fmt.Errorf("engine: execute: %w", open)})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status = %d; want 503", resp.StatusCode)
			}
			if got := resp.Header.Get("Retry-After"); got != "45" {
				t.Errorf("Retry-After = %q; want 45 (the breaker's own interval)", got)
			}
		})
	}
}

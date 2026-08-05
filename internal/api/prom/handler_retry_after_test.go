package prom_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// Layer 11 — the `Retry-After` a breaker-open 503 advertises must be the
// tripped breaker's OWN recovery interval. An operator who widens
// CERBERUS_CH_BREAKER_OPEN_INTERVAL to protect a fragile ClickHouse gets
// clients that return while cerberus is still fast-failing if the header stays
// on a fixed number, which is precisely the synchronised retry storm the
// breaker exists to prevent.

// breakerOpenAfter builds the error shape a tripped breaker returns: the typed
// carrier wrapping the sentinel, stamped with the breaker's interval, seen by
// the handler through the engine's stage wrap.
func breakerOpenAfter(d time.Duration) error {
	return fmt.Errorf("engine: execute: %w", chclient.NewCircuitOpenError("chclient: query", d))
}

// TestHandler_RetryAfterTracksBreakerInterval — every endpoint that can 503 on
// an open breaker sizes the header from the interval on the error, not from a
// literal.
func TestHandler_RetryAfterTracksBreakerInterval(t *testing.T) {
	t.Parallel()

	rangeQuery := url.Values{}
	rangeQuery.Set("query", "up")
	rangeQuery.Set("start", "2026-05-14T12:00:00Z")
	rangeQuery.Set("end", "2026-05-14T12:05:00Z")
	rangeQuery.Set("step", "30s")

	for _, path := range []string{
		"/api/v1/query?query=up&time=2026-05-14T12:00:00Z",
		"/api/v1/query_range?" + rangeQuery.Encode(),
		"/api/v1/labels",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			// 45s is deliberately NOT the package default, so a handler that
			// still writes a fixed value cannot pass by coincidence.
			srv := newServer(&stubQuerier{err: breakerOpenAfter(45 * time.Second)})
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

// TestHandler_RetryAfterRoundsSubSecondIntervalUp — a breaker tuned below a
// second still advertises a back-off, never an immediate retry.
func TestHandler_RetryAfterRoundsSubSecondIntervalUp(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{err: breakerOpenAfter(250 * time.Millisecond)})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up&time=2026-05-14T12:00:00Z")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q; want 1", got)
	}
}

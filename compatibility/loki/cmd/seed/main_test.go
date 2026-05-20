package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestWaitLokiIndexSettle_StickyAbsorbsTransientRegression simulates the
// chunk-flush → BoltDB-persist window where /labels and /series each
// briefly return the full cardinality (ingester in-memory chunks), then
// drop to empty (chunks flushed, BoltDB shipper hasn't yet persisted
// the freshly-flushed index files). Run 26132714829 hit exactly this
// shape on the real harness.
//
// The two endpoints latch at *different ticks* — /labels is full on
// tick 1 then empty thereafter; /series is empty on tick 1 then full on
// tick 2 then empty thereafter. The two threshold-crossings never
// overlap in a single tick. The pre-sticky gate (which required both
// "now" readings to satisfy the threshold concurrently) would time
// out; the sticky gate latches each side independently and returns
// nil on tick 2 after both latches have flipped.
func TestWaitLokiIndexSettle_StickyAbsorbsTransientRegression(t *testing.T) {
	t.Parallel()

	// streams here mirrors a slimmed-down version of the seed: 2 streams
	// each carrying the same single label key. expectedLabelKeys returns
	// ["k"]; len(streams) == 2.
	streams := []stream{
		{labels: map[string]string{"k": "a"}},
		{labels: map[string]string{"k": "b"}},
	}

	var labelPolls, seriesPolls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/loki/api/v1/labels"):
			n := labelPolls.Add(1)
			// Full only on the first poll; empty on every poll after.
			var data []string
			if n == 1 {
				data = []string{"k"}
			} else {
				data = []string{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   data,
			})
		case strings.HasPrefix(r.URL.Path, "/loki/api/v1/series"):
			n := seriesPolls.Add(1)
			// Empty on the first poll, full only on the second, empty
			// on every poll after — the threshold-crossings for the
			// two endpoints never overlap in a single tick, so the
			// pre-sticky gate would time out here.
			var data []map[string]string
			switch {
			case n == 2:
				data = []map[string]string{
					{"k": "a"},
					{"k": "b"},
				}
			default:
				data = []map[string]string{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   data,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	// Use a context with a reasonable bound so a hung test fails fast.
	// The function's internal 90s deadline is the real upper bound; we
	// only need a few ticks to exercise the latch behaviour.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	start := time.Unix(0, 0)
	end := start.Add(1 * time.Hour)

	if err := waitLokiIndexSettle(ctx, srv.URL, streams, start, end, logger); err != nil {
		t.Fatalf("waitLokiIndexSettle returned error despite latched success: %v", err)
	}
}

// TestWaitLokiIndexSettle_NeverLatchedTimesOut confirms the timeout
// path still fires when neither side ever crosses the threshold — the
// stickiness must not turn the gate into a no-op for the truly-broken
// case where Loki never indexes the seed.
func TestWaitLokiIndexSettle_NeverLatchedTimesOut(t *testing.T) {
	t.Parallel()

	streams := []stream{
		{labels: map[string]string{"k": "a"}},
		{labels: map[string]string{"k": "b"}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/loki/api/v1/labels"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   []string{},
			})
		case strings.HasPrefix(r.URL.Path, "/loki/api/v1/series"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   []map[string]string{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	// Use a short context to cut the test off well before the 90s
	// internal deadline — we only need to confirm "no latch → no
	// success", not exercise the full timeout path.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	start := time.Unix(0, 0)
	end := start.Add(1 * time.Hour)

	err := waitLokiIndexSettle(ctx, srv.URL, streams, start, end, logger)
	if err == nil {
		t.Fatalf("waitLokiIndexSettle returned nil when neither side latched")
	}
}

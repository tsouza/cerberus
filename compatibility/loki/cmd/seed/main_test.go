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

// TestWaitLokiIndexSettle pins the contract of the settle gate so future
// threshold + latch regressions surface at PR review time, not on the
// real harness. The function has been bumped three times for cadence /
// threshold without ever having a unit pin:
//
//   - PR #66 grew the budget from 30s → 60s after a one-stream lag.
//   - PR #123 raised it again to 90s for the same tail.
//   - PR #608 relaxed the series threshold from "all N" to ceil(0.9*N)
//     to absorb the one-stream-still-lagging shape seen on cold runners.
//
// Each fix went through review on logic alone. This test pins:
//   - the 90% series threshold (12/13 passes, 11/13 fails);
//   - the AND of (labels_latched, series_latched) gating return-nil;
//   - the high-water-mark latch surviving a regression on either side;
//   - the timeout error shape carrying enough diagnostics to root-cause
//     a stuck CI run from the log line alone.
func TestWaitLokiIndexSettle(t *testing.T) {
	// No t.Parallel(): this test mutates the package-level settle*
	// cadence vars. Running in parallel with another test in this
	// package that read the same vars would race.

	// Shrink production cadence: 1s poll × ~few ticks would still take
	// several seconds per timeout case. Tests need to fail fast, so we
	// override the package-level vars to a 10ms / 5ms / 100ms shape for
	// every case inside this top-level test. Restore on exit so other
	// tests (and `go test -count=N`) see the production defaults.
	origTimeout, origInterval, origProgressAt := settleTimeout, settleInterval, settleProgressAt
	settleTimeout = 500 * time.Millisecond
	settleInterval = 5 * time.Millisecond
	settleProgressAt = 100 * time.Millisecond
	t.Cleanup(func() {
		settleTimeout = origTimeout
		settleInterval = origInterval
		settleProgressAt = origProgressAt
	})

	// fixtureStreams mirrors the production seed shape: 13 streams, each
	// carrying the same 9 resource label keys. That makes the series
	// threshold ceil(0.9 * 13) = 12 — the exact value the gate uses in
	// the prod call. Anyone touching expectedLabelKeys or the threshold
	// formula will trip the case-2 / case-3 boundary first.
	fixtureStreams := func() []stream {
		out := make([]stream, 0, 13)
		for i := 0; i < 13; i++ {
			out = append(out, stream{
				labels: map[string]string{
					"cluster":      "c",
					"namespace":    "n",
					"service":      "s",
					"service_name": "sn",
					"pod":          "p",
					"container":    "k",
					"env":          "e",
					"region":       "r",
					"datacenter":   "d",
				},
			})
		}
		return out
	}

	allLabels := []string{
		"cluster", "namespace", "service", "service_name",
		"pod", "container", "env", "region", "datacenter",
	}

	// seriesSet returns n distinct label sets — used to drive the
	// /series response cardinality.
	seriesSet := func(n int) []map[string]string {
		out := make([]map[string]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, map[string]string{
				"service_name": "sn-" + string(rune('a'+i)),
			})
		}
		return out
	}

	encodeLabels := func(w http.ResponseWriter, data []string) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   data,
		})
	}
	encodeSeries := func(w http.ResponseWriter, data []map[string]string) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   data,
		})
	}

	// runSettle wires up the fake server + a no-op logger + a context
	// bounded slightly above settleTimeout so a hung gate is the test
	// failure, not a hung process.
	runSettle := func(t *testing.T, handler http.HandlerFunc, streams []stream) error {
		t.Helper()
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		t.Cleanup(cancel)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		start := time.Unix(0, 0)
		end := start.Add(time.Hour)
		return waitLokiIndexSettle(ctx, srv.URL, streams, start, end, logger)
	}

	t.Run("all_N_ready_immediately", func(t *testing.T) {
		// Happy path: the server returns the full label set + full
		// series count from the first poll. The gate latches both on
		// tick 1 and returns nil. This is the steady-state shape — if
		// it ever fails, something is wrong with the threshold
		// arithmetic or label-key expectation logic.
		streams := fixtureStreams()
		handler := func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/loki/api/v1/labels"):
				encodeLabels(w, allLabels)
			case strings.HasPrefix(r.URL.Path, "/loki/api/v1/series"):
				encodeSeries(w, seriesSet(13))
			default:
				http.NotFound(w, r)
			}
		}
		if err := runSettle(t, handler, streams); err != nil {
			t.Fatalf("expected nil error for fully-ready server, got: %v", err)
		}
	})

	t.Run("12_of_13_stable_passes", func(t *testing.T) {
		// The PR #608 case: one stream consistently lags the
		// ingester→TSDB-index flush. Labels are full (9/9) and series
		// hold steady at 12/13. ceil(0.9 * 13) = 12, so the threshold
		// is exactly satisfied and the gate must return nil. If any
		// future tightening pushes the threshold back toward "all N",
		// this case is the trip-wire.
		streams := fixtureStreams()
		handler := func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/loki/api/v1/labels"):
				encodeLabels(w, allLabels)
			case strings.HasPrefix(r.URL.Path, "/loki/api/v1/series"):
				encodeSeries(w, seriesSet(12))
			default:
				http.NotFound(w, r)
			}
		}
		if err := runSettle(t, handler, streams); err != nil {
			t.Fatalf("expected nil error at the 90%% boundary (12/13), got: %v", err)
		}
	})

	t.Run("11_of_13_stable_times_out", func(t *testing.T) {
		// One below the 90% boundary. Labels are full, but only 11
		// streams ever surface. The series latch never flips, the
		// AND-gate never closes, and the deadline expires. If a
		// future change drops the threshold below ceil(0.9*N), this
		// case starts passing and the test fails — exactly what we
		// want.
		streams := fixtureStreams()
		handler := func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/loki/api/v1/labels"):
				encodeLabels(w, allLabels)
			case strings.HasPrefix(r.URL.Path, "/loki/api/v1/series"):
				encodeSeries(w, seriesSet(11))
			default:
				http.NotFound(w, r)
			}
		}
		err := runSettle(t, handler, streams)
		if err == nil {
			t.Fatalf("expected timeout at 11/13 (below 90%% threshold), got nil")
		}
		// Sanity-check the error shape: "series_now=11/13" should
		// appear so on-call can read the gap straight off the log.
		if !strings.Contains(err.Error(), "series_now=11/13") {
			t.Fatalf("expected timeout error to mention 'series_now=11/13', got: %v", err)
		}
	})

	t.Run("latched_then_regressed_holds", func(t *testing.T) {
		// The latch-rationale case (run 26132714829 shape): the
		// server returns the full set on the first poll, then drops
		// to a partial set on every poll after. The high-water-mark
		// latch must hold — once both latches have flipped, the gate
		// succeeds regardless of subsequent regressions.
		streams := fixtureStreams()
		var labelPolls, seriesPolls atomic.Int32
		handler := func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/loki/api/v1/labels"):
				n := labelPolls.Add(1)
				if n == 1 {
					encodeLabels(w, allLabels)
					return
				}
				// Drop to a partial set (5 of 9 keys).
				encodeLabels(w, allLabels[:5])
			case strings.HasPrefix(r.URL.Path, "/loki/api/v1/series"):
				n := seriesPolls.Add(1)
				if n == 1 {
					encodeSeries(w, seriesSet(13))
					return
				}
				// Drop well below the 12-threshold.
				encodeSeries(w, seriesSet(7))
			default:
				http.NotFound(w, r)
			}
		}
		if err := runSettle(t, handler, streams); err != nil {
			t.Fatalf("expected latch to hold across regression, got: %v", err)
		}
	})

	t.Run("both_empty_entire_window_times_out", func(t *testing.T) {
		// Worst-case shape: Loki never indexes anything. Both
		// endpoints return empty forever. The gate must time out
		// with a diagnostic error string that pinpoints the failure
		// mode — operators rely on the labels_latched / series_latched
		// + labels_now/series_now counters to root-cause a stuck
		// harness from a single log line.
		streams := fixtureStreams()
		handler := func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasPrefix(r.URL.Path, "/loki/api/v1/labels"):
				encodeLabels(w, []string{})
			case strings.HasPrefix(r.URL.Path, "/loki/api/v1/series"):
				encodeSeries(w, []map[string]string{})
			default:
				http.NotFound(w, r)
			}
		}
		err := runSettle(t, handler, streams)
		if err == nil {
			t.Fatalf("expected timeout when both sides stay empty, got nil")
		}
		for _, want := range []string{
			"labels_latched=false",
			"series_latched=false",
			"labels_now=0/9",
			"series_now=0/13",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("timeout error missing diagnostic %q in: %v", want, err)
			}
		}
	})
}

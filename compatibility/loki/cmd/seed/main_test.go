package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestWaitLokiFlushDrained pins the contract of the metric-based drain
// gate the corpus stage runs before the reference Loki restart. It reads
// Loki's own `/metrics` endpoint, which exposes the authoritative
// signals (`loki_ingester_flush_queue_length`,
// `loki_ingester_memory_chunks`, `loki_ingester_chunks_flushed_total`).
//
// The cases below pin:
//   - the AND of all three conditions gating return-nil;
//   - the delta accounting against the pre-push baseline;
//   - a structured error when a required metric is missing (upstream
//     rename detection);
//   - the timeout error shape — every counter's value end-to-end so
//     on-call can root-cause from a single log line.
func TestWaitLokiFlushDrained(t *testing.T) {
	// No t.Parallel(): this test mutates the package-level drain*
	// cadence vars.
	origTimeout, origInterval, origProgressAt := drainTimeout, drainInterval, drainProgressAt
	drainTimeout = 500 * time.Millisecond
	drainInterval = 5 * time.Millisecond
	drainProgressAt = 100 * time.Millisecond
	t.Cleanup(func() {
		drainTimeout = origTimeout
		drainInterval = origInterval
		drainProgressAt = origProgressAt
	})

	const expectedStreams = 13

	// buildMetricsBody returns a `/metrics` text-exposition payload with
	// the three signals the gate keys on. Other metrics are intentionally
	// omitted — the gate must not depend on anything outside its
	// declared contract.
	buildMetricsBody := func(flushQueue, memChunks, chunksFlushed float64) string {
		return fmt.Sprintf(`# HELP loki_ingester_flush_queue_length unused
# TYPE loki_ingester_flush_queue_length gauge
loki_ingester_flush_queue_length %g
# HELP loki_ingester_memory_chunks unused
# TYPE loki_ingester_memory_chunks gauge
loki_ingester_memory_chunks %g
# HELP loki_ingester_chunks_flushed_total unused
# TYPE loki_ingester_chunks_flushed_total counter
loki_ingester_chunks_flushed_total{reason="forced"} %g
`, flushQueue, memChunks, chunksFlushed)
	}

	serveMetrics := func(body func() string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/metrics" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body()))
		}
	}

	runDrain := func(t *testing.T, handler http.HandlerFunc, baseline lokiMetricsSnapshot) error {
		t.Helper()
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		t.Cleanup(cancel)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		return waitLokiFlushDrained(ctx, srv.URL, expectedStreams, baseline, logger)
	}

	t.Run("all_signals_ready_immediately", func(t *testing.T) {
		// Happy path: queue drained, memory empty, counter past
		// baseline. Gate returns on tick 1.
		body := buildMetricsBody(0, 0, expectedStreams)
		if err := runDrain(t, serveMetrics(func() string { return body }), lokiMetricsSnapshot{}); err != nil {
			t.Fatalf("expected nil error for fully-drained server, got: %v", err)
		}
	})

	t.Run("flush_queue_drains_partway_through", func(t *testing.T) {
		// First poll: queue non-zero. Second poll: queue at zero with
		// all other conditions met. The gate must wait for the queue
		// to drain before returning.
		var polls atomic.Int32
		handler := serveMetrics(func() string {
			if polls.Add(1) == 1 {
				return buildMetricsBody(5, 2, 8)
			}
			return buildMetricsBody(0, 0, expectedStreams)
		})
		if err := runDrain(t, handler, lokiMetricsSnapshot{}); err != nil {
			t.Fatalf("expected nil error after queue drains, got: %v", err)
		}
		if got := polls.Load(); got < 2 {
			t.Fatalf("expected at least 2 polls, got %d", got)
		}
	})

	t.Run("memory_chunks_must_reach_zero", func(t *testing.T) {
		// Queue drained and the counter past baseline, but chunks are
		// still held in memory: a flush that started but has not
		// finished. The gate must not return.
		body := buildMetricsBody(0, 1, expectedStreams)
		err := runDrain(t, serveMetrics(func() string { return body }), lokiMetricsSnapshot{})
		if err == nil {
			t.Fatalf("expected timeout while memory_chunks > 0, got nil")
		}
		if !contains(err.Error(), "memory_chunks=1") {
			t.Fatalf("timeout error missing memory_chunks diagnostic in: %v", err)
		}
	})

	t.Run("delta_must_exceed_baseline", func(t *testing.T) {
		// The counter is above zero but matches the baseline exactly —
		// no work has happened since the baseline was taken. The gate
		// must not return.
		body := buildMetricsBody(0, 0, 50)
		err := runDrain(t, serveMetrics(func() string { return body }), lokiMetricsSnapshot{chunksFlushed: 50})
		if err == nil {
			t.Fatalf("expected timeout when the counter matches baseline, got nil")
		}
		for _, want := range []string{
			"chunks_flushed=50→50",
			"needed_delta=13",
		} {
			if !contains(err.Error(), want) {
				t.Fatalf("timeout error missing diagnostic %q in: %v", want, err)
			}
		}
	})

	t.Run("missing_required_metric_errors", func(t *testing.T) {
		// Loki upstream rename / regression: the `flush_queue_length`
		// gauge is missing entirely. The gate must surface this as a
		// structured error rather than treat the missing value as
		// zero (which would silently flip to "settled").
		handler := serveMetrics(func() string {
			return `# HELP loki_ingester_memory_chunks unused
# TYPE loki_ingester_memory_chunks gauge
loki_ingester_memory_chunks 0
# HELP loki_ingester_chunks_flushed_total unused
# TYPE loki_ingester_chunks_flushed_total counter
loki_ingester_chunks_flushed_total{reason="forced"} 13
`
		})
		err := runDrain(t, handler, lokiMetricsSnapshot{})
		if err == nil {
			t.Fatalf("expected error when required metric is missing, got nil")
		}
		if !contains(err.Error(), "loki_ingester_flush_queue_length") {
			t.Fatalf("expected error to name the missing metric, got: %v", err)
		}
		if !contains(err.Error(), "upstream rename") {
			t.Fatalf("expected error to mention upstream-rename rationale, got: %v", err)
		}
	})

	t.Run("metrics_endpoint_unreachable_times_out_with_last_err", func(t *testing.T) {
		// /metrics always 500s — gate times out and the error carries
		// the last underlying HTTP failure for diagnostics.
		handler := func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}
		err := runDrain(t, handler, lokiMetricsSnapshot{})
		if err == nil {
			t.Fatalf("expected timeout when /metrics is unreachable, got nil")
		}
		if !contains(err.Error(), "status 500") {
			t.Fatalf("expected timeout error to mention HTTP status, got: %v", err)
		}
	})
}

// TestParsePromMetrics pins the parser shape so a future Loki version
// emitting exemplars / a histogram extension doesn't silently break the
// drain gate.
func TestParsePromMetrics(t *testing.T) {
	t.Parallel()

	body := []byte(`# HELP foo unused
# TYPE foo counter
foo 1
foo{bar="baz"} 2

# blank line above must not break the parse
loki_ingester_flush_queue_length 0
loki_ingester_chunks_flushed_total{reason="forced"} 23
loki_ingester_chunks_flushed_total{reason="full"} 7
loki_tsdb_shipper_tables_upload_operation_total{component="x",status="success"} 5
malformed_no_value
loki_ingester_checkpoint_duration_seconds{quantile="0.5"} NaN
`)
	got := parsePromMetrics(body)

	wantPresent := []string{
		"foo",
		`foo{bar="baz"}`,
		"loki_ingester_flush_queue_length",
		`loki_ingester_chunks_flushed_total{reason="forced"}`,
		`loki_ingester_chunks_flushed_total{reason="full"}`,
		`loki_tsdb_shipper_tables_upload_operation_total{component="x",status="success"}`,
	}
	for _, k := range wantPresent {
		if _, ok := got[k]; !ok {
			t.Errorf("expected parser to surface %q, got keys: %v", k, mapKeys(got))
		}
	}
	if _, ok := got["malformed_no_value"]; ok {
		t.Errorf("expected malformed line to be dropped silently, but it parsed")
	}

	// sumMatching must collapse the family across `reason` labels.
	gotFlushed := sumMatching(got, "loki_ingester_chunks_flushed_total")
	if gotFlushed != 30 { // 23 + 7
		t.Errorf("sumMatching(chunks_flushed_total) = %v, want 30", gotFlushed)
	}

	// A family name must not match a longer name it is a prefix of.
	if gotFoo := sumMatching(got, "fo"); gotFoo != 0 {
		t.Errorf("sumMatching(fo) = %v, want 0 (no family named fo)", gotFoo)
	}
}

// TestExtractSnapshot pins the baseline-decoder contract.
func TestExtractSnapshot(t *testing.T) {
	t.Parallel()

	metrics := map[string]float64{
		`loki_ingester_chunks_flushed_total{reason="forced"}`: 11,
		`loki_ingester_chunks_flushed_total{reason="full"}`:   2,
		`loki_ingester_chunks_flushed_total_bogus`:            100,
		`loki_ingester_flush_queue_length`:                    0,
	}
	snap := extractSnapshot(metrics)
	if snap.chunksFlushed != 13 {
		t.Errorf("chunksFlushed = %v, want 13 (both reason rows, nothing else)", snap.chunksFlushed)
	}
}

// TestVerifyLabelsWindowIsAnchorRelative pins the wall-clock
// independence of the post-seed /labels probe window. The 2026-06-08
// nightly went red with zero commits on main because the window's end
// was `time.Now() + 24h`: the span grew with every passing day until
// it crossed Loki's default `max_query_length` (30d1h) and the
// reference Loki started rejecting the probe with status 400. The
// window must bracket the fixture span [anchor, anchor+24h] and must
// stay a fixed width regardless of when the harness runs.
func TestVerifyLabelsWindowIsAnchorRelative(t *testing.T) {
	t.Parallel()

	anchorTS, err := time.Parse(time.RFC3339, anchor)
	if err != nil {
		t.Fatalf("parse anchor: %v", err)
	}
	start, end := verifyLabelsWindow(anchorTS)

	if !start.Before(anchorTS) {
		t.Errorf("window start %v must precede the anchor %v", start, anchorTS)
	}
	if fixtureEnd := anchorTS.Add(24 * time.Hour); !end.After(fixtureEnd) {
		t.Errorf("window end %v must cover the fixture end %v", end, fixtureEnd)
	}
	// Loki's default max_query_length is 30d1h (721h). The probe window
	// must sit far inside it no matter how old the anchor gets.
	const maxQueryLength = 721 * time.Hour
	if span := end.Sub(start); span >= maxQueryLength {
		t.Errorf("window span %v exceeds Loki's default max_query_length %v", span, maxQueryLength)
	}
}

// contains is a tiny helper kept local so the test file has no
// dependency on strings.Contains' import.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func mapKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

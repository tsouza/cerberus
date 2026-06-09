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

// TestWaitLokiIndexSettle pins the contract of the metric-based settle
// gate. The previous cardinality-polling implementation accumulated
// four PRs of patches (#66 → #561 → #576 → #608) trying to model
// "ingester is done" indirectly through `/labels` + `/series` counts;
// this gate now reads Loki's own `/metrics` endpoint, which exposes the
// authoritative signals (`loki_ingester_flush_queue_length`,
// `loki_ingester_memory_chunks`, `loki_ingester_chunks_flushed_total`,
// `loki_tsdb_shipper_tables_upload_operation_total`).
//
// The cases below pin:
//   - the AND of all four conditions gating return-nil;
//   - the delta accounting against the pre-push baseline;
//   - a structured error when a required metric is missing (upstream
//     rename detection);
//   - the timeout error shape — every counter's value end-to-end so
//     on-call can root-cause from a single log line.
func TestWaitLokiIndexSettle(t *testing.T) {
	// No t.Parallel(): this test mutates the package-level settle*
	// cadence vars.
	origTimeout, origInterval, origProgressAt := settleTimeout, settleInterval, settleProgressAt
	settleTimeout = 500 * time.Millisecond
	settleInterval = 5 * time.Millisecond
	settleProgressAt = 100 * time.Millisecond
	t.Cleanup(func() {
		settleTimeout = origTimeout
		settleInterval = origInterval
		settleProgressAt = origProgressAt
	})

	const expectedStreams = 13

	// buildMetricsBody returns a `/metrics` text-exposition payload with
	// the four signals the gate keys on. Other metrics are intentionally
	// omitted — the gate must not depend on anything outside its
	// declared contract.
	buildMetricsBody := func(flushQueue, memChunks, chunksFlushed, shipperUploads float64) string {
		return fmt.Sprintf(`# HELP loki_ingester_flush_queue_length unused
# TYPE loki_ingester_flush_queue_length gauge
loki_ingester_flush_queue_length %g
# HELP loki_ingester_memory_chunks unused
# TYPE loki_ingester_memory_chunks gauge
loki_ingester_memory_chunks %g
# HELP loki_ingester_chunks_flushed_total unused
# TYPE loki_ingester_chunks_flushed_total counter
loki_ingester_chunks_flushed_total{reason="forced"} %g
# HELP loki_tsdb_shipper_tables_upload_operation_total unused
# TYPE loki_tsdb_shipper_tables_upload_operation_total counter
loki_tsdb_shipper_tables_upload_operation_total{component="index-store-tsdb-2024-01-01",status="success"} %g
`, flushQueue, memChunks, chunksFlushed, shipperUploads)
	}

	runSettle := func(t *testing.T, handler http.HandlerFunc, baseline lokiMetricsSnapshot) error {
		t.Helper()
		srv := httptest.NewServer(handler)
		t.Cleanup(srv.Close)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		t.Cleanup(cancel)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		return waitLokiIndexSettle(ctx, srv.URL, expectedStreams, baseline, logger)
	}

	t.Run("all_signals_ready_immediately", func(t *testing.T) {
		// Happy path: queue drained, memory empty, counters past
		// baseline. Gate returns on tick 1.
		baseline := lokiMetricsSnapshot{chunksFlushed: 0, shipperUploads: 0}
		body := buildMetricsBody(0, 0, expectedStreams, 1)
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/metrics" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body))
		}
		if err := runSettle(t, handler, baseline); err != nil {
			t.Fatalf("expected nil error for fully-ready server, got: %v", err)
		}
	})

	t.Run("flush_queue_drains_partway_through", func(t *testing.T) {
		// First poll: queue non-zero. Second poll: queue at zero with
		// all other conditions met. The gate must wait for the queue
		// to drain before returning.
		baseline := lokiMetricsSnapshot{chunksFlushed: 0, shipperUploads: 0}
		var polls atomic.Int32
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/metrics" {
				http.NotFound(w, r)
				return
			}
			n := polls.Add(1)
			if n == 1 {
				_, _ = w.Write([]byte(buildMetricsBody(5, 2, 8, 0)))
				return
			}
			_, _ = w.Write([]byte(buildMetricsBody(0, 0, expectedStreams, 1)))
		}
		if err := runSettle(t, handler, baseline); err != nil {
			t.Fatalf("expected nil error after queue drains, got: %v", err)
		}
		if got := polls.Load(); got < 2 {
			t.Fatalf("expected at least 2 polls, got %d", got)
		}
	})

	t.Run("delta_must_exceed_baseline", func(t *testing.T) {
		// The counter values are above zero but match the baseline
		// exactly — no work has happened since baseline was taken.
		// The gate must not return.
		baseline := lokiMetricsSnapshot{chunksFlushed: 50, shipperUploads: 3}
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/metrics" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(buildMetricsBody(0, 0, 50, 3)))
		}
		err := runSettle(t, handler, baseline)
		if err == nil {
			t.Fatalf("expected timeout when counters match baseline, got nil")
		}
		for _, want := range []string{
			"chunks_flushed=50→50",
			"needed_delta=13",
			"shipper_uploads=3→3",
		} {
			if !contains(err.Error(), want) {
				t.Fatalf("timeout error missing diagnostic %q in: %v", want, err)
			}
		}
	})

	t.Run("shipper_upload_required", func(t *testing.T) {
		// Chunks flushed past baseline + queue drained, but the TSDB
		// shipper has not yet uploaded. /query_range would race the
		// index publication — the gate must wait.
		baseline := lokiMetricsSnapshot{chunksFlushed: 0, shipperUploads: 5}
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/metrics" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(buildMetricsBody(0, 0, expectedStreams+10, 5)))
		}
		err := runSettle(t, handler, baseline)
		if err == nil {
			t.Fatalf("expected timeout when shipper upload counter hasn't moved, got nil")
		}
	})

	t.Run("missing_required_metric_errors", func(t *testing.T) {
		// Loki upstream rename / regression: the `flush_queue_length`
		// gauge is missing entirely. The gate must surface this as a
		// structured error rather than treat the missing value as
		// zero (which would silently flip to "settled").
		baseline := lokiMetricsSnapshot{}
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/metrics" {
				http.NotFound(w, r)
				return
			}
			body := `# HELP loki_ingester_memory_chunks unused
# TYPE loki_ingester_memory_chunks gauge
loki_ingester_memory_chunks 0
# HELP loki_ingester_chunks_flushed_total unused
# TYPE loki_ingester_chunks_flushed_total counter
loki_ingester_chunks_flushed_total{reason="forced"} 13
`
			_, _ = w.Write([]byte(body))
		}
		err := runSettle(t, handler, baseline)
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
		baseline := lokiMetricsSnapshot{}
		handler := func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}
		err := runSettle(t, handler, baseline)
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
// settle gate.
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

	// The targeted-label-set form must match only the success row.
	gotUpload := sumMatching(got, `loki_tsdb_shipper_tables_upload_operation_total{component="x",status="success"`)
	if gotUpload != 5 {
		t.Errorf("sumMatching(tables_upload_operation success-only) = %v, want 5", gotUpload)
	}
}

// TestExtractSnapshot pins the baseline-decoder contract.
func TestExtractSnapshot(t *testing.T) {
	t.Parallel()

	metrics := map[string]float64{
		`loki_ingester_chunks_flushed_total{reason="forced"}`:                             11,
		`loki_ingester_chunks_flushed_total{reason="full"}`:                               2,
		`loki_tsdb_shipper_tables_upload_operation_total{component="x",status="success"}`: 4,
		`loki_tsdb_shipper_tables_upload_operation_total{component="x",status="failure"}`: 1,
		`loki_ingester_flush_queue_length`:                                                0,
	}
	snap := extractSnapshot(metrics)
	if snap.chunksFlushed != 13 {
		t.Errorf("chunksFlushed = %v, want 13", snap.chunksFlushed)
	}
	// Only the status="success" row should be in the upload total —
	// the status="failure" row matters for an alerting metric but not
	// for "did the index publish".
	if snap.shipperUploads != 4 {
		t.Errorf("shipperUploads = %v, want 4 (success-only)", snap.shipperUploads)
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

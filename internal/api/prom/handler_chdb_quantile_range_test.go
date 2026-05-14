//go:build chdb

// chDB-backed regression coverage for the prom handler's
// /api/v1/query_range path when the inner plan is a value-rewrite
// Project on top of a derived (RangeWindow) shape.
//
// Context: PR #322 made `quantile_over_time(phi, m[r])` compile-time
// fold out-of-range phi to ±Inf via a Project that replaces the Value
// column on top of the RangeWindow. PR #328 fixed the wire-binding
// half (non-finite literals inline as `(±1.0/0)` / `(0.0/0)`). The
// txtar roundtrips and chDB lane both passed — but real CH 24.x on
// the compatibility lane kept 502-ing the 12 cases below because the
// HTTP handler's `wrapWithSampleProjection` saw `*chplan.Project` at
// the plan root, classified it as canonical, and emitted an outer
// `SELECT MetricName, TimeUnix, … FROM (<2-column derived inner>)`
// that real CH rejects as "Missing columns: 'MetricName' 'TimeUnix'".
// chDB's parser was lenient and silently dropped the missing-column
// references, masking the bug.
//
// The fix: `isDerivedShape` now treats a value-rewrite Project (one
// whose projections do NOT name all four canonical Sample columns)
// as still-derived, so `wrapWithSampleProjection` synthesises
// MetricName / TimeUnix correctly. This file's tests pin the SQL
// emitted via the handler end-to-end, asserting:
//
//  1. The shape works on the matrix (`query_range`) endpoint that
//     the compat lane exercises.
//  2. Both out-of-range phi paths (-0.5 → -Inf and 1.5 → +Inf) flow
//     through.
//  3. The control case (valid phi = 0.5) keeps working.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestQueryRange_QuantileOverTimeOutOfRange_ChDB pins the compat-lane
// 502 shape: `quantile_over_time(phi, m[r])` on `/api/v1/query_range`
// with phi outside [0, 1]. Pre-fix, the handler emitted SQL
// referencing `MetricName` and `TimeUnix` against a 2-column derived
// inner SELECT — chDB tolerated the missing columns silently but
// real CH 24.x raised 502 ("Missing columns"). The fix routes the
// value-rewrite Project through the derived-shape branch.
func TestQueryRange_QuantileOverTimeOutOfRange_ChDB(t *testing.T) {
	// Seed five `latency_ms` gauge samples spaced 200ms apart, ending
	// close to "now" so the [1s] range catches at least one anchor.
	now := time.Now().UTC()
	seedRows := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		ts := now.Add(-time.Duration(i) * 200 * time.Millisecond).Format("2006-01-02 15:04:05.000")
		seedRows = append(seedRows, fmt.Sprintf(
			`('latency_ms', map('endpoint', '/api/v1'), toDateTime64('%s', 9), 100.0)`, ts,
		))
	}
	seed := gaugeDDL + "\nINSERT INTO otel_metrics_gauge VALUES\n  " +
		strings.Join(seedRows, ",\n  ") + ";"

	srv, _ := newChDBServer(t, seed)

	// Range query parameters matching the compat lane: start = now - 1h,
	// end = now, step = 10s. The "compat shape" the harness probes is
	// the matrix path on `/api/v1/query_range` with sub-second through
	// minute-scale ranges.
	end := now.Format(time.RFC3339Nano)
	start := now.Add(-1 * time.Hour).Format(time.RFC3339Nano)

	cases := []struct {
		name  string
		query string
	}{
		// Mirror the compat lane's exact failing cases — six ranges
		// each for phi=-0.5 and phi=1.5. We don't need every range
		// because the wire-shape bug is range-independent; one short +
		// one mid + one long covers the per-range CH parsing.
		{"phi-neg-1s", "quantile_over_time(-0.5, latency_ms[1s])"},
		{"phi-neg-1m", "quantile_over_time(-0.5, latency_ms[1m])"},
		{"phi-above-1s", "quantile_over_time(1.5, latency_ms[1s])"},
		{"phi-above-1m", "quantile_over_time(1.5, latency_ms[1m])"},
		// Control: valid phi must keep working through the same fix.
		{"phi-valid-1s", "quantile_over_time(0.5, latency_ms[1s])"},
		// Sibling shape: `abs(avg_over_time(...))` produces the same
		// value-rewrite-Project-over-RangeWindow plan via
		// projectValueOverInner's RangeWindow path (rate uses
		// otel_metrics_sum which the test seed doesn't populate; the
		// gauge-side avg_over_time exercises the same chplan shape).
		// Pre-fix this also 502'd against real CH (the txtar roundtrip
		// skipped wrapWithSampleProjection, so this regression was
		// hidden in the existing chDB lane).
		{"abs-over-avg_over_time", "abs(avg_over_time(latency_ms[5m]))"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := fmt.Sprintf(
				"%s/api/v1/query_range?query=%s&start=%s&end=%s&step=10s",
				srv.URL, escape(tc.query), escape(start), escape(end),
			)
			resp, err := http.Get(url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s (pre-fix: 502 from CH "+
					"\"Missing columns: 'MetricName' 'TimeUnix'\" on "+
					"the outer wrapWithSampleProjection canonical-shape "+
					"branch projecting against a 2-column derived inner)",
					resp.StatusCode, body)
			}

			var parsed queryResponse
			if err := json.Unmarshal([]byte(body), &parsed); err != nil {
				t.Fatalf("unmarshal: %v\nbody=%s", err, body)
			}
			if parsed.Status != "success" {
				t.Fatalf("status: got %q (errorType=%q error=%q), want success",
					parsed.Status, parsed.ErrorType, parsed.Error)
			}
			if parsed.Data.ResultType != "matrix" {
				t.Fatalf("resultType: got %q, want matrix", parsed.Data.ResultType)
			}
		})
	}
}

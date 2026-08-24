//go:build chdb

// chDB-backed proof that a bare TOP-LEVEL range-vector selector over a
// native (OTel exponential) histogram metric — `demo_latency_exp_hist[5m]`
// sent to /api/v1/query, no wrapping function — answers with reference
// Prometheus's own "raw range vector" contract: every raw in-window
// sample, ORIGINAL timestamps preserved, no resampling to a grid anchor
// and no per-series collapse (cerberus issue #2548).
//
// Before this fix, [lowerRoot]'s histogram-native dispatch table
// ([lowerHistogramNativeRoot], lower.go) had no recognizer for a bare
// *parser.MatrixSelector at the query root — `lower`'s own
// *parser.MatrixSelector case called lowerMatrixSelector directly, which
// stripped the modifier and fell into the GENERIC lowerVectorSelector
// path, hard-rejecting via expHistogramSelectorRouting's catch-all. This
// file proves the fixed lowering against a real chDB execution rather
// than merely the emitted plan's Go shape: irregular real timestamps
// inside the window all come back with their OWN timestamp and their OWN
// per-row Count/Sum/bucket values (not merged, not resampled), a sample
// exactly at the window's left-open edge is excluded, and a sample
// exactly at the right-closed edge (the eval timestamp itself) is
// included — proving the exact same left-open/right-closed contract
// [lowerMatrixSelector]'s own doc describes for a float metric.
package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestBareMatrixExpHistogram_ChDB seeds one series with four samples at
// irregular real timestamps — three inside the (evalTS−5m, evalTS]
// window, one exactly on each boundary edge — plus a second series with
// a single in-window sample, and proves `<metric>[5m]` returns exactly
// the five in-window rows, each with its own original timestamp and its
// own Count/Sum/first-bucket values, and excludes the two out-of-window
// samples (one strictly before the window, one exactly on the excluded
// left-open edge).
func TestBareMatrixExpHistogram_ChDB(t *testing.T) {
	const metric = "bare_matrix_exp_hist"
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		// Series "a": three irregular in-window samples.
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2026-01-01 00:01:17', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2026-01-01 00:03:42', 9), 3, 9.0, 0, 0, 0, [12], 0, []),\n" +
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2026-01-01 00:04:59', 9), 5, 1.0, 0, 0, 0, [3], 0, []),\n" +
		// Series "a": exactly on the right-closed edge (the eval
		// timestamp itself) — INCLUDED.
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2026-01-01 00:05:00', 9), 7, 2.0, 0, 0, 0, [1], 0, []),\n" +
		// Series "a": exactly on the left-open edge — EXCLUDED (strict
		// `>`, not `>=`).
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2026-01-01 00:00:00', 9), 99, 99.0, 0, 0, 0, [99], 0, []),\n" +
		// Series "a": strictly before the window — EXCLUDED.
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2025-12-31 23:59:00', 9), 100, 100.0, 0, 0, 0, [100], 0, []),\n" +
		// Series "b": a single in-window sample, proving series are not
		// merged/collapsed together.
		"    ('" + metric + "', map('series', 'b'), toDateTime64('2026-01-01 00:02:30', 9), 11, 22.0, 0, 0, 0, [33], 0, []);\n"
	fixture := newChDBFixture(t, seed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	evalTS := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

	query := metric + "[5m]"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) != 5 {
		t.Fatalf("%s: got %d rows, want 5 (the in-window samples of both series, boundary-inclusive): %+v", query, len(rows), rows)
	}

	wantA := map[int64]subqHistRow{
		time.Date(2026, 1, 1, 0, 1, 17, 0, time.UTC).Unix(): {cnt: 2, sum: 4.0, bucket1: 6},
		time.Date(2026, 1, 1, 0, 3, 42, 0, time.UTC).Unix(): {cnt: 3, sum: 9.0, bucket1: 12},
		time.Date(2026, 1, 1, 0, 4, 59, 0, time.UTC).Unix(): {cnt: 5, sum: 1.0, bucket1: 3},
		time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC).Unix():  {cnt: 7, sum: 2.0, bucket1: 1},
	}
	gotA := 0
	gotB := 0
	for _, r := range rows {
		switch r.series {
		case "a":
			gotA++
			want, ok := wantA[r.ts]
			if !ok {
				t.Errorf("%s: unexpected series a row at ts=%d: %+v", query, r.ts, r)
				continue
			}
			if r.cnt != want.cnt || r.sum != want.sum || r.bucket1 != want.bucket1 {
				t.Errorf("%s: series a at ts=%d = %+v, want Count=%v Sum=%v Bucket1=%v", query, r.ts, r, want.cnt, want.sum, want.bucket1)
			}
		case "b":
			gotB++
			wantTS := time.Date(2026, 1, 1, 0, 2, 30, 0, time.UTC).Unix()
			if r.ts != wantTS || r.cnt != 11 || r.sum != 22.0 || r.bucket1 != 33 {
				t.Errorf("%s: series b row = %+v, want ts=%d Count=11 Sum=22 Bucket1=33", query, r, wantTS)
			}
		default:
			t.Errorf("%s: unexpected series %q: %+v", query, r.series, r)
		}
	}
	if gotA != 4 {
		t.Errorf("%s: got %d series-a rows, want 4 (one per in-window timestamp, boundary-inclusive)", query, gotA)
	}
	if gotB != 1 {
		t.Errorf("%s: got %d series-b rows, want 1", query, gotB)
	}
}

// TestBareMatrixExpHistogram_EmptyWindow_ChDB proves an empty in-window
// result — no samples fall inside (evalTS−5m, evalTS] — returns zero
// rows rather than erroring, mirroring reference Prometheus's own empty
// matrix result for a range with no data.
func TestBareMatrixExpHistogram_EmptyWindow_ChDB(t *testing.T) {
	const metric = "bare_matrix_empty_exp_hist"
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + metric + "', map('series', 'a'), toDateTime64('2020-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []);\n"
	fixture := newChDBFixture(t, seed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	evalTS := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)

	query := metric + "[5m]"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	rows := subqHistQueryRows(t, fixture, sqlStr, args)
	if len(rows) != 0 {
		t.Fatalf("%s: got %d rows, want 0 (the seeded sample is years outside the window): %+v", query, len(rows), rows)
	}
}

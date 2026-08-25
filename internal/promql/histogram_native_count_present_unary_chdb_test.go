//go:build chdb

// chDB-backed proof that count_over_time()/present_over_time() over an
// exp-histogram selector, when wrapped by unary `-`/`+` (cerberus issue
// #2591) — or by any other wrapper that reaches the call through the
// generic [promql.lower] dispatcher rather than as the literal query root —
// lower to valid SQL and EXECUTE against real ClickHouse reporting the
// correct FLOAT value, rather than hard-rejecting via
// expHistogramSelectorRouting's catch-all.
//
// One series (service="checkout") carries three samples inside the [5m]
// window evaluated at nestedCountPresentEvalTS, so:
//   - count_over_time(...) answers 3 (a genuine in-window row count).
//   - present_over_time(...) answers 1 (reference never counts beyond
//     existence).
//
// Each case pins the SAME per-series count/presence answer a bare,
// unwrapped count_over_time()/present_over_time() query already answers
// correctly (cerberus issue #2480; this issue's fix does not touch that
// path) run through the wrapper's own arithmetic — mirroring
// histogram_native_resets_changes_count_nested_chdb_test.go's own template
// for the sibling count()/group() nested-retry fix (cerberus issue #2549).
package promql_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

const (
	nestedCountPresentMetric = "nested_count_present_probe_exp_hist"
)

var nestedCountPresentSeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + nestedCountPresentMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [5], 0, []),\n" +
	"    ('" + nestedCountPresentMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:01:00', 9), 8, 16.0, 0, 0, 0, [8], 0, []),\n" +
	"    ('" + nestedCountPresentMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:02:00', 9), 3, 6.0, 0, 0, 0, [3], 0, []);\n"

// nestedCountPresentEvalTS sits just after the window's last sample
// (00:02:00), with the [5m] range comfortably covering all three samples.
var nestedCountPresentEvalTS = time.Date(2026, 1, 1, 0, 2, 5, 0, time.UTC)

func TestLower_ExpHistogram_CountPresentUnaryNested_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, nestedCountPresentSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	countQ := "count_over_time(" + nestedCountPresentMetric + "{service=\"checkout\"}[5m])"
	presentQ := "present_over_time(" + nestedCountPresentMetric + "{service=\"checkout\"}[5m])"

	cases := []struct {
		name  string
		query string
		want  float64
	}{
		// This issue's own two trigger queries, reproduced against the
		// seeded fixture (the issue's own body uses demo_latency_exp_hist,
		// only representative — this package's chdb tests seed their own
		// per-file metric).
		{"unary_minus_count_over_time", "-" + countQ, -3},
		{"unary_minus_present_over_time", "-" + presentQ, -1},
		// Unary `+` is the identity case (cerberus issue #2583's own
		// unaryOverExpHistogram doc) — proves the retry serves BOTH
		// UnaryExpr operators, not just SUB.
		{"unary_plus_count_over_time", "+" + countQ, 3},
		{"unary_plus_present_over_time", "+" + presentQ, 1},
		// A double wrapper (unary minus THEN scalar arithmetic) and a
		// non-unary wrapper (sum/abs), proving [lowerRangeVectorCall]'s
		// retry — added generically, not unary-specifically — closes the
		// gap for every wrapper that reaches the call through the generic
		// dispatcher, exactly as the issue's own root-cause note predicts.
		{"unary_minus_count_times_2", "-" + countQ + " * 2", -6},
		{"sum_unary_minus_present", "sum(-" + presentQ + ")", -1},
		{"abs_unary_minus_count", "abs(-" + countQ + ")", 3},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, nestedCountPresentEvalTS, nestedCountPresentEvalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error (this exact nested shape hard-rejected via expHistogramSelectorRouting's catch-all before cerberus issue #2591's fix): %v", tc.query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}

			rows := fixture.queryOverEmitted(t, "Value", sqlStr, args)
			defer func() { _ = rows.Close() }()

			n := 0
			for rows.Next() {
				n++
				var v float64
				if err := rows.Scan(&v); err != nil {
					t.Fatalf("scan Value: %v", err)
				}
				if math.Abs(v-tc.want) > 1e-9 {
					t.Errorf("query %q: Value = %v, want %v", tc.query, v, tc.want)
				}
			}
			if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			if n != 1 {
				t.Fatalf("query %q: got %d rows, want exactly 1", tc.query, n)
			}
		})
	}
}

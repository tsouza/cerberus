//go:build chdb

// chDB-backed proof that resets()/changes()/count() over a NESTED
// exp-histogram selector (cerberus issue #2549) not only lowers to valid
// SQL but EXECUTES against real ClickHouse and reports the correct
// FLOAT value — the reset/change/presence count reference Prometheus
// itself would answer, put through the wrapper's own arithmetic.
//
// Two series are seeded for nestedResetsCountMetric:
//   - service="checkout": three samples across the [5m] window. The
//     first two GROW the histogram (Count 5 -> 8, buckets [5] -> [8]) —
//     DetectReset sees no regression, so this pair is a `changes()` hit
//     but not a `resets()` hit. The last pair SHRINKS it (Count 8 -> 3,
//     buckets [8] -> [3]) — Count regressing is one of
//     FloatHistogram.DetectReset's own conditions
//     (histogram_native_reset.go's own doc lists it first), so this pair
//     is both a `resets()` hit and a `changes()` hit. Net over the
//     window: resets()=1, changes()=2.
//   - service="checkout-2": a single sample, present only so count()
//     over the instant vector has two series to count rather than one —
//     a bare count() of 1 could not distinguish "the wrapper forwarded
//     the inner count" from "the wrapper silently produced its own
//     literal 1".
//
// Each query pins the SAME per-series reset/change/count answer a bare,
// unwrapped resets()/changes()/count() query already answers correctly
// (this issue's fix does not touch that path) run through the wrapper's
// own arithmetic, so a wrong wrapper composition (rather than a wrong
// resets/changes/count kernel) is what a failing assertion here would
// mean.
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
	nestedResetsCountMetric = "nested_resets_count_probe_exp_hist"
)

var nestedResetsCountSeed = "" +
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
	"    ('" + nestedResetsCountMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [5], 0, []),\n" +
	"    ('" + nestedResetsCountMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:01:00', 9), 8, 16.0, 0, 0, 0, [8], 0, []),\n" +
	"    ('" + nestedResetsCountMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:02:00', 9), 3, 6.0, 0, 0, 0, [3], 0, []),\n" +
	"    ('" + nestedResetsCountMetric + "', map('service', 'checkout-2'), toDateTime64('2026-01-01 00:02:00', 9), 4, 8.0, 0, 0, 0, [4], 0, []);\n"

// nestedResetsCountEvalTS sits just after the window's last sample
// (00:02:00), with the [5m] range comfortably covering all three
// checkout-service samples (00:00:00, 00:01:00, 00:02:00).
var nestedResetsCountEvalTS = time.Date(2026, 1, 1, 0, 2, 5, 0, time.UTC)

func TestLower_ExpHistogram_ResetsChangesCountNested_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, nestedResetsCountSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	resetsQ := "resets(" + nestedResetsCountMetric + "{service=\"checkout\"}[5m])"
	changesQ := "changes(" + nestedResetsCountMetric + "{service=\"checkout\"}[5m])"
	countQ := "count(" + nestedResetsCountMetric + ")"

	cases := []struct {
		name  string
		query string
		want  float64
	}{
		// This issue's own four trigger queries (against the shared
		// nestedResetsCountMetric fixture rather than demo_latency_exp_hist,
		// since the trigger queries themselves are only representative
		// shapes — the issue's own text names them against a metric this
		// package's chdb tests do not seed).
		{"resets_times_2", resetsQ + " * 2", 2}, // resets()=1, *2 = 2
		{"sum_resets", "sum(" + resetsQ + ")", 1},
		{"changes_plus_1", changesQ + " + 1", 3}, // changes()=2, +1 = 3
		{"abs_changes", "abs(" + changesQ + ")", 2},

		// The issue's own architectural note names count() as the third
		// member of the FLOAT-valued family alongside resets/changes.
		{"count_times_2", countQ + " * 2", 4}, // count()=2 series, *2 = 4
		{"sum_count", "sum(" + countQ + ")", 2},
		{"abs_count", "abs(" + countQ + ")", 2},

		// A double wrapper, and the aggregation-over-scaled-call shape:
		// proves the fix composes rather than only matching a single
		// level of nesting.
		{"sum_resets_times_2", "sum(" + resetsQ + " * 2)", 2},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, nestedResetsCountEvalTS, nestedResetsCountEvalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error (this exact nested shape hard-rejected via expHistogramSelectorRouting's catch-all before cerberus issue #2549's fix): %v", tc.query, err)
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

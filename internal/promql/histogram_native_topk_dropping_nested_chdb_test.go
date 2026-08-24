//go:build chdb

// chDB-backed proof that a "dropping" aggregation (topk/bottomk, and their
// five drop-family siblings min/max/stddev/stdvar/quantile) directly over a
// histogram-VALUED shape, NESTED as the argument of a further wrapper such
// as histogram_quantile() or sum() (cerberus issue #2562), now lowers to
// valid SQL and EXECUTES against real ClickHouse, returning the real empty
// result reference Prometheus answers there — not merely that a Go-level
// lowering error stopped being raised.
//
// TestLower_ExpHistogram_DroppingAggregationComposesUnderWrappers
// (histogram_native_drop_aggregation_test.go) already pins the SAME shapes
// at the Go-plan-shape level; this file is that fixture's chDB-executed
// sibling, following the pattern
// TestLower_ExpHistogram_DroppingShapeNestedInBinop_ChDB
// (histogram_native_dropping_nested_binop_chdb_test.go) established for the
// sibling "dropping binop nested under a wrapper" issue (#2534).
//
// The metric is seeded with REAL, non-empty rows so a returned zero-row
// result can only mean the drop actually fired, never that no data was ever
// scanned.
package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

const topkDroppingNestedMetric = "topk_dropping_nested_probe_exp_hist"

var topkDroppingNestedSeed = "" +
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
	"    ('" + topkDroppingNestedMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [5], 0, []),\n" +
	"    ('" + topkDroppingNestedMetric + "', map('service', 'cart'), toDateTime64('2026-01-01 00:00:00', 9), 9, 20.0, 0, 0, 0, [9], 0, []);\n"

var topkDroppingNestedEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// TestLower_ExpHistogram_DroppingAggregationNestedUnderWrapper_ChDB pins
// cerberus issue #2562's own trigger query
// (`histogram_quantile(0.5, topk(1, <exp-histogram selector>))`) plus five
// sibling shapes covering the rest of the "dropping" op family and a
// non-histogram_quantile wrapper, executed against real ClickHouse.
func TestLower_ExpHistogram_DroppingAggregationNestedUnderWrapper_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, topkDroppingNestedSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	queries := []string{
		// The issue's own trigger query.
		"histogram_quantile(0.5, topk(1, " + topkDroppingNestedMetric + "))",
		"histogram_quantile(0.9, bottomk(1, " + topkDroppingNestedMetric + "))",
		// The other five dropping ops, nested under a non-histogram_quantile
		// wrapper — sum() reduces the (already-empty) result, so this also
		// proves the reshaped output composes under further aggregation.
		"sum(min(" + topkDroppingNestedMetric + "))",
		"sum(max(" + topkDroppingNestedMetric + "))",
		"sum(stddev(" + topkDroppingNestedMetric + "))",
		"sum(stdvar(" + topkDroppingNestedMetric + "))",
		"sum(quantile(0.9, " + topkDroppingNestedMetric + "))",
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, topkDroppingNestedEvalTS, topkDroppingNestedEvalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error (this exact shape hard-rejected via expHistogramSelectorRouting's catch-all before cerberus issue #2562's fix): %v", query, err)
			}
			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", query, err)
			}

			rows := fixture.queryOverEmitted(t, "count() AS n", sqlStr, args)
			defer func() { _ = rows.Close() }()
			if !rows.Next() {
				t.Fatalf("count() query returned no rows")
			}
			var n int64
			if err := rows.Scan(&n); err != nil {
				t.Fatalf("scan count(): %v", err)
			}
			if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			if n != 0 {
				t.Fatalf(
					"query %q returned count()=%d, want 0 — reference drops every native-histogram sample under min/max/stddev/stdvar/quantile/topk/bottomk; the metric was seeded with REAL non-empty rows, so a non-zero count would mean the drop never actually fired at execution",
					query, n,
				)
			}
		})
	}
}

//go:build chdb

// chDB-backed proof that a drop-family exp-histogram binop nested as
// ANOTHER binop's own operand (cerberus issue #2534) now lowers to valid
// SQL and EXECUTES against real ClickHouse — not merely that a Go-level
// lowering error stopped being raised.
//
// binary.go's lowerScalarBinopOperand / lowerVectorVectorOperand and
// histogram_native_set_op.go's lowerVectorSetOpOperand are the new
// per-operand opt-ins this issue's fix threads into the generic recursive
// binop lowering; TestLower_ExpHistogram_DroppingShapeComposesUnderWrappers
// (histogram_native_dropping_shape_test.go) already pins the SAME leaf
// composed under a WRAPPER's own argument position (cerberus issue #2528)
// at the Go-plan-shape level — this file is that fixture's chDB-executed,
// binop-nested sibling, using the issue's own four trigger queries
// verbatim.
//
// The metric is seeded with a REAL, non-empty row so a returned zero-row
// result can only mean the drop actually fired — not that no data was
// ever scanned.
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

const nestedBinopDropMetric = "nested_binop_drop_probe_exp_hist"

var nestedBinopDropSeed = "" +
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
	"    ('" + nestedBinopDropMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [5], 0, []);\n"

var nestedBinopDropEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// TestLower_ExpHistogram_DroppingShapeNestedInBinop_ChDB pins cerberus
// issue #2534's own four trigger queries: a drop-family binop
// (`<metric> + 0`) nested as the operand of a further scalar binop
// ([lowerScalarBinopOperand]), a further vector-vector binop reached
// through an aggregation wrapper ([lowerVectorVectorOperand]), and a
// vector set op ([lowerVectorSetOpOperand]'s own drop-family opt-in).
// Every one of these previously hard-rejected via
// expHistogramSelectorRouting's catch-all before ever reaching SQL.
func TestLower_ExpHistogram_DroppingShapeNestedInBinop_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, nestedBinopDropSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	queries := []string{
		// lowerVectorScalar's operand opt-in (a scalar on the outer
		// binop's RHS).
		"(" + nestedBinopDropMetric + " + 0) * 2",
		"(" + nestedBinopDropMetric + " + 0) + 5",
		// lowerVectorScalar's operand opt-in again, this time over an
		// AggregateExpr wrapping the drop-family leaf (composes
		// [lowerExpHistogramDroppingShape]'s own aggregation recursion,
		// cerberus issue #2528, one level deeper).
		"sum(" + nestedBinopDropMetric + " + 0) + 1",
		// lowerVectorSetOpOperand's drop-family opt-in.
		"(" + nestedBinopDropMetric + " + 0) or (" + nestedBinopDropMetric + " + 0)",
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, nestedBinopDropEvalTS, nestedBinopDropEvalTS)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error (this exact shape hard-rejected via expHistogramSelectorRouting's catch-all before cerberus issue #2534's fix): %v", query, err)
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
					"query %q returned count()=%d, want 0 — reference drops every sample through an incompatible-type histogram/scalar (or histogram/histogram) binop; the metric was seeded with a REAL non-empty row, so a non-zero count would mean the drop never actually fired at execution",
					query, n,
				)
			}
		})
	}
}

//go:build chdb

// chDB-backed proof that `count_values("label", <exp-histogram operand>)`,
// NESTED as the argument of a further wrapper such as histogram_quantile()
// or sum() (cerberus issue #2568), now lowers to valid SQL and EXECUTES
// against real ClickHouse, returning the real answer reference Prometheus
// gives there — not merely that a Go-level lowering error stopped being
// raised.
//
// Bare `count_values("label", <exp-histogram selector>)` already lowered
// cleanly via [lowerHistogramNativeRoot]'s own root-only dispatch to
// [countValuesOverExpHistogramValue]; the bug this file exercises is that
// any wrapper around that shape reached the histogram selector underneath
// through [lowerAggregate]'s generic `lower(a.Expr, ...)` fallback instead,
// which hits [expHistogramSelectorRouting]'s hard rejection. Same bug
// class as #2562 (histogram_native_topk_dropping_nested_chdb_test.go is
// that issue's own chDB sibling), but count_values belongs to the
// "preserve" family rather than the "dropping" one, so the fix threads
// [countValuesOverExpHistogramValue] into [lowerCountValues] itself.
//
// The metric is seeded with REAL, non-empty rows so a returned row count
// can only mean the fix actually executed against real data, never that no
// data was ever scanned.
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

const countValuesNestedMetric = "count_values_nested_probe_exp_hist"

var countValuesNestedSeed = "" +
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
	"    ('" + countValuesNestedMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [5], 0, []),\n" +
	"    ('" + countValuesNestedMetric + "', map('service', 'cart'), toDateTime64('2026-01-01 00:00:00', 9), 9, 20.0, 0, 0, 0, [9], 0, []);\n"

var countValuesNestedEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// TestLower_ExpHistogram_CountValuesNestedUnderWrapper_ChDB pins cerberus
// issue #2568's own trigger query (`histogram_quantile(0.5, count_values("v",
// <exp-histogram selector>))`) plus a non-histogram_quantile wrapper, both
// executed against real ClickHouse.
func TestLower_ExpHistogram_CountValuesNestedUnderWrapper_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, countValuesNestedSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	lower := func(t *testing.T, query string) (string, []any) {
		t.Helper()
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		plan, err := promql.LowerAt(context.Background(), expr, s, countValuesNestedEvalTS, countValuesNestedEvalTS)
		if err != nil {
			t.Fatalf("LowerAt(%q): unexpected error (this exact shape hard-rejected via expHistogramSelectorRouting's catch-all before cerberus issue #2568's fix): %v", query, err)
		}
		sqlStr, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit(%q): %v", query, err)
		}
		return sqlStr, args
	}

	// The issue's own trigger query. Reference Prometheus's classic
	// histogram_quantile requires an `le` label on every sample it
	// buckets; count_values's own synthetic label ("v") never carries
	// one, so every group is dropped and the correct real answer is an
	// EMPTY result — proving the query executes cleanly against
	// non-empty seed data is what distinguishes "the fix actually ran"
	// from "there was nothing to scan".
	t.Run("histogram_quantile(0.5, count_values(...)) is empty (no le label)", func(t *testing.T) {
		query := `histogram_quantile(0.5, count_values("v", ` + countValuesNestedMetric + `))`
		sqlStr, args := lower(t, query)
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
			t.Fatalf("query %q returned count()=%d, want 0 (no `le` label survives count_values)", query, n)
		}
	})

	// A non-histogram_quantile wrapper (sum()) around the identical
	// count_values(...) shape: reference Prometheus's count_values emits
	// one row per distinct stringified value, and summing collapses that
	// back down to the total number of underlying series — proving the
	// nested lowering produces a REAL, non-empty, numerically correct
	// result rather than merely avoiding an error.
	t.Run("sum(count_values(...)) totals the underlying series", func(t *testing.T) {
		query := `sum(count_values("v", ` + countValuesNestedMetric + `))`
		sqlStr, args := lower(t, query)
		rows := fixture.queryOverEmitted(t, "`Value` AS val", sqlStr, args)
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			t.Fatalf("query %q returned no rows", query)
		}
		var val float64
		if err := rows.Scan(&val); err != nil {
			t.Fatalf("scan Value: %v", err)
		}
		if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		const wantTotal = 2.0 // two seeded series (checkout, cart)
		if val != wantTotal {
			t.Fatalf("query %q returned Value=%v, want %v (total seeded series)", query, val, wantTotal)
		}
	})
}

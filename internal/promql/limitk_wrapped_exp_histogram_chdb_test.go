//go:build chdb

// chDB-backed proof that `limitk`/`limit_ratio` over an exp-histogram
// operand, wrapped by a further float-only function (cerberus issue
// #2575), now lowers to valid SQL and EXECUTES against real ClickHouse —
// answering the real empty result reference Prometheus's float-only
// functions give for a native-histogram sample, rather than panicking
// with "promql: projectValueOverInner received a histogram-shaped
// input: ...".
//
// TestLower_ExpHistogram_LimitKAndLimitRatioComposeUnderFloatOnlyWrappers
// (limitk_wrapped_exp_histogram_test.go) already pins the SAME shapes at
// the Go-plan-shape level; this file is that fixture's chDB-executed
// sibling, following the pattern
// TestLower_ExpHistogram_DroppingAggregationNestedUnderWrapper_ChDB
// (histogram_native_topk_dropping_nested_chdb_test.go) established for
// the sibling "dropping op nested under a wrapper" issue (#2562).
//
// The metric is seeded with THREE series' worth of REAL, non-empty rows
// so a returned row count can only mean the lowering actually preserved
// (K=2 of 3) or dropped (every row) the sample — never that no data was
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

const limitkWrappedMetric = "limitk_wrapped_probe_exp_hist"

var limitkWrappedSeed = "" +
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
	"    ('" + limitkWrappedMetric + "', map('service', 'checkout'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [5], 0, []),\n" +
	"    ('" + limitkWrappedMetric + "', map('service', 'cart'), toDateTime64('2026-01-01 00:00:00', 9), 9, 20.0, 0, 0, 0, [9], 0, []),\n" +
	"    ('" + limitkWrappedMetric + "', map('service', 'search'), toDateTime64('2026-01-01 00:00:00', 9), 3, 6.0, 0, 0, 0, [3], 0, []);\n"

var limitkWrappedEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// countRowsOverEmitted lowers query, emits it, and returns the row count
// the emitted SQL produces against fixture. Isolated here so both tests
// below share the exact same execution path.
func countRowsOverEmitted(t *testing.T, fixture *chdbFixture, s schema.Metrics, query string) int64 {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, limitkWrappedEvalTS, limitkWrappedEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): unexpected error (this exact shape panicked or was rejected before cerberus issue #2575's fix): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}

	rows := fixture.queryOverEmitted(t, "count() AS n", sqlStr, args)
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("count() query for %q returned no rows", query)
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan count() for %q: %v", query, err)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err for %q: %v", query, err)
	}
	return n
}

// TestLower_ExpHistogram_LimitKWrappedByFloatOnlyFn_ChDB pins cerberus
// issue #2575's own trigger query (`abs(limitk(2, <exp-hist selector>))`)
// plus its siblings, executed against real ClickHouse: every one must
// answer count()=0 — reference's float-only functions drop every
// native-histogram sample — never panic and never forward a non-empty
// histogram-shaped result.
func TestLower_ExpHistogram_LimitKWrappedByFloatOnlyFn_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, limitkWrappedSeed)
	s := schema.DefaultOTelMetrics()

	queries := []string{
		// The issue's own trigger query.
		"abs(limitk(2, " + limitkWrappedMetric + "))",
		"ceil(limitk(2, " + limitkWrappedMetric + "))",
		"sqrt(limitk(2, " + limitkWrappedMetric + "))",
		"limitk(2, " + limitkWrappedMetric + ") + 0",
		// limit_ratio reproduces identically.
		"abs(limit_ratio(0.5, " + limitkWrappedMetric + "))",
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			n := countRowsOverEmitted(t, fixture, s, query)
			if n != 0 {
				t.Fatalf(
					"query %q returned count()=%d, want 0 — reference drops every native-histogram sample under a float-only wrapper; the metric was seeded with THREE REAL non-empty series, so a non-zero count would mean the drop never actually fired at execution",
					query, n,
				)
			}
		})
	}
}

// TestLower_ExpHistogram_LimitKUnwrapped_ChDB is the contrasting
// regression guard, executed against the SAME seed and fixture: the
// UNWRAPPED case cerberus issue #2518 shipped must keep preserving K of
// the 3 seeded series' histogram rows, end to end — not silently regress
// to 0 (over-dropping) or 3 (K never applied) once the wrapped-composition
// fix above lands.
func TestLower_ExpHistogram_LimitKUnwrapped_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, limitkWrappedSeed)
	s := schema.DefaultOTelMetrics()

	const k = 2
	query := "limitk(2, " + limitkWrappedMetric + ")"
	n := countRowsOverEmitted(t, fixture, s, query)
	if n != k {
		t.Fatalf("query %q returned count()=%d, want %d — unwrapped limitk must keep preserving exactly K of the 3 seeded series' histogram rows (cerberus issue #2518 must not regress)", query, n, k)
	}
}

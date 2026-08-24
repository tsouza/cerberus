//go:build chdb

// chDB-backed proof that histogram_count()/histogram_sum()/histogram_avg()/
// histogram_quantile() actually read a real, correctly-merged distribution
// at real ClickHouse execution when their argument is itself a
// HISTOGRAM-VALUED shape rather than a bare selector — sum(m), avg(m),
// rate(m[5m]), a histogram-histogram binop, a set-op — not merely that the
// emitted plan's Go shape lowers without error (cerberus issue #2554).
//
// Before this fix, lowerHistogramValueFn's (histogram_value_fns.go) and
// lowerHistogramQuantile's (histogram_quantile.go) non-bare-selector
// fallbacks assumed any such argument provably carried no native-histogram
// samples and folded to an empty result — silently wrong for every shape
// below, which all answer histogram-valued under reference Prometheus.
// Threading isExpHistogramValuedShape / lowerExpHistogramValuedShape ahead
// of that fallback (mirroring every other wrapper in this package) fixes
// the routing; these tests additionally caught a second, genuinely silent
// bug the routing fix alone did not: a histogram-valued plan's raw
// structural columns publish under the FIXED chplan.Histogram*Column
// aliases (HistogramCount, HistogramSum, …), not the schema's own raw
// names — reading them via the raw schema.Metrics would reference columns
// hp never emits (UNKNOWN_IDENTIFIER at real ClickHouse execution, not a
// Go-level lowering error), which is exactly the class of bug a
// Go-only "does it lower" probe cannot catch.
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

// hvfnNestedRateDDL is subqHistDDL's shape (subquery_histogram_native_chdb_test.go)
// widened with AggregationTemporality — rate()'s DELTA-vs-CUMULATIVE branch
// (schema.AggregationTemporalityColumn) reads that column unconditionally,
// unlike every other shape this file seeds against subqHistDDL directly.
const hvfnNestedRateDDL = "CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64), " +
	"`AggregationTemporality` Int32 DEFAULT 2" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n"

// hvfnRunScalar lowers query at evalTS, emits it, and returns the single
// `Value` its Sample row carries — failing the test if the query does not
// produce exactly one row.
func hvfnRunScalar(t *testing.T, fixture *chdbFixture, s schema.Metrics, query string, evalTS time.Time) float64 {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
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
	rows := fixture.queryOverEmitted(t, "`Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()
	var got float64
	var n int
	for rows.Next() {
		if err := rows.Scan(&got); err != nil {
			t.Fatalf("%s: scan: %v", query, err)
		}
		n++
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("%s: rows.Err: %v", query, err)
	}
	if n != 1 {
		t.Fatalf("%s: got %d rows, want exactly 1", query, n)
	}
	return got
}

// TestHistogramValueFnNestedSum_ChDB proves histogram_count()/
// histogram_sum() over sum() and histogram_avg() over avg() — the
// issue's first and third trigger queries — merge two series' Count/Sum
// scalars correctly before the value function reads them.
//
// sum(m): Count=2+5=7, Sum=4.0+1.0=5.0.
// avg(m): the same merge divided by the group's member count (2):
// Count=3.5, Sum=2.5, so histogram_avg = Sum/Count = 2.5/3.5.
func TestHistogramValueFnNestedSum_ChDB(t *testing.T) {
	const metric = "hvfn_nested_sum_exp_hist"
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + metric + "', map('job', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
		"    ('" + metric + "', map('job', 'b'), toDateTime64('2026-01-01 00:01:00', 9), 5, 1.0, 0, 0, 0, [3], 0, []);\n"
	fixture := newChDBFixture(t, seed)

	s := schema.DefaultOTelMetrics()
	evalTS := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)

	cases := []struct {
		query string
		want  float64
	}{
		{"histogram_count(sum(" + metric + "))", 7},
		{"histogram_sum(sum(" + metric + "))", 5.0},
		{"histogram_avg(avg(" + metric + "))", 2.5 / 3.5},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			got := hvfnRunScalar(t, fixture, s, tc.query, evalTS)
			if got != tc.want {
				t.Errorf("%s = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestHistogramValueFnNestedRate_ChDB proves histogram_sum(rate(m[5m])) —
// the issue's second trigger query — reads the SAME Sum/Count rate()'s own
// native lowering computes, rather than hand-deriving the rate
// extrapolation arithmetic here: the oracle independently lowers the bare
// `rate(m[5m])` HistogramProjection (already exercised by
// histogram_native_range_fn.go's own test suite) and reads its
// HistogramSum/HistogramCount columns directly.
func TestHistogramValueFnNestedRate_ChDB(t *testing.T) {
	const metric = "hvfn_nested_rate_exp_hist"
	seed := hvfnNestedRateDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + metric + "', map('job', 'a'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
		"    ('" + metric + "', map('job', 'a'), toDateTime64('2026-01-01 00:04:00', 9), 10, 20.0, 0, 0, 0, [30], 0, []);\n"
	fixture := newChDBFixture(t, seed)

	s := schema.DefaultOTelMetrics()
	evalTS := time.Date(2026, 1, 1, 0, 4, 0, 0, time.UTC)

	oracleSumQuery := "rate(" + metric + "[5m])"
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	oracleExpr, err := p.ParseExpr(oracleSumQuery)
	if err != nil {
		t.Fatalf("ParseExpr(oracle): %v", err)
	}
	oraclePlan, err := promql.LowerAt(context.Background(), oracleExpr, s, evalTS, evalTS)
	if err != nil {
		t.Fatalf("LowerAt(oracle): %v", err)
	}
	oracleSQL, oracleArgs, err := chsql.Emit(context.Background(), oraclePlan)
	if err != nil {
		t.Fatalf("Emit(oracle): %v", err)
	}
	oracleRows := fixture.queryOverEmitted(t, "`HistogramSum` AS sum, `HistogramCount` AS cnt", oracleSQL, oracleArgs)
	var oracleSum, oracleCount float64
	var oracleN int
	for oracleRows.Next() {
		if err := oracleRows.Scan(&oracleSum, &oracleCount); err != nil {
			t.Fatalf("scan oracle: %v", err)
		}
		oracleN++
	}
	if err := testsql.TolerantRowsErr(oracleRows.Err()); err != nil {
		t.Fatalf("oracle rows.Err: %v", err)
	}
	_ = oracleRows.Close()
	if oracleN != 1 {
		t.Fatalf("oracle rate() probe: got %d rows, want 1", oracleN)
	}
	if oracleSum == 0 || oracleCount == 0 {
		t.Fatalf("oracle values are degenerate zero — seed data does not exercise rate(): sum=%v count=%v", oracleSum, oracleCount)
	}

	gotSum := hvfnRunScalar(t, fixture, s, "histogram_sum(rate("+metric+"[5m]))", evalTS)
	gotCount := hvfnRunScalar(t, fixture, s, "histogram_count(rate("+metric+"[5m]))", evalTS)
	if gotSum != oracleSum {
		t.Errorf("histogram_sum(rate(...)) = %v, want %v (rate()'s own HistogramSum)", gotSum, oracleSum)
	}
	if gotCount != oracleCount {
		t.Errorf("histogram_count(rate(...)) = %v, want %v (rate()'s own HistogramCount)", gotCount, oracleCount)
	}
}

// TestHistogramQuantileNestedBinop_ChDB proves histogram_quantile(0.5, m1 +
// m2) — the issue's fourth trigger query — walks the merged bucket ladder
// the `+` binop produces. The oracle seeds a THIRD metric with the
// hand-computed merge result (Count/Sum/buckets summed elementwise, same
// Scale/offset so no scale-fold is needed) as a bare selector, and asserts
// histogram_quantile over the binop returns the identical value
// histogram_quantile over the already-merged bare selector does — reusing
// the extensively-tested bare-selector native quantile path as the oracle
// rather than hand-deriving the interpolation arithmetic here.
func TestHistogramQuantileNestedBinop_ChDB(t *testing.T) {
	const (
		metricA     = "hvfn_nested_quantile_a_exp_hist"
		metricB     = "hvfn_nested_quantile_b_exp_hist"
		premerged   = "hvfn_nested_quantile_merged_exp_hist"
		queryBinop  = "histogram_quantile(0.5, " + metricA + " + " + metricB + ")"
		queryOracle = "histogram_quantile(0.5, " + premerged + ")"
	)
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + metricA + "', map('svc', 'x'), toDateTime64('2026-01-01 00:01:00', 9), 10, 10.0, 0, 0, 0, [2,3,5], 0, []),\n" +
		"    ('" + metricB + "', map('svc', 'x'), toDateTime64('2026-01-01 00:01:00', 9), 6, 6.0, 0, 0, 0, [1,2,3], 0, []),\n" +
		"    ('" + premerged + "', map('svc', 'x'), toDateTime64('2026-01-01 00:01:00', 9), 16, 16.0, 0, 0, 0, [3,5,8], 0, []);\n"
	fixture := newChDBFixture(t, seed)

	s := schema.DefaultOTelMetrics()
	evalTS := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)

	gotBinop := hvfnRunScalar(t, fixture, s, queryBinop, evalTS)
	gotOracle := hvfnRunScalar(t, fixture, s, queryOracle, evalTS)
	if gotBinop != gotOracle {
		t.Fatalf("%s = %v, want %v (== %s over the hand-pre-merged distribution)", queryBinop, gotBinop, gotOracle, queryOracle)
	}
	// The bucket walk over [3,5,8] (16 observations) at phi=0.5 must land
	// strictly inside the populated range rather than degenerating to 0 —
	// pins the oracle itself against a silently-degenerate false-positive
	// equality (both sides empty/zero would also satisfy gotBinop==gotOracle).
	if gotBinop <= 0 {
		t.Fatalf("%s = %v, want a strictly positive quantile", queryBinop, gotBinop)
	}
}

// TestHistogramValueFnNestedSetOp_ChDB proves histogram_count(m1 and m2) —
// a boundary variant beyond the issue's own four named triggers, found
// while probing this gap's true extent — reads the LHS's own Count for a
// matched series. Reference Prometheus's `and` never inspects a matched
// sample's value, so the merged/reduced distribution to read is simply
// the LHS's own, unlike `+`/sum()/avg()/rate() which combine both sides.
func TestHistogramValueFnNestedSetOp_ChDB(t *testing.T) {
	const (
		metricA = "hvfn_nested_setop_a_exp_hist"
		metricB = "hvfn_nested_setop_b_exp_hist"
	)
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + metricA + "', map('svc', 'x'), toDateTime64('2026-01-01 00:01:00', 9), 9, 9.0, 0, 0, 0, [9], 0, []),\n" +
		"    ('" + metricB + "', map('svc', 'x'), toDateTime64('2026-01-01 00:01:00', 9), 100, 100.0, 0, 0, 0, [100], 0, []);\n"
	fixture := newChDBFixture(t, seed)

	s := schema.DefaultOTelMetrics()
	evalTS := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)

	got := hvfnRunScalar(t, fixture, s, "histogram_count("+metricA+" and "+metricB+")", evalTS)
	if got != 9 {
		t.Fatalf("histogram_count(a and b) = %v, want 9 (LHS's own Count, `and` never touches Value)", got)
	}
}

// TestHistogramValueFnNestedSumRange_ChDB proves the range-mode fan-out:
// two distinct grid anchors, each merging a distinct pair of samples,
// produce two DISTINCT correctly-summed totals rather than one value
// broadcast across the matrix or every step collapsing onto a single
// (wrong) timestamp — the regression this fix's Attributes+Timestamp
// GroupBy threading through lowerHistogramValueFnOverProjection /
// lowerHistogramQuantileNativeOverProjection guards against.
func TestHistogramValueFnNestedSumRange_ChDB(t *testing.T) {
	const metric = "hvfn_nested_sum_range_exp_hist"
	seed := subqHistDDL +
		"INSERT INTO otel_metrics_exponential_histogram " +
		"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
		"    ('" + metric + "', map('job', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 2, 2.0, 0, 0, 0, [2], 0, []),\n" +
		"    ('" + metric + "', map('job', 'b'), toDateTime64('2026-01-01 00:01:00', 9), 5, 5.0, 0, 0, 0, [5], 0, []),\n" +
		"    ('" + metric + "', map('job', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 10, 10.0, 0, 0, 0, [10], 0, []),\n" +
		"    ('" + metric + "', map('job', 'b'), toDateTime64('2026-01-01 00:02:00', 9), 20, 20.0, 0, 0, 0, [20], 0, []);\n"
	fixture := newChDBFixture(t, seed)

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)

	query := "histogram_count(sum(" + metric + "))"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	rows := fixture.queryOverEmitted(t, "toUnixTimestamp(`TimeUnix`) AS ts, `Value` AS val", sqlStr, args)
	defer func() { _ = rows.Close() }()
	got := map[int64]float64{}
	for rows.Next() {
		var ts int64
		var v float64
		if err := rows.Scan(&ts, &v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[ts] = v
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if want := 7.0; got[start.Unix()] != want {
		t.Errorf("%s: anchor %v = %v, want %v (2+5)", query, start, got[start.Unix()], want)
	}
	if want := 30.0; got[end.Unix()] != want {
		t.Errorf("%s: anchor %v = %v, want %v (10+20)", query, end, got[end.Unix()], want)
	}
}

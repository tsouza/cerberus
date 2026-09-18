//go:build chdb

// chDB-backed proof that lowerMixedVVAdditiveArithmetic's histogram,
// histogram merge (histogram_native_mixed_or_vector_arithmetic.go) — the
// THIRD call site of histogramBinopBucketWidthBudgetGuardExpr, alongside
// the default one-to-one path (histogram_binop_merge_bound_chdb_test.go)
// and the group_left()/group_right() path
// (histogram_binop_card_merge_bound_chdb_test.go) — closes cerberus issue
// #3558 the same way its two siblings do: a scale-divergent two-operand
// merge is compacted to a bounded width via
// [wrapExpHistogramMergeScaleRefinement], not rejected outright.
package promql_test

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/tsouza/cerberus/internal/promql"
)

const (
	mvvMergeBoundLHSHistMetric = "mvv_merge_bound_lhs_exp_hist"
	mvvMergeBoundRHSHistMetric = "mvv_merge_bound_rhs_exp_hist"
)

// mvvMergeBoundSeed seeds ONE "hh" series (both mixed-or operands resolve
// histogram) whose two histogram arms diverge enough in PositiveOffset (0
// vs 20,000, both Scale 0, one bucket each — the SAME divergence
// histogram_binop_merge_bound_chdb_test.go's own scale-divergence proof
// uses) that the natural merge would span 20,001 buckets.
func mvvMergeBoundSeed() string {
	return "" +
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
		"    ('" + mvvMergeBoundLHSHistMetric + "', map('series', 'hh'), toDateTime64('2026-01-01 00:00:00', 9), 1, 1.0, 0, 0, 0, [1], 0, []),\n" +
		"    ('" + mvvMergeBoundRHSHistMetric + "', map('series', 'hh'), toDateTime64('2026-01-01 00:00:00', 9), 1, 1.0, 0, 0, " + strconv.Itoa(mvvMergeBoundFarOffset) + ", [1], 0, []);\n"
}

// mvvMergeBoundFarOffset is the RHS arm's PositiveOffset: 20,000 buckets
// from the LHS arm's 0, so the natural merge spans 20,001.
const mvvMergeBoundFarOffset = 20000

// mvvMergeBoundQuery is the `(<lhs> or histogram_quantile(...)) + (<rhs>
// or histogram_quantile(...))` shape every case here lowers.
func mvvMergeBoundQuery() string {
	return fmt.Sprintf(
		"(%s or histogram_quantile(0.5, %s)) + (%s or histogram_quantile(0.5, %s))",
		mvvMergeBoundLHSHistMetric, mvvMergeBoundLHSHistMetric,
		mvvMergeBoundRHSHistMetric, mvvMergeBoundRHSHistMetric,
	)
}

// TestMixedVVAdditiveMergeBudget_ChDB_ScaleDivergenceCompactsRatherThanRejects
// proves cerberus issue #3558's fix reaches lowerMixedVVAdditiveArithmetic's
// histogram,histogram merge: a scale-divergent two-operand merge (natural
// width 20,001, crossing maxHistogramMergeCostUnits by many orders of
// magnitude) succeeds with a compacted, bounded-width result instead of
// being rejected outright.
func TestMixedVVAdditiveMergeBudget_ChDB_ScaleDivergenceCompactsRatherThanRejects(t *testing.T) {
	fixture := newChDBFixture(t, mvvMergeBoundSeed())

	got, err := readMergedHistogramShape(t, fixture, mvvMergeBoundQuery(), promql.LowerOpts{})
	if err != nil {
		t.Fatalf("a scale-divergent mixed-or vector-vector histogram merge must be compacted to a bounded width, not rejected: %v", err)
	}
	assertCompactedMerge(t, got, 0, mvvMergeBoundFarOffset+1, 2)
}

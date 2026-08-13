package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_avg.go lowers `avg [by/without] (<selector>)` over an
// OTel exponential (native) histogram — the fifth histogram-VALUED
// lowering, after the bare selector, `sum()` and `rate()`/`increase()`.
//
// Reference Prometheus answers `avg()` over native-histogram samples
// with a native histogram: promql's aggregation accumulates an
// incremental mean of the group's FloatHistograms and publishes it. The
// incremental form exists purely for floating-point accuracy over long
// groups — the VALUE it converges on is the group's summed distribution
// divided by the number of contributing samples, which is what this
// lowering computes directly.
//
// So `avg()` is `sum()` plus one division, and this file is deliberately
// only that division: [sumOrAvgOverExpHistogram] recognises both
// aggregations and [expHistogramGroupMerge] builds the identical merge
// for both, so the two can never drift apart in the arithmetic that
// reconciles bucket ladders. Sharing the merge is the whole point of the
// split — a second, parallel merge would be a second place for the
// scale-fold to go wrong.
//
// WHICH fields the division touches is the load-bearing detail, and it
// is not "all of them". Reference divides a FloatHistogram by a scalar
// in `FloatHistogram.Div`:
//
//	h.ZeroCount /= scalar
//	h.Count     /= scalar
//	h.Sum       /= scalar
//	for i := range h.PositiveBuckets { h.PositiveBuckets[i] /= scalar }
//	for i := range h.NegativeBuckets { h.NegativeBuckets[i] /= scalar }
//
// — the five COUNT-bearing fields, and nothing else. Scale, the zero
// threshold and both bucket offsets describe where the buckets SIT on
// the value axis, not how much fell into them; scaling any of them would
// silently move the distribution rather than average it. That is the
// most plausible way to ship a wrong answer here, because a plan that
// divided the offsets too would still publish thirteen well-typed
// columns and still decode — so [TestLower_ExpHistogram_AvgDividesOnlyTheCountFields]
// pins the split in both directions.
//
// The divisor is the group's SERIES count, counted at the same across-
// series stage that merges the ladders: each row reaching that stage is
// one series' single resolved sample (the per-series stage below it
// already collapsed each series to its newest in-window reading), so
// `count()` there is exactly reference's `group.groupCount` — the number
// of samples that went into the mean.
//
// Fractional bucket counts are the visible consequence and are correct:
// averaging three histograms whose bucket holds 1, 1 and 2 observations
// publishes 1.333. Issue #2037 pinned the nine histogram output columns
// as Float64 / Array(Float64) / Int32 precisely so a fractional ladder
// decodes end to end, which is why this lowering needs no decode-path
// work of its own.

// expHistogramGroupSeriesCountAlias is the across-series stage's member
// count — the divisor `avg()` scales the merged distribution by.
//
// It shares the `_hq_` prefix, and the collision argument, of the merge
// aliases in histogram_quantile.go: user labels cannot produce it.
const expHistogramGroupSeriesCountAlias = "_hq_group_series_count"

// expHistogramGroupSeriesCountAgg counts the rows entering the
// across-series merge, i.e. the number of series contributing a sample
// to the group.
//
// It is collected for `avg()` only — see [expHistogramGroupMergeAggs].
// `sum()` never reads it, and emitting it there would change a shipped
// lowering's SQL to carry a column nothing consumes.
func expHistogramGroupSeriesCountAgg() chplan.AggFunc {
	return chplan.AggFunc{
		Name:  "count",
		Args:  []chplan.Expr{},
		Alias: expHistogramGroupSeriesCountAlias,
	}
}

// expHistogramAvgScaleProjections divides the merged row's five
// count-bearing fields by the group's series count, leaving every
// position-bearing field untouched.
//
// It rewrites [expHistogramGroupMergeProjections]'s output rather than
// re-deriving it, so `avg()` publishes exactly the columns `sum()`
// publishes, in exactly the same order — the thirteen-column contract
// chplan.HistogramProjection and chclient's positional row cursor both
// depend on.
//
// The two count-bearing SHAPES need different arithmetic: Count, Sum and
// ZeroCount are scalars and divide directly, while the two bucket
// ladders are arrays and divide element-wise through `arrayMap`.
func expHistogramAvgScaleProjections(projs []chplan.Projection, s schema.Metrics) []chplan.Projection {
	scaled := make([]chplan.Projection, len(projs))
	for i, p := range projs {
		switch p.Alias {
		case s.CountColumn, s.SumColumn, s.ZeroCountColumn:
			scaled[i] = chplan.Projection{Expr: expHistogramAvgDivide(p.Expr), Alias: p.Alias}
		case s.PositiveBucketCountsColumn, s.NegativeBucketCountsColumn:
			scaled[i] = chplan.Projection{Expr: expHistogramAvgDivideLadder(p.Expr), Alias: p.Alias}
		default:
			// Scale, ZeroThreshold and both bucket offsets: position, not
			// count. Reference's Div leaves all four alone.
			scaled[i] = p
		}
	}
	return scaled
}

// expHistogramAvgDivide divides one scalar field by the group's series
// count.
func expHistogramAvgDivide(e chplan.Expr) chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpDiv,
		Left:  e,
		Right: &chplan.ColumnRef{Name: expHistogramGroupSeriesCountAlias},
	}
}

// expHistogramAvgDivideLadder divides every bucket of one merged ladder
// by the group's series count, element-wise.
//
// The merged ladder is already dense over the merged scale — the
// across-series merge emits one entry per index in the merged range — so
// an element-wise map is the whole of reference's
// `for i := range h.PositiveBuckets { h.PositiveBuckets[i] /= scalar }`.
func expHistogramAvgDivideLadder(e chplan.Expr) chplan.Expr {
	const paramBucket = "b"
	return &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramBucket},
				// BareIdent, not ColumnRef: a lambda parameter is bound by
				// the lambda, not read off a row, and every other
				// exp-histogram bucket expression in this package spells it
				// that way (see expHistogramBucketRowContribExpr).
				Body: expHistogramAvgDivide(&chplan.BareIdent{Name: paramBucket}),
			},
			e,
		},
	}
}

// expHistogramGroupIsAvg reports whether an aggregation this file's
// sibling recognised is the `avg` half of the pair.
func expHistogramGroupIsAvg(agg *parser.AggregateExpr) bool {
	return agg.Op == parser.AVG
}

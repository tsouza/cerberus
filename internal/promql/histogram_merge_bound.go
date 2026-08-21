package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
)

// histogram_merge_bound.go closes issue #2385.
//
// The across-SERIES exponential-histogram merge (expHistogramMergeBucketsExpr)
// renders an arrayMap over the MERGED bucket range for every contributing
// row's downscaled contribution, so its cost is proportional to
//
//	(merged bucket-range width) x (series contributing to the group)
//
// and both factors are DATA-DETERMINED: nothing in the plan or the emitted
// GROUP BY caps how many series can land in one (anchor, group) cell, and
// nothing caps how far two series' Scale/Offset can diverge before their
// merged bucket ladder spans a huge range. A production corpus hit exactly
// this shape 19 times with real ClickHouse MEMORY_LIMIT_EXCEEDED failures (up
// to 6.31 GiB on a 6 GiB cap) — see the issue for the full investigation,
// including the two contributors already ruled out (applyNativeHistogramAnalyzerFix)
// / already fixed (the redundant per-target-bucket mergedStart re-evaluation,
// #2267 — see expHistogramOverMergedBucketRangeExpr).
//
// Both factors are knowable only once ClickHouse has read the actual rows —
// the Go-side plan-time gates (requireSubquerySampleBudget's family, in
// internal/engine) work only for STATICALLY derivable quantities like a
// grid's anchor count, and series-per-group / merged-bucket-width are
// neither. So the bound has to be an emitted, ClickHouse-evaluated check:
// the same `throwIf(<cond>, <msg>) = 0` idiom duplicate_labelset_guard.go
// and info_fn.go already use for a plan-shape invariant only ClickHouse
// itself can verify.
//
// It is wired as a [chplan.Filter]'s WHERE predicate wrapping the merge
// Aggregate BEFORE [expHistogramMergeSortStage] resorts its groupArrays —
// see wrapExpHistogramMergeBudgetGuard — rather than as that Aggregate's
// Having, for two reasons:
//
//   - The merge Aggregate's GroupBy is EMPTY whenever the query carries no
//     `by`/`without` clause (`sum by()` collapses to one group — see
//     histogramAggGroupBy), and an empty-GroupBy Aggregate with
//     DropEmptyOnNoGroup renders as the two-layer `WHERE _cerb_n > 0` shape
//     that cannot ALSO carry a HAVING (see chplan.Aggregate.Having's doc and
//     emitAggregate's explicit ErrUnsupported). A Filter wrapping the
//     Aggregate's output composes identically with both the grouped and the
//     collapsed-to-one-group shapes.
//   - Placed BEFORE the sort stage, a group this guard rejects never pays
//     for expHistogramMergeSortStage's three arraySort calls either — no
//     reason to sort a group about to be refused.
//
// It is exactly as immune to ClickHouse's column-pruning analyzer as a
// HAVING clause would be: it reads real output columns
// (expHistogramMergeProjections / hqQuantileRankScalarMergeProjections
// already read every one of them back downstream), so none of them is ever
// dead code the analyzer could drop.
//
// Rejecting — not truncating the merge to the cap — matches the rest of the
// resource-bound family (requireCompareGridBudget's doc makes the same
// call): a silently partial histogram-quantile answer would be worse than an
// honest refusal, and every peer bound already rejects rather than
// truncates.

const (
	// maxHistogramMergeSeriesPerGroup caps how many series' distributions may
	// be merged into one native-histogram (anchor, group) cell. Each
	// contributing row adds one full pass of the merged bucket range inside
	// expHistogramMergeBucketsExpr's row-sum, so this axis multiplies
	// directly against maxHistogramMergeBucketWidth. 4096 is generous for any
	// legitimate `by(<label>)` cardinality within a single anchor cell while
	// still refusing a pathological high-cardinality collapse before
	// ClickHouse allocates for it.
	maxHistogramMergeSeriesPerGroup = 4096

	// maxHistogramMergeBucketWidth caps the merged bucket ladder's width —
	// mergedEnd-mergedStart+1 after every contributing row's offset is
	// downscaled to the group's coarsest Scale — for EITHER signed ladder
	// (Positive/Negative). A single well-formed exponential histogram from an
	// OTel SDK default configuration carries at most a few hundred buckets;
	// 16384 leaves generous headroom for legitimately wide or high-scale
	// distributions merging together while still refusing the shape that
	// produced #2385's OOMs — two series whose Offset/Scale diverge enough
	// that the merged range spans a huge, mostly-empty span.
	maxHistogramMergeBucketWidth = 16384
)

// mergedLengthExpr returns a FULLY BOUND rendering of one signed ladder's
// merged bucket-range length, reusing expHistogramOverMergedBucketRangeExpr
// — the SAME hqLet-bound helper the real merge site
// (expHistogramMergeBucketsExpr) reuses — rather than calling
// expHistogramMergeBucketsBoundsExpr directly. Calling the bounds helper
// raw would render its own, UNBOUND copy of the `arrayMin(arrayMap(...))`
// merged-start subtree — exactly the cerberus issue #2267 hazard
// expHistogramOverMergedBucketRangeExpr's own doc describes, and exactly
// what TestExpHistogramMergedStartIsBoundOncePerSite pins against: every
// render of that subtree must be one of that helper's own hqLet bindings.
// Going through it here keeps the guard from becoming a second, unbound
// occurrence of the very hazard the merge site was already fixed for.
//
// Reused by [histogramBinopBucketWidthBudgetGuardExpr]
// (histogram_native_binop.go, cerberus issue #2428): the two-operand
// binop merge (histogramBinopMergedBucketsExpr) renders the identical
// unbounded arrayMap-over-mergedLength shape this file's own
// [histogramMergeBudgetGuardExpr] bounds for the cross-series merge, so
// the same fully-bound length rendering closes that narrower instance of
// the same bug class without a second, divergent implementation.
func mergedLengthExpr(scalesArr, offArr, bucArr, mergedScale chplan.Expr) chplan.Expr {
	return expHistogramOverMergedBucketRangeExpr(
		scalesArr, offArr, bucArr, mergedScale,
		func(_, mergedLength chplan.Expr) chplan.Expr { return mergedLength },
	)
}

// histogramMergeBudgetGuardExpr renders the throwIf(...) = 0 predicate that
// bounds the shared across-series exponential-histogram merge. It reads the
// SAME groupArray aliases expHistogramMergeAggs collects
// (hqAggScalesArrayAlias and the four offset/bucket array aliases), so it
// must be attached directly above the Aggregate that produces them — see
// wrapExpHistogramMergeBudgetGuard.
//
// Both width calls (positive and negative ladder) are cheap —
// O(series-per-group), an arrayMin/arrayMax over the same short groupArray
// this guard already reads length() of — the expensive part they gate is
// the O(width x series) row-sum in expHistogramMergeBucketsExpr, never
// reached for a group this guard rejects.
func histogramMergeBudgetGuardExpr() chplan.Expr {
	scalesArr := &chplan.ColumnRef{Name: hqAggScalesArrayAlias}
	mergedScale := &chplan.ColumnRef{Name: hqAggMergedScaleAlias}
	posOff := &chplan.ColumnRef{Name: hqAggPosOffsetsArrayAlias}
	posBuc := &chplan.ColumnRef{Name: hqAggPosBucketsArrayAlias}
	negOff := &chplan.ColumnRef{Name: hqAggNegOffsetsArrayAlias}
	negBuc := &chplan.ColumnRef{Name: hqAggNegBucketsArrayAlias}

	seriesCount := &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{scalesArr}}

	overBudget := orExpr(
		gtLit(seriesCount, maxHistogramMergeSeriesPerGroup),
		orExpr(
			gtLit(mergedLengthExpr(scalesArr, posOff, posBuc, mergedScale), maxHistogramMergeBucketWidth),
			gtLit(mergedLengthExpr(scalesArr, negOff, negBuc, mergedScale), maxHistogramMergeBucketWidth),
		),
	)

	return &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.FuncCall{
			Fn: chplan.FnThrowIf,
			Args: []chplan.Expr{
				overBudget,
				&chplan.InlineString{V: chplan.HistogramMergeBudgetMessage},
			},
		},
		Right: &chplan.LitInt{V: 0},
	}
}

// wrapExpHistogramMergeBudgetGuard wraps merged — the raw across-series
// exponential-histogram merge [chplan.Aggregate] built from
// [expHistogramMergeAggs] plus [expHistogramMergeSeriesOrderKeyAgg] — in the
// budget guard's Filter. [expHistogramMergeSortStage] calls this on every
// Aggregate it receives BEFORE resorting its groupArrays, and all four of
// that function's callers (histogram_quantile.go, histogram_quantile_range.go,
// and the two in histogram_native_sum.go — instant quantile, range quantile,
// and the histogram-VALUED sum() publishers) already route through it, so
// the guard is wired into every across-series merge shape from this one
// choke point rather than needing to be added at each call site.
func wrapExpHistogramMergeBudgetGuard(merged chplan.Node) chplan.Node {
	return &chplan.Filter{Input: merged, Predicate: histogramMergeBudgetGuardExpr()}
}

// orExpr returns `a OR b`. Mirrors andExpr, this package's existing
// conjunction helper.
func orExpr(a, b chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpOr, Left: a, Right: b}
}

// gtLit returns `expr > n`.
func gtLit(expr chplan.Expr, n int) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpGt, Left: expr, Right: &chplan.LitInt{V: int64(n)}}
}

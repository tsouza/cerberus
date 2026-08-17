package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_binop.go lowers binary arithmetic between two
// histogram-VALUED shapes — `<exp-hist shape> (+|-) <exp-hist shape>` —
// into a histogram-VALUED result (cerberus issue #2263), and drops the
// sample entirely for the incompatible-type op family (cerberus issue
// #2277).
//
// Reference Prometheus answers `+` between two native-histogram samples
// with FloatHistogram.Add: bucket-wise addition after reconciling the
// two operands' scales to the coarser of the two
// (promql/engine.go's vectorElemBinop, the `hlhs != nil && hrhs != nil`
// case). `-` is FloatHistogram.Sub, which upstream documents as "works
// like Add but subtracts the other histogram" — the identical
// reconciliation, with the second operand's counts negated first.
//
// Every other histogram/histogram binary op except `==`/`!=` — `*`, `/`,
// `^`, `%`, `atan2`, and every comparison but `==`/`!=` — is rejected by
// reference Prometheus itself (NewIncompatibleTypesInBinOpInfo, the same
// info-annotation drop [expHistogramDroppingScalarBinop] recognises for
// the scalar/histogram case): reference answers HTTP 200 with an empty
// result plus a warning annotation, not a query-aborting error. Cerberus
// used to hard-error the same shape at expHistogramSelectorRouting
// (lower.go); [expHistogramDroppingHistogramBinop] /
// [lowerExpHistogramDroppingHistogramBinop] below intercept it earlier
// and answer empty instead, matching reference (#2277). `==`/`!=`
// between two histograms ARE answered by reference with a KEPT result (a
// structural-equality filter, not a drop and not a merge), need
// different mechanics than either path in this file, and are tracked
// separately (#2273) — along with on()/ignoring()/group_left()/
// group_right() support for `+`/`-` (see this file's rejection error
// below).
//
// The bucket RECONCILIATION (which absolute index each operand's own
// buckets land on at the merged, coarser Scale) is the same integer
// index-alignment math [expHistogramMergeBucketsBoundsExpr] /
// [expHistogramMergeOffsetExpr] already implement for the cross-SERIES
// sum() merge (histogram_native_sum.go) and the histogram_quantile()
// cross-series fold (histogram_quantile_native_window.go), reused here
// unchanged — it is purely structural (bitShiftRight / arrayMin /
// arrayMax over Scale and Offset), independent of how the VALUES at
// each resolved index get folded, so sharing it cannot let a binary op
// between two histograms and `sum()` drift apart on WHERE a bucket ends
// up.
//
// The VALUE fold is deliberately NOT shared with that merge. sum() /
// avg() / histogram_quantile() answer a cross-SERIES GROUP, and
// reference Prometheus's own aggregation evaluator folds that group
// with FloatHistogram.KahanAdd — a Neumaier-compensated recurrence
// (promql/engine.go's `sum`/`avg` aggregation path calls
// `group.histogramValue.KahanAdd(h, group.histogramKahanC)`) — which is why
// [expHistogramMergeBucketsExpr] / [promHistogramKahanSum] exist and why
// [histogram_quantile.go]'s own within-row downscale fold routes through
// that same compensated recurrence: upstream's KahanAdd downscales via
// `kahanReduceResolution`, itself compensated.
//
// The V-V binary operator this file answers is a DIFFERENT reference
// method: `vectorElemBinop`'s `hlhs != nil && hrhs != nil` case calls
// the PLAIN `hlhs.Copy().Add(hrhs)` / `.Sub(hrhs)` — model/histogram/
// float_histogram.go's `Add`/`Sub` do `h.Sum += other.Sum`,
// `targetBuckets[i] += originBuckets[...]` with NO compensation
// anywhere, including their OWN within-row downscale fold
// (`mustReduceResolution` → `reduceResolution`, a plain `+=` loop —
// contrast `KahanAdd`'s `kahanReduceResolution`). Reusing the Kahan
// merge here would silently diverge from reference at the ULP level
// for any two-histogram `+`/`-` whose Count/Sum/bucket values do not
// happen to add up exactly in float64 (which plain integer/round test
// fixtures cannot surface, since Kahan compensation is a no-op exactly
// when there is no rounding error to correct). The two-operand fold
// below ([histogramBinopMergeProjections]) is therefore a SEPARATE,
// plain implementation — see its doc for the specific functions.
//
// Lowering shape (instant mode):
//
//	HistogramProjection [MetricName='', Attributes, now64(9), Value=0, <nine histogram columns>]
//	  Project [Attributes (rebuilt from the shared match key), merged
//	           Scale / ZeroCount / ZeroThreshold / {Pos,Neg}{Offset,BucketCounts},
//	           Count, Sum]
//	    Aggregate groupBy=[Attributes] having=count()=2
//	              funcs=<histogramBinopMergeAggs>
//	      UnionAll
//	        <lhs HistogramProjection>
//	        <rhs HistogramProjection, negated for `-`>
//
// The `having count() = 2` clause stands in for VectorJoin's INNER JOIN:
// default vector matching (no on()/ignoring()/group_left()/group_right())
// keys on the full Attributes map — the same map each operand's own
// HistogramProjection is already grouped by — so a matching key with
// fewer than two contributing rows means the series existed on only one
// side, which PromQL's default one-to-one V-V matching drops. Two rows
// sharing a key on the SAME side cannot happen (each operand's own
// grouping already guarantees one row per Attributes value), so
// `!= 2` can only mean "unmatched", never "ambiguous" — unlike
// VectorJoin's on()/ignoring() case, no runtime many-to-many guard is
// needed here.
func expHistogramHistogramBinop(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (lhs, rhs parser.Expr, sub bool, vm *parser.VectorMatching, ok bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, nil, false, nil, false
	}
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || (b.Op != parser.ADD && b.Op != parser.SUB) {
		return nil, nil, false, nil, false
	}
	if !isExpHistogramValuedShape(b.LHS, s, ctx) || !isExpHistogramValuedShape(b.RHS, s, ctx) {
		return nil, nil, false, nil, false
	}
	return b.LHS, b.RHS, b.Op == parser.SUB, b.VectorMatching, true
}

// lowerExpHistogramHistogramBinop lowers the shape
// [expHistogramHistogramBinop] recognised. Both operands defer to their
// own existing histogram-valued lowering unchanged — this function only
// adds the join + merge on top.
func lowerExpHistogramHistogramBinop(lhsExpr, rhsExpr parser.Expr, sub bool, vm *parser.VectorMatching, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if !isDefaultMatching(vm) {
		return nil, fmt.Errorf(
			"promql: on()/ignoring()/group_left()/group_right() matching is not yet supported for binary " +
				"arithmetic between two exponential histogram selectors; only default (full label set) matching is supported",
		)
	}
	hpL, err := lowerExpHistogramValuedOperand(lhsExpr, s, ctx)
	if err != nil {
		return nil, err
	}
	hpR, err := lowerExpHistogramValuedOperand(rhsExpr, s, ctx)
	if err != nil {
		return nil, err
	}
	if sub {
		// `h1 - h2` is FloatHistogram.Sub, which reference documents as
		// Add with the second operand's counts negated first — reuse the
		// existing scalar-scaling machinery (issue #2087) to negate every
		// count-bearing field (Count, Sum, ZeroCount, both bucket
		// ladders) while leaving Scale/ZeroThreshold/offsets untouched,
		// then fold through the same merge as `+`.
		hpR = scaleHistogramProjection(hpR, chplan.OpMul, &chplan.LitFloat{V: -1}, s)
	}
	return mergeTwoHistogramProjections(hpL, hpR, s, ctx), nil
}

// lowerExpHistogramValuedOperand lowers one binop operand through its own
// histogram-valued recogniser and asserts the shared HistogramProjection
// cap every such lowering publishes.
func lowerExpHistogramValuedOperand(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*chplan.HistogramProjection, error) {
	node, matched, err := lowerExpHistogramValuedShape(expr, s, ctx)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, fmt.Errorf("promql: internal invariant violated: exp-histogram binop operand matched no known histogram-valued shape for %v", expr)
	}
	hp, ok := node.(*chplan.HistogramProjection)
	if !ok {
		return nil, fmt.Errorf("promql: internal invariant violated: exp-histogram binop operand lowering did not cap with *chplan.HistogramProjection, got %T", node)
	}
	return hp, nil
}

// expHistogramDroppingHistogramBinop recognises `<exp-hist shape> OP
// <exp-hist shape>` for the op family reference Prometheus answers with
// NewIncompatibleTypesInBinOpInfo when BOTH operands carry a histogram —
// MUL/DIV/POW/MOD, every comparison except EQLC/NEQ, and ATAN2 (cerberus
// issue #2277). Unlike [expHistogramHistogramBinop]'s +/- merge, the
// result is empty regardless of vector matching mode: every matched pair
// drops (keep=false) for this op family, so an empty output holds
// whether the matched pair set came from DEFAULT one-to-one matching or
// an explicit on()/ignoring()/group_left()/group_right() clause — there
// is no observable difference between them left to validate, unlike
// +/-'s explicit "not yet supported" rejection for anything but default
// matching.
//
// EQLC/NEQ are excluded: reference answers those with a KEPT result (a
// structural-equality filter), not a drop — different mechanics, tracked
// separately by #2273.
func expHistogramDroppingHistogramBinop(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (lhs, rhs parser.Expr, ok bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return nil, nil, false
	}
	b, isBin := unwrapBinaryExpr(expr)
	if !isBin || !expHistogramHistogramBinopDrops(b.Op) {
		return nil, nil, false
	}
	if !isExpHistogramValuedShape(b.LHS, s, ctx) || !isExpHistogramValuedShape(b.RHS, s, ctx) {
		return nil, nil, false
	}
	return b.LHS, b.RHS, true
}

// expHistogramHistogramBinopDrops reports whether op is one of reference's
// NewIncompatibleTypesInBinOpInfo shapes when both binop operands carry a
// histogram — the complement of [expHistogramHistogramBinop]'s ADD/SUB
// merge and [expHistogramDroppingHistogramBinop]'s excluded EQLC/NEQ.
func expHistogramHistogramBinopDrops(op parser.ItemType) bool {
	switch op {
	case parser.MUL, parser.DIV, parser.POW, parser.MOD,
		parser.GTR, parser.LSS, parser.GTE, parser.LTE, parser.ATAN2:
		return true
	default:
		return false
	}
}

// lowerExpHistogramDroppingHistogramBinop lowers the shape
// [expHistogramDroppingHistogramBinop] recognised. Both operands are
// lowered through their own existing histogram-valued recognisers first —
// unmodified, exactly as [lowerExpHistogramHistogramBinop] does for the
// merge shape — so a nested lowering error in either operand still
// surfaces normally rather than being silently swallowed by the drop.
// Only the LHS's HistogramProjection caps the resulting constant-false
// Filter; which side is chosen is arbitrary; since every row is filtered
// out, cerberus never uses the RHS lowering's rows, but the RHS lowering
// still runs so its own errors are still caught before the drop applies.
func lowerExpHistogramDroppingHistogramBinop(lhsExpr, rhsExpr parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	hpL, err := lowerExpHistogramValuedOperand(lhsExpr, s, ctx)
	if err != nil {
		return nil, err
	}
	if _, err := lowerExpHistogramValuedOperand(rhsExpr, s, ctx); err != nil {
		return nil, err
	}
	// Reference returns keep=false plus an incompatible-types info
	// annotation. Cerberus has no info-annotation wire channel, but
	// matches the accepted empty result — the same Filter{Predicate:
	// LitBool{false}} shape [lowerExpHistogramScalarBinop]'s drop path
	// uses for the scalar/histogram case.
	return &chplan.Filter{Input: hpL, Predicate: &chplan.LitBool{V: false}}, nil
}

// mergeTwoHistogramProjections adds hpL and hpR's distributions,
// series-by-series, via the same scale-fold + offset-align + zero-pad
// reconciliation the cross-series merges apply. See this file's doc for
// why the join collapses to a `having count() = 2` guard rather than a
// VectorJoin.
func mergeTwoHistogramProjections(hpL, hpR *chplan.HistogramProjection, s schema.Metrics, ctx lowerCtx) chplan.Node {
	histSchema := histogramProjectionSchema(s)
	groupBy, groupByAliases, attrsRebuild := histogramAggGroupBy(
		nil, &chplan.ColumnRef{Name: histSchema.AttributesColumn}, histSchema,
	)

	stepAligned := ctx.step > 0
	var projs []chplan.Projection
	if stepAligned {
		// Range mode: both operands' HistogramProjection already
		// publishes the per-step grid anchor under the canonical
		// TimestampColumn (see nativeHistogramProjection), so grouping
		// by it keeps each anchor's merge independent — the same
		// prepend [expHistogramGroupMerge] applies for its own anchor
		// parameter.
		anchor := &chplan.ColumnRef{Name: histSchema.TimestampColumn}
		groupBy = append([]chplan.Expr{anchor}, groupBy...)
		groupByAliases = append([]string{histSchema.TimestampColumn}, groupByAliases...)
		projs = append(projs, chplan.Projection{Expr: anchor, Alias: histSchema.TimestampColumn})
	}

	merged := &chplan.Aggregate{
		Input:              &chplan.UnionAll{Inputs: []chplan.Node{hpL, hpR}},
		GroupBy:            groupBy,
		GroupByAliases:     groupByAliases,
		AggFuncs:           histogramBinopMergeAggs(histSchema),
		Having:             histogramBinopBothSidesMatchedGuard(),
		DropEmptyOnNoGroup: true,
	}
	projs = append(projs, chplan.Projection{Expr: attrsRebuild, Alias: histSchema.AttributesColumn})
	projs = append(projs, histogramBinopMergeProjections(histSchema)...)
	reshaped := &chplan.Project{Input: merged, Projections: projs}

	tsExpr := chplan.Expr(chplan.NowNano())
	if stepAligned {
		tsExpr = &chplan.ColumnRef{Name: histSchema.TimestampColumn}
	}
	return nativeHistogramProjection(reshaped, &chplan.LitString{V: ""}, tsExpr, histSchema)
}

// histogramBinopBothSidesMatchedGuard renders `count() = 2` — see this
// file's doc comment for why that alone implements default one-to-one
// V-V matching's INNER JOIN semantics here.
func histogramBinopBothSidesMatchedGuard() chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.FuncCall{Fn: chplan.FnCount},
		Right: &chplan.LitInt{V: 2},
	}
}

// histogramBinopMergeAggs collects the group's Count / Sum arrays plus
// the same Scale (min) / ZeroCount / ZeroThreshold (max) / signed-bucket
// (Offset + BucketCounts, both keyed by Scale) groupArrays
// [expHistogramMergeAggs] collects for the cross-series merge — the
// COLLECTION shape is identical (groupArray has no notion of
// compensation; only the FOLD below does), and [histogramBinopMergeAggs]
// / [histogramBinopMergeProjections]'s Having guard guarantees each
// array holds exactly the two matched operands' own values.
func histogramBinopMergeAggs(s schema.Metrics) []chplan.AggFunc {
	return append([]chplan.AggFunc{
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.CountColumn}}, Alias: hqMergeCountsArrayAlias},
		{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.SumColumn}}, Alias: hqMergeSumsArrayAlias},
	}, expHistogramMergeAggs(s)...)
}

// histogramBinopMergeProjections folds the two-element groupArrays
// [histogramBinopMergeAggs] collected into the merged histogram row,
// using PLAIN (uncompensated) arithmetic throughout — matching
// reference Prometheus's `Add` / `Sub` (see this file's header doc for
// why the Kahan-compensated [expHistogramMergeProjections] the
// cross-series merge uses is the WRONG fold here). Scale / ZeroThreshold
// / both Offsets are structural (bitShiftRight / min / max index
// arithmetic, no float-summation policy involved) and are reused from
// [expHistogramMergeOffsetExpr] unchanged; only the fields that fold
// floats — Count, Sum, ZeroCount and both bucket ladders — get the
// plain implementation.
func histogramBinopMergeProjections(s schema.Metrics) []chplan.Projection {
	projs := []chplan.Projection{
		{Expr: plainArraySum(&chplan.ColumnRef{Name: hqMergeCountsArrayAlias}), Alias: s.CountColumn},
		{Expr: plainArraySum(&chplan.ColumnRef{Name: hqMergeSumsArrayAlias}), Alias: s.SumColumn},
		{Expr: &chplan.ColumnRef{Name: hqAggMergedScaleAlias}, Alias: s.ScaleColumn},
		{Expr: plainArraySum(&chplan.ColumnRef{Name: hqMergeZeroCountsArrayAlias}), Alias: s.ZeroCountColumn},
	}
	if s.ZeroThresholdColumn != "" {
		projs = append(projs, chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: s.ZeroThresholdColumn},
			Alias: s.ZeroThresholdColumn,
		})
	}
	return append(projs, []chplan.Projection{
		{Expr: expHistogramMergeOffsetExpr(hqAggPosOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.PositiveOffsetColumn},
		{Expr: histogramBinopMergedBucketsExpr(hqAggPosOffsetsArrayAlias, hqAggPosBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.PositiveBucketCountsColumn},
		{Expr: expHistogramMergeOffsetExpr(hqAggNegOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.NegativeOffsetColumn},
		{Expr: histogramBinopMergedBucketsExpr(hqAggNegOffsetsArrayAlias, hqAggNegBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.NegativeBucketCountsColumn},
	}...)
}

// plainArraySum renders CH's `arraySum(<values>)` — a single plain
// accumulation with no Kahan/Neumaier compensation. Over the exactly
// two-element arrays [histogramBinopMergeAggs] collects (guaranteed by
// [histogramBinopBothSidesMatchedGuard]'s `count() = 2` Having), this
// computes exactly one floating-point addition — bit-identical to
// reference's plain `h.Sum += other.Sum` — regardless of which order
// groupArray happened to collect the two rows in, since IEEE754
// addition of exactly two operands is commutative (unlike Kahan-
// Neumaier compensated summation, which is NOT bit-identical to a plain
// add even for two terms — see this file's header doc).
func plainArraySum(values chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnArraySum, Args: []chplan.Expr{values}}
}

// histogramBinopMergedBucketsExpr renders the merged PositiveBucketCounts
// (or NegativeBucketCounts) for the binop's two-element groupArrays,
// mirroring [expHistogramMergeBucketsExpr]'s structure (same bounds via
// [expHistogramMergeBucketsBoundsExpr], same per-row index-matching
// picker via [expHistogramRowContribsExpr]) but folding with
// [plainArraySum] via [histogramBinopBucketRowContribExpr] instead of
// the Kahan-compensated arrayReduce('sumKahan', ...) — see this file's
// header doc for why. There is no series-order sort here (contrast
// [expHistogramMergeBucketsExpr]'s [expHistogramSortRowsByKeyExpr] calls,
// needed only because Kahan-Neumaier summation is order-sensitive even
// once the same set of terms is fixed): a plain add of exactly two
// terms is order-invariant, so which of the two groupArray positions is
// the caller's LHS or RHS never affects the result.
func histogramBinopMergedBucketsExpr(offArrAlias, bucArrAlias, scalesArrAlias, mergedScaleAlias string) chplan.Expr {
	mergedScale := &chplan.ColumnRef{Name: mergedScaleAlias}
	scalesArr := &chplan.ColumnRef{Name: scalesArrAlias}
	offArr := &chplan.ColumnRef{Name: offArrAlias}
	bucArr := &chplan.ColumnRef{Name: bucArrAlias}

	mergedStart, mergedLength := expHistogramMergeBucketsBoundsExpr(scalesArr, offArr, bucArr, mergedScale)

	const paramT = "t"
	rowContribs := expHistogramRowContribsExpr(
		scalesArr, offArr, bucArr,
		histogramBinopBucketRowContribExpr(mergedScale, mergedStart, paramT),
	)
	rowsSum := plainArraySum(rowContribs)

	return &chplan.FuncCall{
		Fn: chplan.FnArrayMap,
		Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{paramT}, Body: rowsSum},
			&chplan.FuncCall{
				Fn: chplan.FnRange,
				Args: []chplan.Expr{
					&chplan.FuncCall{Fn: chplan.FnToUInt64, Args: []chplan.Expr{mergedLength}},
				},
			},
		},
	}
}

// histogramBinopBucketRowContribExpr is [expHistogramBucketRowContribExpr]
// with its within-row downscale fold changed from the Kahan-compensated
// [promHistogramKahanSum] to [plainArraySum] — matching reference's own
// split between `KahanAdd`'s compensated `kahanReduceResolution` and
// `Add`/`Sub`'s plain `reduceResolution` (see this file's header doc).
// The index-matching picker itself is shared verbatim via
// [expHistogramBucketPositionPickerExpr] — only the fold wrapping it
// differs between the two callers.
func histogramBinopBucketRowContribExpr(mergedScale, mergedStart chplan.Expr, paramT string) chplan.Expr {
	return plainArraySum(expHistogramBucketPositionPickerExpr(mergedScale, mergedStart, paramT))
}

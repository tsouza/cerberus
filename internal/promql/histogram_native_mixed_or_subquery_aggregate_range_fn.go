package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_subquery_aggregate_range_fn.go composes a
// SELECT/FOLD-family outer function over a subquery whose own inner is
// `sum`/`avg` [by/without] wrapping a mixed float/histogram `or` —
// `<fn>((sum by (series) ((a) or (b)))[range:step])` — cerberus issue
// #2581's own remaining gap, the one
// [wrapMixedOrSubqueryInner]'s doc names as deliberately unattempted by
// this file's sibling (histogram_native_mixed_or_subquery_range_fn.go)
// because naively DISTRIBUTING the outer function per `or` ARM and
// recombining does not reproduce reference's sum/avg drop-on-collision
// semantics (a `by`/`without` clause can put rows from BOTH arms into the
// SAME output group, and reference drops that group entirely rather than
// picking a side).
//
// # Why this composes soundly after all
//
// Two independent findings, both verified directly against this repo and
// against the vendored tsouza/prometheus fork, make a SOUND composition
// possible without touching the "distribute per arm" idea at all:
//
//  1. Reference Prometheus's `sum`/`avg` drop-on-collision rule is ALREADY
//     correctly implemented in this codebase — [combineMixedAggregateBranches]
//     (histogram_native_mixed_or_aggregate.go, cerberus issue #2346),
//     reduces the histogram arm and the float arm SEPARATELY and combines
//     them with a drop-on-group-collision rule (two UNLESSes + a Mixed
//     `or`), not a naive per-arm distribute. And it is ALREADY reachable at
//     a subquery's own per-anchor grid: [lowerSubquery] tries
//     [lowerHistogramNativeSubqueryInner] first, which runs
//     [lowerHistogramNativeRoot] — [sumOrAvgOverMixedExpHistogramSetOp] /
//     [lowerSumOrAvgOverMixedExpHistogramSetOp] included — against the
//     subquery's own inner expression at the subquery's own grid
//     ([subqueryGridCtx]). So `sub.Expr` in
//     `<fn>((sum by (series) ((a) or (b)))[range:step])` ALREADY lowers to
//     a genuinely collision-correct [chplan.MixedRowShape] relation, one
//     row per (output group, subquery anchor) — [lowerOuterRangeFnOverSubquery]
//     just unconditionally REJECTS a [chplan.MixedRowShape] `inner` today
//     (its own histogramSubqueryFloatOnlyDropFunc exception aside), which
//     is the actual, narrower gap this file closes.
//
//  2. Reference's own SELECT/FOLD-family window reducers (tsouza/prometheus's
//     promql/functions.go) already define exactly what to do with a
//     per-anchor relation whose TYPE can differ from one subquery anchor to
//     the next for the SAME output group — which is exactly the shape the
//     per-anchor Mixed relation above can produce, because a `by`/`without`
//     clause's grouping can pick up float rows at one anchor and histogram
//     rows at another for what reference treats as one output series:
//
//     - The nine names whose result preserves the operand's own value type
//     (rate/increase/delta/irate/idelta/sum_over_time/avg_over_time,
//     minus last_over_time/first_over_time — see the scope note below)
//     check `len(samples.Histograms) > 0 && len(samples.Floats) > 0` for
//     the WHOLE window and DROP the series entirely
//     (NewMixedFloatsHistogramsWarning) when both are present —
//     extrapolatedRate (rate/increase/delta/irate/idelta) and
//     funcSumOverTime/funcAvgOverTime all do this identical check. This is
//     the SAME "drop on collision" shape [combineMixedAggregateBranches]
//     already implements one level down, just keyed by (output group)
//     across the WHOLE window instead of (output group, one anchor).
//     [windowPurityUnless] below reproduces it with the identical
//     UNLESS-pair idiom, StepAligned forced false so the match spans every
//     subquery anchor in the window rather than one.
//     - count_over_time / present_over_time / ts_of_first_over_time /
//     ts_of_last_over_time read no per-sample value at all beyond its
//     existence or its timestamp (funcCountOverTime/funcPresentOverTime/
//     funcTsOfFirstOverTime/funcTsOfLastOverTime never branch on
//     samples.Histograms vs samples.Floats) — no window-purity test
//     applies; they consume the already-correct per-anchor Mixed relation
//     directly.
//
// # Scope
//
// Composes for thirteen of the fifteen SELECT/FOLD-family names:
// count_over_time, present_over_time, ts_of_first_over_time,
// ts_of_last_over_time (type-blind, [lowerSumOrAvgMixedOrSubquerySelectFn]),
// rate, increase, delta, irate, idelta, sum_over_time, avg_over_time
// (window-purity-filtered, [lowerSumOrAvgMixedOrSubqueryFoldFn]), and
// resets, changes (type-aware sequential merge, cerberus issue #2615 —
// see histogram_native_mixed_or_subquery_resets_changes.go's own doc for
// why that pair needs its own machinery rather than either of the two
// composers above, and why it reaches all three grid modes where the
// seven-name FOLD family only reaches two).
//
// Two names deliberately stay on the pre-existing rejection:
//
//   - last_over_time / first_over_time select ONE raw sample verbatim
//     (whichever type it happens to be) rather than folding a window, so
//     they need a MIXED-shaped (not Histogram-shaped) "pick the newest/
//     oldest row" reduction that also carries the discriminator and Value
//     columns through the same argMax/argMin selection
//     ([nativeExpHistBareAggsDirectional] only carries the nine histogram
//     columns today) — real new machinery, not a two-line extension.
//     Tracked as cerberus issue #2714.
//
// The FOLD family's window-purity test ([windowPurityUnless]) is sound
// today only for a SINGLE window per output series — an instant query, or
// an `@`-pinned subquery broadcast across a query_range grid
// ([sumOrAvgMixedOrSubqueryOuterFn]'s own gate excludes true query_range
// fan-out for these seven names): a genuine fan-out evaluates each output
// step's own [range] window independently, and the purity test would need
// to be scoped PER OUTER ANCHOR rather than once across the whole subquery
// grid — real new machinery ([chplan.RangeBucketFanout] has no primitive
// for a cross-relation per-window EXISTS test today), not a limitation of
// the approach itself. Tracked as cerberus issue #2715 — note this file's
// own resets/changes sibling reaches true fan-out just fine, precisely
// because it needs no window-purity test at all (see that file's own
// top-level doc).
func sumOrAvgMixedOrSubqueryOuterFnRecognized(c *parser.Call, s schema.Metrics, ctx lowerCtx) (sumOrAvgMixedOrSubqueryShape, bool) {
	if s.ExpHistogramTable == "" || ctx.metadataFullRange {
		return sumOrAvgMixedOrSubqueryShape{}, false
	}
	if len(c.Args) != 1 {
		return sumOrAvgMixedOrSubqueryShape{}, false
	}
	sub, ok := peelWrappers(c.Args[0]).(*parser.SubqueryExpr)
	if !ok || sub.Range <= 0 || !subqueryHasEvalAnchor(sub, ctx) {
		return sumOrAvgMixedOrSubqueryShape{}, false
	}
	agg, b, ok := sumOrAvgOverMixedExpHistogramSetOp(sub.Expr, s, ctx)
	if !ok {
		return sumOrAvgMixedOrSubqueryShape{}, false
	}
	switch c.Func.Name {
	case countOverTimeWindowFn, presentOverTimeWindowFn, tsOfFirstOverTimeExpHistFn, tsOfLastOverTimeExpHistFn:
		return sumOrAvgMixedOrSubqueryShape{sub: sub, agg: agg, b: b, windowFn: c.Func.Name}, true
	case resetsWindowFn, changesWindowFn:
		// Unlike the FOLD family below, resets/changes need no window-wide
		// purity test at all (see histogram_native_mixed_or_subquery_resets_changes.go's
		// own doc), so all three grid modes — instant, `@`-pinned
		// broadcast, and true query_range fan-out — compose here with no
		// restriction.
		return sumOrAvgMixedOrSubqueryShape{sub: sub, agg: agg, b: b, windowFn: c.Func.Name}, true
	case rateWindowFn, increaseWindowFn, deltaWindowFn, irateWindowFn, ideltaWindowFn, sumOverTimeWindowFn, avgOverTimeWindowFn:
		// See this file's own top-level doc, "Scope": the window-purity
		// drop test is not yet sound for a true query_range fan-out (a
		// non-pinned subquery under query_range), so that specific
		// sub-shape stays unmatched here and falls through to the
		// pre-existing rejection unchanged.
		if ctx.rangeMode() && !subqueryPinned(sub) {
			return sumOrAvgMixedOrSubqueryShape{}, false
		}
		return sumOrAvgMixedOrSubqueryShape{sub: sub, agg: agg, b: b, windowFn: c.Func.Name}, true
	default:
		return sumOrAvgMixedOrSubqueryShape{}, false
	}
}

// sumOrAvgMixedOrSubqueryShape is the shape
// [sumOrAvgMixedOrSubqueryOuterFnRecognized] matched: sub's own inner is
// agg (`sum`/`avg` [by/without]) wrapping b, a mixed float/histogram `or`
// ([sumOrAvgOverMixedExpHistogramSetOp]'s own shape), reached under an
// outer SELECT/FOLD-family call named windowFn.
type sumOrAvgMixedOrSubqueryShape struct {
	sub      *parser.SubqueryExpr
	agg      *parser.AggregateExpr
	b        *parser.BinaryExpr
	windowFn string
}

// lowerSumOrAvgMixedOrSubqueryOuterFn lowers the shape
// [sumOrAvgMixedOrSubqueryOuterFnRecognized] matched. Both branches below
// share the identical subquery grid resolution and `bool`-modifier guard;
// they diverge only in whether the window-purity drop test applies.
func lowerSumOrAvgMixedOrSubqueryOuterFn(shape sumOrAvgMixedOrSubqueryShape, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	sub := shape.sub
	step := sub.Step
	if step == 0 {
		step = defaultSubqueryStep
	}
	if step < 0 {
		return nil, fmt.Errorf("promql: subquery step must be positive, got %s", sub.Step)
	}
	gridCtx, ok, err := subqueryGridCtx(sub, step, ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("promql: histogram-valued subquery requires query eval-time context (use LowerAt)")
	}
	if shape.b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}

	switch shape.windowFn {
	case countOverTimeWindowFn, presentOverTimeWindowFn, tsOfFirstOverTimeExpHistFn, tsOfLastOverTimeExpHistFn:
		return lowerSumOrAvgMixedOrSubquerySelectFn(shape, gridCtx, s, ctx)
	case resetsWindowFn, changesWindowFn:
		return lowerMixedOrSubqueryResetsOrChanges(shape, gridCtx, s, ctx)
	default:
		return lowerSumOrAvgMixedOrSubqueryFoldFn(shape, gridCtx, s, ctx)
	}
}

// lowerSumOrAvgMixedOrSubquerySelectFn answers the four type-blind names —
// see this file's own top-level doc. mixedRel, the subquery's own inner
// aggregation lowered ONCE across the whole grid via the EXACT existing
// root-only composer ([lowerSumOrAvgOverMixedExpHistogramSetOp]), is
// already the correct per-(group, subquery anchor) Mixed relation these
// functions fold over — no window-purity filtering needed, so it is
// handed directly to [selectFnOverSubqueryWindowed] /
// [lowerSelectFnOverSubqueryRange] / [capSelectFnOverSubquery], the SAME
// three-mode reduction cerberus issue #2545/#2569 already built for a
// PURE histogram-native subquery inner — those three functions only ever
// read the Attributes/Timestamp/Value columns for this four-name subset
// (never the nine Histogram*Column payload fields), so a Mixed-shaped
// input reduces identically to a Histogram-shaped one.
func lowerSumOrAvgMixedOrSubquerySelectFn(shape sumOrAvgMixedOrSubqueryShape, gridCtx lowerCtx, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	sub := shape.sub
	mixedRel, err := lowerSumOrAvgOverMixedExpHistogramSetOp(shape.agg, shape.b, s, gridCtx)
	if err != nil {
		return nil, err
	}
	if chplan.RowShapeOf(mixedRel) != chplan.MixedRowShape {
		return nil, fmt.Errorf("promql: internal invariant violated: sum/avg-mixed-or subquery input is %T with %s row shape", mixedRel, chplan.RowShapeOf(mixedRel))
	}

	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}
	histSchema := histogramProjectionSchema(s)
	histSchema.AggregationTemporalityColumn = ""

	if ctx.rangeMode() && subqueryPinned(sub) {
		windowed := selectFnOverSubqueryWindowed(shape.windowFn, mixedRel, histSchema)
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return capSelectFnOverSubquery(
			shape.windowFn,
			&chplan.CrossJoin{Left: grid, Right: windowed},
			&chplan.ColumnRef{Name: stepGridAnchorColumn},
			histSchema,
		), nil
	}
	if ctx.rangeMode() {
		return lowerSelectFnOverSubqueryRange(shape.windowFn, mixedRel, sub.Range, anchor.Offset, histSchema, ctx), nil
	}
	windowed := selectFnOverSubqueryWindowed(shape.windowFn, mixedRel, histSchema)
	tsExpr := chplan.NowNano()
	if !anchor.End.IsZero() {
		tsExpr = windowRightBoundExpr(evalAnchor{End: anchor.End})
	}
	return capSelectFnOverSubquery(shape.windowFn, windowed, tsExpr, histSchema), nil
}

// lowerSumOrAvgMixedOrSubqueryFoldFn answers the seven type-preserving
// FOLD-family names — see this file's own top-level doc. Unlike the
// type-blind siblings above, this rebuilds the subquery's own inner
// aggregation from [shadowResolveMixedExpHistogramOperands]'s two
// per-anchor branches directly (rather than calling the combined
// [lowerSumOrAvgOverMixedExpHistogramSetOp]) so [windowPurityUnless] can
// filter EACH branch to its window-pure groups BEFORE either one reaches
// its own window fold — the collision-drop test reference's
// extrapolatedRate / funcSumOverTime / funcAvgOverTime apply for the
// WHOLE window, not per subquery anchor.
func lowerSumOrAvgMixedOrSubqueryFoldFn(shape sumOrAvgMixedOrSubqueryShape, gridCtx lowerCtx, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	sub := shape.sub
	histForAgg, floatForAgg, err := shadowResolveMixedExpHistogramOperands(shape.b, s, gridCtx)
	if err != nil {
		return nil, err
	}
	histBranch, err := lowerExpHistogramSumOrAvgOverPlan(shape.agg, histForAgg, s, ctx.resourceBounds.HistogramMergeMaxCostUnits)
	if err != nil {
		return nil, err
	}
	floatBranch, err := lowerPlainAggOverMixedFloatArm(shape.agg, floatForAgg, s, gridCtx)
	if err != nil {
		return nil, err
	}

	histPure := windowPurityUnless(histBranch, floatBranch, true, s)
	floatPure := windowPurityUnless(floatBranch, histBranch, false, s)

	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}

	histFolded, err := lowerHistFoldOverPureSubqueryBranch(shape, histPure, anchor, s, ctx)
	if err != nil {
		return nil, err
	}
	floatFolded := lowerFloatFoldOverPureSubqueryBranch(shape, floatPure, anchor, s, ctx)

	// histFolded/floatFolded are already disjoint by group construction
	// (windowPurityUnless above excludes any group with both a histogram
	// AND a float row anywhere in the window from BOTH pure branches, so
	// the SAME group cannot resurface in both folds) — the identical
	// "structural no-op" reuse [combineMixedAggregateBranches]'s own doc
	// describes for its ROOT-only caller applies here too, one level up.
	return combineMixedAggregateBranches(histFolded, floatFolded, s, ctx), nil
}

// windowPurityUnless keeps left's rows whose Attributes signature never
// appears ANYWHERE in right — StepAligned forced false so the match
// spans every subquery anchor in the window, unlike
// [mixedOrShadowUnless]'s identical-shaped per-anchor test one level up
// (StepAligned there follows ctx.step, matching one anchor at a time).
// This is the WINDOW-level collision-drop test this file's own doc
// derives from reference's extrapolatedRate / funcSumOverTime /
// funcAvgOverTime: a group with a row of BOTH types anywhere in the
// window loses every one of its rows on BOTH sides, so neither side's
// window fold ever sees it and neither produces an output row for it —
// the fold-level equivalent of reference dropping the whole series.
func windowPurityUnless(left, right chplan.Node, leftIsHistogram bool, s schema.Metrics) chplan.Node {
	return &chplan.VectorSetOp{
		Left:             left,
		Right:            right,
		Op:               chplan.VectorSetUnless,
		Match:            chplan.VectorMatch{},
		StepAligned:      false,
		Histogram:        leftIsHistogram,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}
}

// lowerHistFoldOverPureSubqueryBranch folds input — histPure, already
// window-purity-filtered — with shape.windowFn over sub.Range, reusing
// [expHistogramValuedSubqueryWindowStage] / [aggregatedHistogramProjection]
// unchanged: the SAME per-anchor-input window fold
// [lowerExpHistogramRangeFnOverSubquery] applies to a pure histogram-native
// subquery inner, since histPure already carries the identical
// (Attributes, Timestamp, thirteen-column HistogramProjection) contract
// that function's own `input` does. Only the two single-window grid modes
// [sumOrAvgMixedOrSubqueryOuterFnRecognized] admits (instant, `@`-pinned
// broadcast) are built here — see this file's own "Scope" doc for the
// true fan-out exclusion.
func lowerHistFoldOverPureSubqueryBranch(shape sumOrAvgMixedOrSubqueryShape, input chplan.Node, anchor evalAnchor, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	sub := shape.sub
	histSchema := histogramProjectionSchema(s)
	histSchema.AggregationTemporalityColumn = ""
	windowShape := histogramAggShape{windowRange: sub.Range, windowFn: shape.windowFn}

	if ctx.rangeMode() && subqueryPinned(sub) {
		windowed := expHistogramValuedSubqueryWindowStage(input, windowShape, anchor, histSchema)
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return aggregatedHistogramProjection(
			&chplan.CrossJoin{Left: grid, Right: windowed},
			&chplan.ColumnRef{Name: stepGridAnchorColumn},
			histSchema,
		), nil
	}
	windowed := expHistogramValuedSubqueryWindowStage(input, windowShape, anchor, histSchema)
	tsExpr := chplan.NowNano()
	if !anchor.End.IsZero() {
		tsExpr = windowRightBoundExpr(evalAnchor{End: anchor.End})
	}
	return aggregatedHistogramProjection(windowed, tsExpr, histSchema), nil
}

// lowerFloatFoldOverPureSubqueryBranch folds input — floatPure, already
// window-purity-filtered — with shape.windowFn over sub.Range using the
// ordinary, non-histogram [chplan.RangeWindow] reducer
// [lowerRangeVectorCall] / [lowerOuterRangeFnOverSubquery] already use for
// every plain-float range/subquery composition. input's own Timestamp
// column is already s.TimestampColumn (both
// [shadowResolveMixedExpHistogramOperands]'s branches publish under that
// name, and [windowPurityUnless] forwards it unchanged), so RangeWindow's
// own windowing needs no spine-widening pass: gridCtx already bounded
// input to exactly the window this reduction needs, unlike the generic
// [lowerSubquery] path's own unbounded convention.
func lowerFloatFoldOverPureSubqueryBranch(shape sumOrAvgMixedOrSubqueryShape, input chplan.Node, anchor evalAnchor, s schema.Metrics, ctx lowerCtx) chplan.Node {
	sub := shape.sub
	rw := &chplan.RangeWindow{
		Input:           input,
		Func:            shape.windowFn,
		Range:           sub.Range,
		Offset:          anchor.Offset,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
	if ctx.rangeMode() && subqueryPinned(sub) {
		rw.End = anchor.End
		return wrapRangeWindowAtBroadcast(rw, ctx, s, nil, &chplan.ColumnRef{Name: s.ValueColumn})
	}
	if !anchor.End.IsZero() {
		rw.End = anchor.End
	}
	return rw
}

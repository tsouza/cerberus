package promql

import (
	"fmt"
	"time"

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
//     them with a drop-on-group-collision rule (one
//     [chplan.VectorSetOp.MixedDropCollisions] union), not a naive
//     per-arm distribute. And it is ALREADY reachable at
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
// Composes for all fifteen SELECT/FOLD-family names, across all three grid
// modes (instant, `@`-pinned subquery broadcast, and true query_range
// fan-out):
//
//   - count_over_time, present_over_time, ts_of_first_over_time,
//     ts_of_last_over_time (type-blind, [lowerSumOrAvgMixedOrSubquerySelectFn]).
//   - rate, increase, delta, irate, idelta, sum_over_time, avg_over_time
//     (window-purity-filtered, [lowerSumOrAvgMixedOrSubqueryFoldFn] for the
//     two single-window modes, [lowerSumOrAvgMixedOrSubqueryFoldFnRange] for
//     true fan-out — cerberus issue #2715, see that function's own doc for
//     how the purity test is rescoped per outer anchor).
//   - resets, changes (type-aware sequential merge, cerberus issue #2615 —
//     see histogram_native_mixed_or_subquery_resets_changes.go's own doc for
//     why that pair needs its own machinery rather than either composer
//     above).
//   - last_over_time, first_over_time (type-preserving "pick the newest/
//     oldest row" reduction, cerberus issue #2714 —
//     histogram_native_mixed_or_subquery_last_first.go).
//
// The FOLD family's single-window purity test ([windowPurityUnless]) only
// ever matches ONE window per output series — sound for an instant query or
// an `@`-pinned subquery broadcast, where every output step reports the
// SAME evaluated window, but not for a genuine fan-out, which evaluates
// each output step's own [range] window independently.
// [lowerSumOrAvgMixedOrSubqueryFoldFnRange] answers that mode with a
// DIFFERENT lowering strategy rather than a wider [windowPurityUnless]: it
// reuses the exact per-anchor fan-out reducers each branch already has
// ([lowerExpHistogramSubqueryRangeFnRange] for the histogram side,
// [applyStepGridFanout]'s [chplan.RangeWindow.OuterRange] matrix mode for
// the float side — the SAME helper lowerRangeVectorCall uses for an
// ORDINARY range-vector function under query_range) and adds a per-anchor
// raw-sample-EXISTENCE fan-out for the OPPOSITE branch,
// then anti-joins the two on (Attributes, per-step anchor) via the existing
// [mixedOrShadowUnless] StepAligned idiom — no new chplan or chsql surface,
// exactly like this package's other mixed-or composers.
func sumOrAvgMixedOrSubqueryOuterFnRecognized(c *parser.Call, s schema.Metrics, ctx lowerCtx) (sumOrAvgMixedOrSubqueryShape, bool) {
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
	case lastOverTimeWindowFn, firstOverTimeWindowFn:
		// Cerberus issue #2714 — histogram_native_mixed_or_subquery_last_first.go.
		// Like resets/changes above, this pair picks a single row rather
		// than folding a window, so it needs no window-wide purity test
		// either and reaches all three grid modes.
		return sumOrAvgMixedOrSubqueryShape{sub: sub, agg: agg, b: b, windowFn: c.Func.Name}, true
	case rateWindowFn, increaseWindowFn, deltaWindowFn, irateWindowFn, ideltaWindowFn, sumOverTimeWindowFn, avgOverTimeWindowFn:
		// True query_range fan-out (a non-pinned subquery under query_range)
		// composes via [lowerSumOrAvgMixedOrSubqueryFoldFnRange] — cerberus
		// issue #2715, see this file's own top-level "Scope" doc.
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
	case lastOverTimeWindowFn, firstOverTimeWindowFn:
		return lowerMixedOrSubqueryLastFirst(shape, gridCtx, s, ctx)
	default:
		if ctx.rangeMode() && !subqueryPinned(sub) {
			return lowerSumOrAvgMixedOrSubqueryFoldFnRange(shape, gridCtx, s, ctx)
		}
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
	mixedRel, err := lowerSumOrAvgOverMixedExpHistogramSetOp(shape.agg, shape.b, s, gridCtx)
	if err != nil {
		return nil, err
	}
	if chplan.RowShapeOf(mixedRel) != chplan.MixedRowShape {
		return nil, fmt.Errorf("promql: internal invariant violated: sum/avg-mixed-or subquery input is %T with %s row shape", mixedRel, chplan.RowShapeOf(mixedRel))
	}
	// [lowerSelectFnOverExpHistogramSubqueryInput]'s own doc: this
	// continuation only ever reads the Attributes/Timestamp/Value columns
	// for this four-name subset, so a Mixed-shaped input reduces identically
	// to a Histogram-shaped one — no dedicated Mixed-only body needed.
	return lowerSelectFnOverExpHistogramSubqueryInput(mixedRel, shape.sub, shape.windowFn, s, ctx)
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
	return combineMixedAggregateBranches(histFolded, floatFolded, s, ctx.step > 0), nil
}

// lowerSumOrAvgMixedOrSubqueryFoldFnRange is [lowerSumOrAvgMixedOrSubqueryFoldFn]'s
// true query_range fan-out sibling (cerberus issue #2715): each output step
// evaluates its OWN [sub.Range] window independently, so the window-purity
// drop test — reference's "a window holding both a histogram and a float
// sample drops the whole group" rule — must be scoped PER (group, output
// anchor) rather than once across the whole subquery grid the way
// [windowPurityUnless] does for the single-window modes.
//
// The key realisation: [windowPurityUnless]'s own drop test cannot simply
// be reapplied per anchor to the two branches' FOLDED results
// ([lowerExpHistogramSubqueryRangeFnRange] / [chplan.RangeWindow]'s own
// fan-out), because reference's rule keys on raw sample EXISTENCE within
// the window, not on whether that side's own fold could produce a value —
// a single float sample cannot feed rate's own two-point floor, so it never
// reaches floatFoldedFanout, but it still condemns a colliding histogram
// window under reference regardless. So this builds a SEPARATE, MinSamples-1
// existence fan-out per branch ([mixedOrSubqueryHistExistsFanout] /
// the inline count_over_time [chplan.RangeWindow] for the float side) and
// anti-joins each branch's FOLDED fan-out against the OPPOSITE branch's
// existence fan-out — on (Attributes, per-step anchor), via
// [mixedOrShadowUnless]'s existing StepAligned idiom, the SAME (Attributes,
// Timestamp) match key [combineMixedAggregateBranches] already uses to
// recombine two per-step branches. Both existence fan-outs and both folded
// fan-outs publish their per-step anchor under s.TimestampColumn — the
// histogram side via [aggregatedHistogramProjection]'s own tsExpr
// projection, the float side via chsql's projectAnchorAsTimestampColumn
// (internal/chsql/range_window.go), which surfaces a matrix RangeWindow's
// anchor under its own TimestampColumn field rather than the generic
// "anchor_ts" name — so no extra renaming Project is needed anywhere here.
//
// No new chplan or chsql surface: every node built here is a
// [chplan.RangeBucketFanout], a [chplan.RangeWindow], a [chplan.Project],
// or a [chplan.VectorSetOp], composed the same way this package's other
// mixed-or composers already do.
func lowerSumOrAvgMixedOrSubqueryFoldFnRange(shape sumOrAvgMixedOrSubqueryShape, gridCtx lowerCtx, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
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

	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}

	histSchema := histogramProjectionSchema(s)
	histSchema.AggregationTemporalityColumn = ""

	histFoldedFanout := lowerExpHistogramSubqueryRangeFnRange(
		histBranch, histogramAggShape{windowRange: sub.Range, windowFn: shape.windowFn}, anchor.Offset, histSchema, ctx,
	)
	floatFoldedFanout := mixedOrSubqueryFloatRangeWindow(floatBranch, shape.windowFn, sub.Range, anchor.Offset, s, ctx)

	histExists := mixedOrSubqueryHistExistsFanout(histBranch, sub.Range, anchor.Offset, s, ctx)
	floatExists := mixedOrSubqueryFloatRangeWindow(floatBranch, countOverTimeWindowFn, sub.Range, anchor.Offset, s, ctx)

	histPure := mixedOrShadowUnless(histFoldedFanout, floatExists, true, chplan.VectorMatch{}, s, ctx.step > 0)
	floatPure := mixedOrShadowUnless(floatFoldedFanout, histExists, false, chplan.VectorMatch{}, s, ctx.step > 0)

	// histPure/floatPure are already disjoint by group-and-anchor
	// construction (the two anti-joins above exclude any (group, anchor)
	// with a raw sample of BOTH types from BOTH branches), so this
	// recombine is the identical "structural no-op" reuse
	// [lowerSumOrAvgMixedOrSubqueryFoldFn] already documents for its own
	// single-window sibling.
	return combineMixedAggregateBranches(histPure, floatPure, s, ctx.step > 0), nil
}

// mixedOrSubqueryFloatRangeWindow builds the plain-float per-anchor fan-out
// [chplan.RangeWindow] fn over branch — reused for both the FOLD family's
// own float fold (windowFn = shape.windowFn) and the float side's raw
// existence test (windowFn = count_over_time, which [chplan.RangeWindow]
// emits nothing for an anchor whose window holds zero samples, exactly the
// "exists" answer needs). [applyStepGridFanout] (unlike
// [lowerFloatFoldOverPureSubqueryBranch]'s single-window sibling, which
// leaves Start/End/Step/OuterRange all zero) is what switches the emitter
// from one REDUCED row per series ([chplan.ReducedWindowRowShape] — a
// single-anchor instant fold, [chplan.RowShapeOf]'s own doc) to one row per
// (series, output anchor): [chplan.RangeWindow.OuterRange] is what actually
// selects the matrix emission path, not Start/End/Step alone — the SAME
// helper lowerRangeVectorCall (lower.go) uses to fan an ORDINARY,
// non-histogram range-vector function across a query_range grid.
func mixedOrSubqueryFloatRangeWindow(branch chplan.Node, windowFn string, windowRange, offset time.Duration, s schema.Metrics, ctx lowerCtx) chplan.Node {
	rw := &chplan.RangeWindow{
		Input:           branch,
		Func:            windowFn,
		Range:           windowRange,
		Offset:          offset,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
	applyStepGridFanout(rw, ctx)
	return rw
}

// mixedOrSubqueryHistExistsFanout is the histogram side's raw-sample
// existence fan-out: one [chplan.RangeBucketFanout] row per (group, output
// anchor) whose window holds at least one RAW histBranch row — MinSamples 1,
// deliberately independent of shape.windowFn's own (often stricter, e.g.
// rate's two-point floor) MinSamples requirement, because reference's
// mixed-type collision rule keys on raw sample existence, not on whether
// the histogram side's own fold succeeds. AnchorAlias stays
// [stepGridAnchorColumn] — the fixed name every other [chplan.RangeBucketFanout]
// call site in this package uses — rather than s.TimestampColumn directly:
// histBranch's own raw per-row timestamp is ALREADY published under
// s.TimestampColumn (TimestampCol reads it for bucket membership), so
// naming the synthesized anchor the SAME thing would collide the fan-out's
// own GROUP BY between the two. The wrapping [chplan.Project] renames it
// to s.TimestampColumn afterward, once it is safely a distinct column. The
// result is widened to the ordinary four-column canonical contract (an
// empty `__name__`, matching every other derived-sample projection in this
// package) purely so [mixedOrShadowUnless]'s generic canonical-arm emission
// can reference it — its own count value is never read by any consumer.
func mixedOrSubqueryHistExistsFanout(histBranch chplan.Node, windowRange, offset time.Duration, s schema.Metrics, ctx lowerCtx) chplan.Node {
	fanout := &chplan.RangeBucketFanout{
		Input:          histBranch,
		Start:          ctx.start.UTC(),
		End:            ctx.end.UTC(),
		Step:           ctx.step,
		Lookback:       windowRange,
		Offset:         offset,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
		GroupByAliases: []string{s.AttributesColumn},
		AggFuncs:       []chplan.AggFunc{windowSampleCountAgg(s)},
		MinSamples:     1,
		AnchorAlias:    stepGridAnchorColumn,
		TimestampCol:   s.TimestampColumn,
	}
	return &chplan.Project{
		Input: fanout,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: stepGridAnchorColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: hqWindowSampleCountAlias}, Alias: s.ValueColumn},
		},
	}
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
// (instant, `@`-pinned broadcast) are built here — the true fan-out mode
// is [lowerSumOrAvgMixedOrSubqueryFoldFnRange]'s own sibling reduction, not
// this function widened, because the fan-out purity test needs a
// per-anchor-scoped existence check this function's window-wide
// histPure/floatPure inputs cannot express — see that function's own doc.
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

package promql

import (
	"fmt"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_subquery_select.go lowers the SELECT / COUNT family of
// range-vector functions — count_over_time, present_over_time,
// last_over_time, first_over_time, resets, changes, ts_of_first_over_time,
// ts_of_last_over_time — over a subquery whose own inner expression already
// resolves histogram-native (cerberus issue #2545, the outer-range-vector-fn
// sibling of #2543's bare-subquery-inner fix).
//
// [rangeFnOverExpHistogramSubquery] / [lowerExpHistogramRangeFnOverSubquery]
// (histogram_native_range_fn.go) already answer the FOLD family — rate,
// increase, delta, irate, idelta, and (since this same issue) sum_over_time
// / avg_over_time — which telescope a window of published histograms into
// ONE boundary-corrected distribution. The eight functions this file answers
// never fold: each either SELECTS one of the window's own published samples
// verbatim (last_over_time / first_over_time and their timestamp-reporting
// siblings ts_of_last_over_time / ts_of_first_over_time), or COUNTS
// something about the window's sample sequence without ever reading the
// distribution's own value (count_over_time / present_over_time, resets /
// changes). Reference Prometheus (tsouza/prometheus's promql/functions.go)
// defines real, type-blind semantics for all eight over a window that holds
// ONLY histogram samples — see this file's own doc on each recognizer for
// the citation — which is exactly the shape a subquery whose inner already
// resolved histogram-native produces: one histogram sample per subquery
// anchor, the identical Matrix a bare exp-histogram selector's `[range]`
// would have handed the same functions.
//
// Every other function [chplan.IsPromQLRangeWindowFunc] names —
// max_over_time, min_over_time, stddev_over_time, stdvar_over_time,
// quantile_over_time, mad_over_time, deriv, predict_linear,
// double_exponential_smoothing (holt_winters), ts_of_max_over_time,
// ts_of_min_over_time — reads ONLY `matrixVal[0].Floats` in reference (each
// function's own doc comment or [rangeVectorFloatOnlyDropFuncs] cites the
// exact read), so an all-histogram window answers empty (annotated with a
// "histogram(s) ignored" warning cerberus does not surface) rather than a
// real value. Those keep rejecting through
// [lowerOuterRangeFnOverSubquery]'s existing guard — this file's recognizer
// deliberately excludes their names, so nothing here can shadow that
// rejection.
//
// # Plan shape
//
// Each of the three grid modes [lowerExpHistogramRangeFnOverSubquery]
// distinguishes (pinned-broadcast, range fan-out, plain instant) reduces the
// SAME subquery-anchor relation [lowerExpHistogramValuedShape] already
// produced for the FOLD family, grouped by the published Attributes column
// rather than [histogramIdentityExpr]'s raw-table identity (the subquery's
// own inner already resolved and published Attributes; there is no
// ResourceAttributes merge left to redo) — mirroring
// [expHistogramValuedSubqueryWindowStage]'s identical substitution. No
// additional window Filter is layered on top: [subqueryGridCtx] already
// scoped the relation's own anchors to exactly the outer window this
// function reduces (pinned/instant) or the union the range fan-out then
// re-windows per anchor (range mode) — the same reasoning
// [lowerExpHistogramRangeFnOverSubquery]'s own doc gives for omitting a
// Filter of its own.
type histogramSubquerySelectShape struct {
	sub      *parser.SubqueryExpr
	windowFn string
}

// selectFnOverExpHistogramSubquery recognizes count_over_time /
// present_over_time / last_over_time / first_over_time / resets / changes /
// ts_of_first_over_time / ts_of_last_over_time over a subquery whose inner
// expression is already histogram-valued. Mirrors
// [rangeFnOverExpHistogramSubquery]'s own recognizer shape rung for rung,
// differing only in which function names it admits.
//
// Its match composes two different ways depending on the matched name's
// OWN output shape (cerberus issue #2569, the generic-composition sibling
// of #2545's original root-only fix):
//
//   - The six names whose output is an ordinary FLOAT sample
//     (count_over_time, present_over_time, resets, changes,
//     ts_of_first_over_time, ts_of_last_over_time) need no histogram-aware
//     downstream handling at all once matched — any wrapper that reaches
//     this shape through the generic `lower()` → [lowerCall] path (an
//     aggregation's own generic fallback, a scalar math function, …) gets
//     back a plain [chplan.SampleRowShape] node it already knows how to
//     consume. [lowerCall] retries this recognizer directly for exactly
//     that reason.
//   - The two names whose output PRESERVES the window's own histogram
//     sample (last_over_time, first_over_time) must never reach a
//     consumer that lowers generically without a histogram-shape guard —
//     see [selectFnHistogramPreservingSubquery], which narrows this same
//     match to those two names and is threaded into
//     [lowerExpHistogramValuedShape] / [isExpHistogramValuedShape]
//     instead, mirroring how [rangeFnOverExpHistogramSubquery] (the FOLD
//     family, entirely histogram-preserving) is threaded there rather than
//     into [lowerCall].
func selectFnOverExpHistogramSubquery(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (histogramSubquerySelectShape, bool) {
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok || len(call.Args) != 1 {
		return histogramSubquerySelectShape{}, false
	}
	switch call.Func.Name {
	case countOverTimeWindowFn, presentOverTimeWindowFn,
		lastOverTimeWindowFn, firstOverTimeWindowFn,
		resetsWindowFn, changesWindowFn,
		tsOfFirstOverTimeExpHistFn, tsOfLastOverTimeExpHistFn:
	default:
		return histogramSubquerySelectShape{}, false
	}
	sub, ok := peelWrappers(call.Args[0]).(*parser.SubqueryExpr)
	if !ok || sub.Range <= 0 || !isExpHistogramValuedShape(sub.Expr, s, ctx) || !subqueryHasEvalAnchor(sub, ctx) {
		return histogramSubquerySelectShape{}, false
	}
	return histogramSubquerySelectShape{sub: sub, windowFn: call.Func.Name}, true
}

// selectFnHistogramPreservingSubquery narrows [selectFnOverExpHistogramSubquery]'s
// eight-function match to last_over_time / first_over_time — the two names
// whose result is itself a published histogram sample rather than a float
// (cerberus issue #2569). Threaded into [lowerExpHistogramValuedShape] /
// [isExpHistogramValuedShape] rather than [lowerCall]: unlike the other six
// names in that switch, a caller reaching either of these through the fully
// generic `lower()` path with no histogram-shape awareness (for example
// [lowerLabelReplace]'s own inner lowering, or an ordinary
// [chplan.Aggregate]'s AggFunc, both of which read `Value` unconditionally)
// would either read the wrong column or hit
// [projectAttributesOverInner]'s histogram-shape guard — see that
// function's own doc for the ClickHouse-level failure this narrowing
// avoids. Every existing [lowerExpHistogramValuedShape] consumer already
// knows how to carry a [chplan.HistogramRowShape] node forward (that is
// the whole point of the shared recognizer), so matching there instead
// gives last_over_time/first_over_time-over-subquery the SAME recursive
// reach [rangeFnOverExpHistogramSubquery] already has for the FOLD family
// — nested under sum()/avg() via [mergeableExpHistogramAggregate],
// label_replace/label_join via [labelCallOverExpHistogram], and every
// other consumer this file's own package documents.
func selectFnHistogramPreservingSubquery(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (histogramSubquerySelectShape, bool) {
	shape, ok := selectFnOverExpHistogramSubquery(expr, s, ctx)
	if !ok {
		return histogramSubquerySelectShape{}, false
	}
	switch shape.windowFn {
	case lastOverTimeWindowFn, firstOverTimeWindowFn:
		return shape, true
	default:
		return histogramSubquerySelectShape{}, false
	}
}

// lowerSelectFnOverExpHistogramSubquery lowers the recognised shape. The
// three-way grid split (pinned-broadcast / range fan-out / plain instant)
// and the subquery-grid derivation are [lowerExpHistogramRangeFnOverSubquery]'s
// own rung for rung — see that function's doc for why each arm looks the
// way it does; only the per-anchor reduction (this file's
// [selectFnOverSubqueryWindowed] / [lowerSelectFnOverSubqueryRange] in place
// of the FOLD family's window-fold-and-reshape) differs.
func lowerSelectFnOverExpHistogramSubquery(shape histogramSubquerySelectShape, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	sub := shape.sub
	step := sub.Step
	if step == 0 {
		step = defaultSubqueryStep
	}
	if step < 0 {
		return nil, fmt.Errorf("promql: subquery step must be positive, got %s", sub.Step)
	}
	gridCtx, state, err := subqueryGridCtx(sub, step, ctx)
	if err != nil {
		return nil, err
	}
	if state == subqueryGridUnavailable {
		return nil, fmt.Errorf("promql: histogram-valued subquery requires query eval-time context (use LowerAt)")
	}

	input, matched, err := lowerExpHistogramValuedShape(sub.Expr, s, gridCtx)
	if err != nil {
		return nil, err
	}
	if !matched || chplan.RowShapeOf(input) != chplan.HistogramRowShape {
		return nil, fmt.Errorf("promql: internal invariant violated: histogram subquery input is %T with %s row shape", input, chplan.RowShapeOf(input))
	}
	node, err := lowerSelectFnOverExpHistogramSubqueryInput(input, sub, shape.windowFn, s, ctx)
	if err != nil {
		return nil, err
	}
	// Same reasoning as [lowerExpHistogramRangeFnOverSubquery]'s own cap:
	// every name [selectFnOverExpHistogramSubquery] admits (count/present/
	// last/first_over_time, resets, changes, ts_of_first/last_over_time)
	// is a per-series window selection, so an empty subquery matrix
	// selects nothing and capping the selected result is the same answer
	// as capping the matrix.
	if state == subqueryGridEmpty {
		node = emptySubqueryGrid(node)
	}
	return node, nil
}

// lowerSelectFnOverExpHistogramSubqueryInput is
// [lowerSelectFnOverExpHistogramSubquery] split at the one point that
// actually varies by caller: everything from here on operates on input —
// already gridCtx-lowered and already confirmed HistogramRowShape — with no
// further dependence on HOW input was derived. cerberus issue #2724 reuses
// this continuation directly for a further `and`/`unless`/`or` wrapping a
// histogram-native (or mixed-or) subquery inner
// (histogram_native_mixed_or_subquery_further_setop_range_fn.go): that
// shape's own input comes from [lowerSubquery]'s ordinary dispatch
// ([lowerSubqueryOverBinary], cerberus issue #2589) rather than
// [lowerExpHistogramValuedShape], but once lowered it is byte-for-byte the
// same thirteen-column HistogramRowShape contract this function's own
// caller already produces, so the window fold itself needs no
// caller-specific branch at all.
func lowerSelectFnOverExpHistogramSubqueryInput(input chplan.Node, sub *parser.SubqueryExpr, windowFn string, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}
	histSchema := histogramProjectionSchema(s)
	// Mirrors [lowerExpHistogramRangeFnOverSubquery]: HistogramProjection is
	// a Prometheus-native value boundary, not a physical OTel row, so it
	// never carries the OTel temporality column these eight functions have
	// no use for regardless (none of them needs [needsTemporalityAgg]).
	histSchema.AggregationTemporalityColumn = ""

	if ctx.rangeMode() && subqueryPinned(sub) {
		windowed := selectFnOverSubqueryWindowed(windowFn, input, histSchema, ctx)
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return capSelectFnOverSubquery(
			windowFn,
			&chplan.CrossJoin{Left: grid, Right: windowed},
			&chplan.ColumnRef{Name: stepGridAnchorColumn},
			histSchema,
		), nil
	}
	if ctx.rangeMode() {
		return lowerSelectFnOverSubqueryRange(windowFn, input, sub.Range, anchor.Offset, histSchema, ctx), nil
	}
	windowed := selectFnOverSubqueryWindowed(windowFn, input, histSchema, ctx)
	tsExpr := chplan.NowNano()
	if !anchor.End.IsZero() {
		tsExpr = windowRightBoundExpr(evalAnchor{End: anchor.End})
	}
	return capSelectFnOverSubquery(windowFn, windowed, tsExpr, histSchema), nil
}

// selectFnOverSubqueryWindowed builds the instant-mode (and pinned-broadcast,
// before the CrossJoin) per-series reduction: the subquery-anchor relation
// grouped by its own published Attributes column, collapsed by the
// windowFn-appropriate aggregate set. No window bound (windowRange / anchor)
// is threaded in here — every aggregate reduces the WHOLE relation handed to
// it, already scoped to the outer window by [subqueryGridCtx] (see this
// file's own top-level doc); resets/changes' [minSamplesFilter] floor reads
// only the aggregated sample-count column, not the window bounds themselves.
//
// Every name but last_over_time / first_over_time additionally carries the
// name-drop collision guard ([selectFnDropsSeriesName], cerberus issue
// #3232): grouping by Attributes ALONE merges two series that differ only
// by `__name__`, which is exactly the pair reference refuses to answer once
// the drop collapses them onto one label set. The two preserving names are
// excluded because they keep `__name__` in their output — and for exactly
// that reason they key ON it instead ([selectFnSubqueryKey], cerberus issue
// #3253) rather than carrying no series-identity answer at all.
func selectFnOverSubqueryWindowed(windowFn string, input chplan.Node, s schema.Metrics, ctx lowerCtx) chplan.Node {
	groupBy, aliases := selectFnSubqueryKey(windowFn, s)
	reduce := func(aggs []chplan.AggFunc) chplan.Node {
		return selectFnSubqueryNameGuard(windowFn, &chplan.Aggregate{
			Input:              input,
			GroupBy:            groupBy,
			GroupByAliases:     aliases,
			AggFuncs:           selectFnSubqueryAggs(windowFn, aggs, s),
			DropEmptyOnNoGroup: true,
		}, s, ctx)
	}
	switch windowFn {
	case lastOverTimeWindowFn, firstOverTimeWindowFn:
		return reduce(nativeExpHistValuedLatestAggsDirectional(windowFn, s))
	case countOverTimeWindowFn, presentOverTimeWindowFn:
		return reduce([]chplan.AggFunc{expHistogramCountPresentValueAgg(windowFn, s)})
	case resetsWindowFn, changesWindowFn:
		return expHistogramPairCountStage(
			minSamplesFilter(
				reduce(append(expHistogramPairCountAggs(windowFn, s), windowSampleCountAgg(s))),
				stalenessMinSamples,
			),
			windowFn, aliases, s, expHistogramDensifiedResetMaskEligible(ctx.lowerers),
		)
	default: // tsOfFirstOverTimeExpHistFn, tsOfLastOverTimeExpHistFn
		return reduce(tsOfSampleTimestampAgg(windowFn, s))
	}
}

// selectFnDropsSeriesName reports whether windowFn removes `__name__`
// from its own output — true for every name
// [selectFnOverExpHistogramSubquery] admits except last_over_time and
// first_over_time, which select a published sample verbatim and keep the
// name with it. [capSelectFnOverSubquery] reads it too when it picks an
// output projection — the name-preserving branch is that projection's own
// first question — so the guard and the projection decide "does this name
// drop" from ONE predicate rather than from two switches that could drift.
func selectFnDropsSeriesName(windowFn string) bool {
	switch windowFn {
	case lastOverTimeWindowFn, firstOverTimeWindowFn:
		return false
	default:
		return true
	}
}

// selectFnSubqueryAggs and selectFnSubqueryNameGuard are the two halves
// of the name-drop collision guard, applied to whichever of the three
// grid modes is reducing. See [subqueryNameCollisionAgg] for what the
// guard tests and why the test is exact; this pair only decides WHERE it
// rides and WHICH names owe it.
//
// Both are no-ops for the two name-preserving names, so every reduction
// in this file can route through them unconditionally rather than
// repeating the split per grid mode.
func selectFnSubqueryAggs(windowFn string, aggs []chplan.AggFunc, s schema.Metrics) []chplan.AggFunc {
	if !selectFnDropsSeriesName(windowFn) {
		return aggs
	}
	return append(aggs, subqueryNameCollisionAgg(s))
}

func selectFnSubqueryNameGuard(windowFn string, node chplan.Node, s schema.Metrics, ctx lowerCtx) chplan.Node {
	if !selectFnDropsSeriesName(windowFn) {
		return node
	}
	return subqueryNameCollisionFilter(node, s, ctx)
}

// selectFnSubqueryKey is the third half of the same split, and the one
// cerberus issue #3253 added: WHICH identity the reduction groups on.
//
// A name-dropping name reduces on Attributes alone — its output carries no
// `__name__`, so two names under one label set are one output series, and
// [selectFnSubqueryNameGuard] is what refuses to answer it. A
// name-PRESERVING name (last_over_time / first_over_time) publishes the
// selected sample's own `__name__`, so its output identity is the PAIR:
// reducing on Attributes alone merged the two series and then argMax'd a
// single name out of them, which published one of the pair and DELETED the
// other, where reference folds per series and answers both.
//
// The wider key is only ever too COARSE to be wrong, never too fine: where
// `__name__` is uniform within an Attributes group — every ordinary
// single-metric query — the two keys partition the relation identically.
// The key expressions are [mixedLastFirstSeriesKey]'s, shared rather than
// re-derived: the Mixed-shape sibling reached the same conclusion first
// (cerberus issue #3227) and a second spelling of one key is a spelling
// that can drift.
func selectFnSubqueryKey(windowFn string, s schema.Metrics) ([]chplan.Expr, []string) {
	if selectFnDropsSeriesName(windowFn) {
		return []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}, []string{s.AttributesColumn}
	}
	return mixedLastFirstSeriesKey(s), mixedLastFirstSeriesKeyAliases(s)
}

// lowerSelectFnOverSubqueryRange is the query_range shape: one
// `[sub.Range]` window per step anchor via [chplan.RangeBucketFanout] over
// the subquery-anchor relation directly — the range-mode counterpart of
// [selectFnOverSubqueryWindowed]. Unlike [buildHistogramBucketFanout] (the
// bare selectors' own fan-out builder) this never wraps a raw table Scan,
// so it builds the [chplan.RangeBucketFanout] node itself rather than
// reusing that helper — the same substitution
// [lowerExpHistogramSubqueryRangeFnRange] makes for the FOLD family, and
// for the identical reason: [canonicalGroupKeyExprs] canonicalises a raw
// table's series-identity binding, which the subquery's already-published
// Attributes column does not need.
//
// The per-series sample floor is [stalenessMinSamples] (one) for every
// function this file answers — even resets/changes, whose bare sibling
// reads the identical constant via [histogramAggShape.minSamples] — so the
// fan-out's own MinSamples HAVING is reused unconditionally instead of
// resolving it per function.
func lowerSelectFnOverSubqueryRange(windowFn string, input chplan.Node, windowRange, offset time.Duration, s schema.Metrics, ctx lowerCtx) chplan.Node {
	anchorRef := &chplan.ColumnRef{Name: stepGridAnchorColumn}
	groupBy, aliases := selectFnSubqueryKey(windowFn, s)
	// Same guard the other two grid modes carry
	// ([selectFnSubqueryNameGuard]), on the per-(series, anchor) buckets
	// this mode reduces to instead of the per-series groups they do. The
	// question is per output vector either way — reference checks one
	// evaluation step's Matrix at a time — and a fan-out bucket IS one
	// step's group, so the abort rides on the fan-out's own output rather
	// than needing an anchor key of its own.
	fanout := func(aggs []chplan.AggFunc) chplan.Node {
		return selectFnSubqueryNameGuard(windowFn, &chplan.RangeBucketFanout{
			Input:          input,
			Start:          ctx.start.UTC(),
			End:            ctx.end.UTC(),
			Step:           ctx.step,
			Lookback:       windowRange,
			Offset:         offset,
			GroupBy:        groupBy,
			GroupByAliases: aliases,
			AggFuncs:       selectFnSubqueryAggs(windowFn, aggs, s),
			MinSamples:     stalenessMinSamples,
			AnchorAlias:    stepGridAnchorColumn,
			TimestampCol:   s.TimestampColumn,
		}, s, ctx)
	}
	switch windowFn {
	case lastOverTimeWindowFn, firstOverTimeWindowFn:
		return capSelectFnOverSubquery(windowFn, fanout(nativeExpHistValuedLatestAggsDirectional(windowFn, s)), anchorRef, s)
	case countOverTimeWindowFn, presentOverTimeWindowFn:
		return capSelectFnOverSubquery(windowFn, fanout([]chplan.AggFunc{expHistogramCountPresentValueAgg(windowFn, s)}), anchorRef, s)
	case resetsWindowFn, changesWindowFn:
		perSeries := expHistogramPairCountStage(
			fanout(expHistogramPairCountAggs(windowFn, s)),
			windowFn, []string{stepGridAnchorColumn, s.AttributesColumn}, s, expHistogramDensifiedResetMaskEligible(ctx.lowerers),
		)
		return expHistogramPairCountProjection(perSeries, anchorRef, s)
	default: // tsOfFirstOverTimeExpHistFn, tsOfLastOverTimeExpHistFn
		return capSelectFnOverSubquery(windowFn, fanout(tsOfSampleTimestampAgg(windowFn, s)), anchorRef, s)
	}
}

// capSelectFnOverSubquery caps the per-anchor reduction with the
// windowFn-appropriate output shape: [nativeHistogramProjection]'s
// thirteen-column contract (name PRESERVED via the selected row's own
// aggregated MetricName — [bareExpHistogramNameExpr]) for last_over_time /
// first_over_time, [expHistogramCountPresentProjection]'s float quartet
// (name dropped) for count_over_time / present_over_time,
// [expHistogramPairCountProjection]'s float quartet (name dropped) for
// resets / changes, and [tsOfSelectProjection]'s float quartet (name
// dropped) for the two ts_of_* siblings.
func capSelectFnOverSubquery(windowFn string, input chplan.Node, tsExpr chplan.Expr, s schema.Metrics) chplan.Node {
	if !selectFnDropsSeriesName(windowFn) {
		return nativeHistogramProjection(input, bareExpHistogramNameExpr(s), tsExpr, s)
	}
	switch windowFn {
	case countOverTimeWindowFn, presentOverTimeWindowFn:
		return expHistogramCountPresentProjection(input, tsExpr, s)
	case resetsWindowFn, changesWindowFn:
		return expHistogramPairCountProjection(input, tsExpr, s)
	default: // tsOfFirstOverTimeExpHistFn, tsOfLastOverTimeExpHistFn
		return tsOfSelectProjection(input, tsExpr, s)
	}
}

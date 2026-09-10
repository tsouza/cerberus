package promql

import (
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_subquery_call_subquery.go answers cerberus issue #2726:
// a SELECT/FOLD-family outer function over a DOUBLY-nested subquery whose
// own inner is a nested subquery in turn —
// `<fn>(<inner-sub>)[<outer-range>:<step>]`, canonical example
// `max_over_time(rate(m[1m])[5m:30s])[1h:5m]` — when innerSub's OWN inner
// expression resolves histogram-native (or a further and/unless/or
// wrapping one, cerberus issue #2724). This is the doubly-nested sibling
// [lowerSubqueryOverCallSubquery] itself names as the shape deliberately
// left on [histogramSubqueryFloatOnlyDropFunc]'s reject-or-drop path
// before this file existed.
//
// # Why the single-level continuations don't just work here
//
// The single-level histogram-subquery continuations
// (histogram_native_subquery_select.go,
// histogram_native_range_fn.go, and their Mixed-shape siblings) all fan
// out across the AMBIENT REQUEST's own grid (ctx.start/ctx.end/ctx.step),
// with the subquery's `[range]` as the per-anchor Lookback. That is the
// right grid for `<fn>(<histogram-subquery>)` — the outer function reduces
// once per REQUEST step. This doubly-nested shape needs a SECOND,
// independent grid instead: the OUTER subquery's own anchors, spaced
// `step` across `sub.Range`, each of which reduces over wideInner's
// per-inner-step rows via a `[innerSub.Range]` lookback — exactly what
// [chplan.RangeWindow.OuterRange] already provides for the plain-float
// sibling built by [lowerSubqueryOverCallSubquery] itself. Cerberus issue
// #2726 generalizes [chplan.RangeBucketFanout] (histogram_native_subquery_
// select.go / histogram_native_range_fn.go's array-fold engine, which
// carried only the ambient-grid (Start, End, Step) mode) with the SAME
// OuterRange-independent-grid mode, so every AggFunc set those files
// already built stays unchanged — only the grid parameters differ.
//
// # Composition
//
// Every one of the fifteen SELECT/FOLD-family names dispatches to a
// continuation reusing the SAME aggregate-selection helpers the ambient-
// grid siblings already built ([nativeExpHistBareAggsDirectional],
// [expHistogramCountPresentValueAgg], [expHistogramPairCountAggs] /
// [expHistogramPairCountStage] / [expHistogramPairCountProjection],
// [tsOfSampleTimestampAgg], [capSelectFnOverSubquery] for the eight
// SELECT-family names; [mixedLastFirstSeriesKeyedAggs] / [mixedLastFirstProjection]
// and [mixedPairCountAggs] / [mixedPairCountStage] for their Mixed-shape
// last/first and resets/changes siblings; [expHistogramValuedWindowFold] /
// [expHistogramValuedWindowAggs] / [expHistogramWindowReshape] /
// [selectExpHistogramWindowSamples] for the seven FOLD-family names, and
// [splitMixedRelByDiscriminator] + [combineMixedAggregateBranches] for
// their Mixed-shape recombination) — but NEVER the ambient-grid
// continuation FUNCTIONS themselves, which hardcode ctx.start/end/step.
type histogramCallSubqueryGrid struct {
	outerSub   *parser.SubqueryExpr
	innerRange time.Duration
	step       time.Duration
}

// buildOuterRangeSubqueryFanout builds the chplan.RangeBucketFanout that
// reduces wideInner — the widened doubly-nested inner subquery's own
// per-(series, t_inner) relation — into one row per OUTER-subquery anchor
// across [grid.outerSub.End - grid.outerSub.Range, End] spaced by
// grid.step, mirroring [lowerSubqueryOverCallSubquery]'s own OuterRange-
// mode RangeWindow at the identical composition point.
//
// wideInner's per-row timestamp column is grid-agnostic: whether it
// resolved HistogramRowShape or MixedRowShape, it was returned AS-IS by
// [lowerHistogramNativeSubqueryInner] / [lowerSubqueryOverBinary]'s own
// histogram/mixed branches (never wrapped by [subqueryAnchorShape], which
// only applies to the Sample-shape siblings), so its timestamp column is
// s.TimestampColumn — the same column every ambient-grid continuation
// already reads via TimestampCol: s.TimestampColumn.
func buildOuterRangeSubqueryFanout(
	wideInner chplan.Node,
	grid histogramCallSubqueryGrid,
	anchor evalAnchor,
	key callSubqueryFanoutKey,
	aggs []chplan.AggFunc,
	minSamples int,
	s schema.Metrics,
) *chplan.RangeBucketFanout {
	return &chplan.RangeBucketFanout{
		Input:          wideInner,
		End:            anchor.End,
		OuterRange:     grid.outerSub.Range,
		Step:           grid.step,
		StepAlign:      true,
		Lookback:       grid.innerRange,
		Offset:         anchor.Offset,
		GroupBy:        key.exprs,
		GroupByAliases: key.aliases,
		AggFuncs:       aggs,
		MinSamples:     minSamples,
		AnchorAlias:    chplan.RangeWindowAnchorColumn,
		TimestampCol:   s.TimestampColumn,
	}
}

// callSubqueryFanoutKey is the per-series grouping key
// [buildOuterRangeSubqueryFanout] folds each outer anchor's window under,
// together with the aliases the fanout publishes it as. The two are one
// value because the emitter pairs them POSITIONALLY, so a widening that
// touched only one of them would misname a column rather than fail.
type callSubqueryFanoutKey struct {
	exprs   []chplan.Expr
	aliases []string
}

// seriesIdentityCallSubqueryKey is `[Attributes, MetricName]` — the whole
// of a series' identity — and is what every NAME-PRESERVING continuation
// keys on.
//
// The fanout used to key on Attributes alone throughout, and the
// name-preserving continuations then recovered `__name__` with an
// argMax/argMin pick over the group. That is a wrong answer whenever two
// series share their attributes and differ in `__name__` — exactly what
// the `or` that builds a Mixed relation produces, since upstream matches
// its arms on a signature that EXCLUDES `__name__` (`rangeEval` prepends
// `labels.MetricName` to the ignoring(...) list before hashing,
// promql/engine.go). Reference folds per SERIES and keys its output on
// `Metric.Hash()`, so it answers TWO series where cerberus published
// whichever name the pick happened to select and dropped the other series
// outright. Cerberus issue #3240.
//
// [mixedLastFirstSeriesKey]'s doc carries the argument that this widening
// is safe for any input: against a fold that groups by full series
// identity an Attributes-only key is only ever too COARSE, never too fine,
// and where `__name__` is uniform within an Attributes group the wider key
// is byte-identical to the narrower one. This is the same key, reused
// rather than re-derived.
func seriesIdentityCallSubqueryKey(s schema.Metrics) callSubqueryFanoutKey {
	return callSubqueryFanoutKey{
		exprs:   mixedLastFirstSeriesKey(s),
		aliases: mixedLastFirstSeriesKeyAliases(s),
	}
}

// attributesCallSubqueryKey is `[Attributes]`, and is what every
// NAME-DROPPING continuation keys on.
//
// Those continuations must NOT widen: their output has no `__name__` at
// all (the projections publish `” AS MetricName`), so two series that
// differ only in `__name__` would be published under one identical label
// set — the shape reference raises
// `vector cannot contain metrics with the same labelset` for. Keying on
// Attributes alone is what lets [subqueryNameCollisionAgg]'s
// distinct-`__name__` count see the collision at all, and
// [subqueryNameCollisionFilter] turn it into that error.
func attributesCallSubqueryKey(s schema.Metrics) callSubqueryFanoutKey {
	return callSubqueryFanoutKey{
		exprs:   []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
		aliases: []string{s.AttributesColumn},
	}
}

// guardedCallSubqueryFanout is [buildOuterRangeSubqueryFanout] keyed on
// [attributesCallSubqueryKey] and wrapped in the duplicate-labelset guard
// — the composition every NAME-DROPPING continuation in this file owes.
//
// The two halves have to travel together: the guard reads a
// distinct-`__name__` count that only [subqueryNameCollisionAgg] publishes,
// and that count only answers the right question over a group key that does
// not already carry the name. Splitting them across call sites is how the
// singly-nested family's own guard is written
// ([selectFnSubqueryAggs] / [selectFnSubqueryNameGuard]); here the whole
// file's key is one decision, so the pair collapses into one call.
//
// The widened agg list is built into a fresh slice rather than appended
// in place. [lowerExpHistogramFoldOverCallSubqueryInput] hands the SAME
// `aggs` value to its downstream reshape stages afterwards, and those
// stages derive their projection from the list they are given — an
// in-place append into that slice's spare capacity would put the guard
// column into a relation whose own reduction never published it.
func guardedCallSubqueryFanout(
	wideInner chplan.Node,
	grid histogramCallSubqueryGrid,
	anchor evalAnchor,
	aggs []chplan.AggFunc,
	minSamples int,
	s schema.Metrics,
	ctx lowerCtx,
) chplan.Node {
	guarded := make([]chplan.AggFunc, 0, len(aggs)+1)
	guarded = append(guarded, aggs...)
	guarded = append(guarded, subqueryNameCollisionAgg(s))
	return subqueryNameCollisionFilter(
		buildOuterRangeSubqueryFanout(wideInner, grid, anchor, attributesCallSubqueryKey(s), guarded, minSamples, s),
		s, ctx,
	)
}

// lowerHistogramOrMixedCallSubqueryInput dispatches the fifteen SELECT/
// FOLD-family names for the doubly-nested composition
// `<fn>(<inner-sub>)[<outer-range>:<step>]`, mirroring
// [lowerHistogramOrMixedSubqueryOuterFnInput]'s own switch vocabulary rung
// for rung. wideInner is already lowered (the widened inner subquery's own
// relation) and shape is its already-computed row shape.
func lowerHistogramOrMixedCallSubqueryInput(
	wideInner chplan.Node,
	shape chplan.RowShape,
	windowFn string,
	outerSub, innerSub *parser.SubqueryExpr,
	step time.Duration,
	s schema.Metrics,
	ctx lowerCtx,
) (node chplan.Node, matched bool, err error) {
	grid := histogramCallSubqueryGrid{outerSub: outerSub, innerRange: innerSub.Range, step: step}
	switch windowFn {
	case countOverTimeWindowFn, presentOverTimeWindowFn, tsOfFirstOverTimeExpHistFn, tsOfLastOverTimeExpHistFn:
		node, err = lowerSelectFnOverCallSubqueryInput(wideInner, grid, windowFn, s, ctx)
		return node, true, err
	case lastOverTimeWindowFn, firstOverTimeWindowFn:
		if shape == chplan.HistogramRowShape {
			node, err = lowerSelectFnOverCallSubqueryInput(wideInner, grid, windowFn, s, ctx)
			return node, true, err
		}
		node, err = lowerMixedLastFirstOverCallSubqueryInput(wideInner, grid, windowFn, s, ctx)
		return node, true, err
	case resetsWindowFn, changesWindowFn:
		if shape == chplan.HistogramRowShape {
			node, err = lowerSelectFnOverCallSubqueryInput(wideInner, grid, windowFn, s, ctx)
			return node, true, err
		}
		node, err = lowerMixedResetsOrChangesOverCallSubqueryInput(wideInner, grid, windowFn, s, ctx)
		return node, true, err
	case rateWindowFn, increaseWindowFn, deltaWindowFn, irateWindowFn, ideltaWindowFn, sumOverTimeWindowFn, avgOverTimeWindowFn:
		if shape == chplan.HistogramRowShape {
			node, err = lowerExpHistogramFoldOverCallSubqueryInput(wideInner, grid, windowFn, s, ctx)
			return node, true, err
		}
		node, err = lowerMixedFoldOverCallSubqueryInput(wideInner, grid, windowFn, s, ctx)
		return node, true, err
	default:
		return nil, false, nil
	}
}

// lowerSelectFnOverCallSubqueryInput answers all eight SELECT-family names
// (count_over_time, present_over_time, last_over_time, first_over_time,
// resets, changes, ts_of_first_over_time, ts_of_last_over_time) over
// wideInner, regardless of whether its shape is HistogramRowShape or
// MixedRowShape — mirrors [lowerSelectFnOverExpHistogramSubqueryInput]'s
// own type-blind reasoning for the four names that never read the
// histogram-specific columns, widened here to ALL eight since a Mixed
// input's last_over_time / first_over_time / resets / changes route here
// too via [mixedLastFirstSeriesKeyedAggs] / [mixedPairCountAggs] (built for a
// Mixed-shaped input exactly as their ambient-grid siblings are).
//
// Callers (see [lowerHistogramOrMixedCallSubqueryInput]) only ever pass
// the four fully type-blind names here for a MixedRowShape wideInner;
// last_over_time/first_over_time/resets/changes over Mixed route through
// the dedicated Mixed continuations below instead.
func lowerSelectFnOverCallSubqueryInput(wideInner chplan.Node, grid histogramCallSubqueryGrid, windowFn string, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	anchor, err := subqueryAnchor(grid.outerSub, ctx)
	if err != nil {
		return nil, err
	}
	// wideInner is a Histogram/Mixed-shaped relation, so it publishes its
	// nine histogram payload fields under their fixed chplan.Histogram*Column
	// aliases — NOT under the physical exp-histogram table's own
	// Count/Sum/Scale/… names, which nothing at this level of the plan still
	// exposes. Reading them through histogramProjectionSchema is what every
	// other continuation in this file already does, and what the single-level
	// sibling [lowerSelectFnOverExpHistogramSubqueryInput] does for the
	// identical eight names; passing the ambient `s` instead emitted
	// `argMax(Count, TimeUnix)` over a relation publishing `HistogramCount`,
	// a ClickHouse "Unknown expression or function identifier" error for
	// last_over_time / first_over_time / resets / changes (the four names
	// here that read the payload at all). AggregationTemporalityColumn is
	// cleared for that sibling's own stated reason: a HistogramProjection is
	// a Prometheus-native value boundary, not a physical OTel row.
	histSchema := histogramProjectionSchema(s)
	histSchema.AggregationTemporalityColumn = ""
	anchorRef := &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}
	// The name-preserving pair keys on full series identity; the six that
	// drop `__name__` key on Attributes alone and carry the collision
	// guard instead. See [seriesIdentityCallSubqueryKey] /
	// [attributesCallSubqueryKey].
	if !selectFnDropsSeriesName(windowFn) {
		fanout := buildOuterRangeSubqueryFanout(
			wideInner, grid, anchor, seriesIdentityCallSubqueryKey(s),
			nativeExpHistValuedLatestAggsDirectional(windowFn, histSchema), stalenessMinSamples, s,
		)
		return capSelectFnOverSubquery(windowFn, fanout, anchorRef, histSchema), nil
	}
	guarded := func(aggs []chplan.AggFunc) chplan.Node {
		return guardedCallSubqueryFanout(wideInner, grid, anchor, aggs, stalenessMinSamples, s, ctx)
	}
	switch windowFn {
	case countOverTimeWindowFn, presentOverTimeWindowFn:
		return capSelectFnOverSubquery(windowFn, guarded([]chplan.AggFunc{expHistogramCountPresentValueAgg(windowFn, histSchema)}), anchorRef, histSchema), nil
	case resetsWindowFn, changesWindowFn:
		perSeries := expHistogramPairCountStage(
			guarded(expHistogramPairCountAggs(windowFn, histSchema)),
			windowFn, []string{chplan.RangeWindowAnchorColumn, s.AttributesColumn}, histSchema,
			expHistogramDensifiedResetMaskEligible(ctx.lowerers),
		)
		return expHistogramPairCountProjection(perSeries, anchorRef, histSchema), nil
	default: // tsOfFirstOverTimeExpHistFn, tsOfLastOverTimeExpHistFn
		return capSelectFnOverSubquery(windowFn, guarded(tsOfSampleTimestampAgg(windowFn, histSchema)), anchorRef, histSchema), nil
	}
}

// lowerMixedLastFirstOverCallSubqueryInput answers last_over_time /
// first_over_time over a MixedRowShape wideInner — the doubly-nested
// sibling of [lowerMixedOrSubqueryLastFirstRange], fed the identical
// [mixedLastFirstSeriesKeyedAggs] / [mixedLastFirstProjection] pair but reducing
// across [buildOuterRangeSubqueryFanout]'s independent outer-subquery grid
// instead of the ambient request grid.
func lowerMixedLastFirstOverCallSubqueryInput(wideInner chplan.Node, grid histogramCallSubqueryGrid, windowFn string, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	anchor, err := subqueryAnchor(grid.outerSub, ctx)
	if err != nil {
		return nil, err
	}
	histSchema := histogramProjectionSchema(s)
	histSchema.AggregationTemporalityColumn = ""
	anchorRef := &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}
	fanout := buildOuterRangeSubqueryFanout(
		wideInner, grid, anchor, seriesIdentityCallSubqueryKey(s),
		mixedLastFirstSeriesKeyedAggs(windowFn, histSchema), stalenessMinSamples, s,
	)
	return mixedLastFirstProjection(fanout, anchorRef, histSchema, s), nil
}

// lowerMixedResetsOrChangesOverCallSubqueryInput answers resets/changes
// over a MixedRowShape wideInner — the doubly-nested sibling of
// [lowerMixedOrSubqueryResetsRange], fed the identical [mixedPairCountAggs]
// / [mixedPairCountStage] pair but reducing across
// [buildOuterRangeSubqueryFanout]'s independent outer-subquery grid.
func lowerMixedResetsOrChangesOverCallSubqueryInput(wideInner chplan.Node, grid histogramCallSubqueryGrid, windowFn string, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	anchor, err := subqueryAnchor(grid.outerSub, ctx)
	if err != nil {
		return nil, err
	}
	histSchema := histogramProjectionSchema(s)
	histSchema.AggregationTemporalityColumn = ""
	anchorRef := &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}
	fanout := guardedCallSubqueryFanout(
		wideInner, grid, anchor, mixedPairCountAggs(windowFn, histSchema), stalenessMinSamples, s, ctx,
	)
	perSeries := mixedPairCountStage(fanout, windowFn, []string{chplan.RangeWindowAnchorColumn, s.AttributesColumn}, histSchema, expHistogramDensifiedResetMaskEligible(ctx.lowerers))
	return expHistogramPairCountProjection(perSeries, anchorRef, s), nil
}

// lowerExpHistogramFoldOverCallSubqueryInput answers the seven FOLD-family
// names (rate, increase, delta, irate, idelta, sum_over_time, avg_over_time)
// over a HistogramRowShape wideInner — the doubly-nested sibling of
// [lowerExpHistogramSubqueryRangeFnRange], reusing that function's own
// window-fold machinery ([expHistogramValuedWindowFold],
// [expHistogramValuedWindowAggs], [selectExpHistogramWindowSamples],
// [expHistogramWindowReshape], [aggregatedHistogramProjection]) unchanged;
// only the RangeBucketFanout's grid parameters differ.
func lowerExpHistogramFoldOverCallSubqueryInput(wideInner chplan.Node, grid histogramCallSubqueryGrid, windowFn string, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	anchor, err := subqueryAnchor(grid.outerSub, ctx)
	if err != nil {
		return nil, err
	}
	histSchema := histogramProjectionSchema(s)
	// HistogramProjection is a Prometheus-native value boundary, not a
	// physical OTel row — mirrors lowerExpHistogramRangeFnOverSubqueryInput.
	histSchema.AggregationTemporalityColumn = ""
	shape := histogramAggShape{windowRange: grid.innerRange, windowFn: windowFn}
	anchorRef := &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}
	win := histogramWindow{lookback: shape.windowRange, offset: anchor.Offset, minSamples: shape.minSamples()}
	rangeStart, rangeEnd := fanoutWindowBoundsExpr(anchorRef, win)
	fold, winIn := expHistogramValuedWindowFold(shape, rangeStart, rangeEnd, histSchema)
	winIn = winIn.withLowerers(ctx.lowerers)
	aggs := expHistogramValuedWindowAggs(histSchema, windowFn)
	grouped := guardedCallSubqueryFanout(wideInner, grid, anchor, aggs, win.minSamples, s, ctx)
	selected := selectExpHistogramWindowSamples(
		guardExpHistogramWindowReduction(grouped, ctx), aggs,
		[]string{chplan.RangeWindowAnchorColumn, s.AttributesColumn},
		histogramWindowSelectionFor(windowFn),
	)
	perSeries := expHistogramWindowReshape(
		selected, aggs, []string{chplan.RangeWindowAnchorColumn, s.AttributesColumn},
		expHistogramResetMaskFor(windowFn), fold, windowFn, winIn,
		expHistogramValuedWindowScalars(fold, histSchema), histSchema,
	)
	return aggregatedHistogramProjection(perSeries, anchorRef, histSchema), nil
}

// lowerMixedFoldOverCallSubqueryInput answers the seven FOLD-family names
// over a MixedRowShape wideInner — the doubly-nested sibling of
// [lowerFurtherWrapMixedOrSubqueryFoldFn], splitting wideInner by its
// [chplan.MixedDiscriminatorColumn] exactly as that function does (no
// window-purity test needed here either, for the identical reason that
// doc gives: this shape carries no `by`/`without` grouping, so every
// series is homogeneously histogram- or float-typed for its entire
// window), folding the histogram half through
// [lowerExpHistogramFoldOverCallSubqueryInput] and the float half through
// an ordinary OuterRange-mode [chplan.RangeWindow] (ITS OuterRange /
// StepAlign fields already support this grid with no chplan change), then
// recombining via the SAME [combineMixedAggregateBranches] the ambient-grid
// sibling uses — passed stepAligned=true unconditionally rather than
// ctx.step > 0: both branches are ALWAYS multi-row per OUTER-subquery
// anchor here (the grid is sub.Range/step, independent of the ambient
// query's own instant/range mode), unlike the ambient-grid sibling whose
// branches genuinely collapse to one row per series in instant mode.
func lowerMixedFoldOverCallSubqueryInput(wideInner chplan.Node, grid histogramCallSubqueryGrid, windowFn string, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	histSchema := histogramProjectionSchema(s)
	histSchema.AggregationTemporalityColumn = ""
	histBranch, floatBranch := splitMixedRelByDiscriminator(wideInner, histSchema, s)

	histFolded, err := lowerExpHistogramFoldOverCallSubqueryInput(histBranch, grid, windowFn, s, ctx)
	if err != nil {
		return nil, err
	}
	anchor, err := subqueryAnchor(grid.outerSub, ctx)
	if err != nil {
		return nil, err
	}
	floatFolded := &chplan.RangeWindow{
		Input:           floatBranch,
		Func:            windowFn,
		Range:           grid.innerRange,
		OuterRange:      grid.outerSub.Range,
		Step:            grid.step,
		StepAlign:       true,
		End:             anchor.End,
		Offset:          anchor.Offset,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
	return combineMixedAggregateBranches(histFolded, floatFolded, s, true), nil
}

// nestedCallSubqueryShape reports whether expr — a SubqueryExpr's own
// inner expression — is `<fn>(<inner-sub>)`: the doubly-nested shape
// [lowerSubqueryOverCallSubquery] answers for cerberus issue #2726. Used
// to keep an EVEN OUTER range-vector function wrapping such a subquery
// (`<outer-fn2>(<fn>(<inner-sub>)[<outer-range>:<step>])`, a THIRD level
// of nesting) off [lowerHistogramOrMixedSubqueryOuterFnInput]'s dispatch —
// see that function's own guard for why.
func nestedCallSubqueryShape(expr parser.Expr) bool {
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok {
		return false
	}
	arity, matrixArg := subqueryInnerRangeFnShape(call.Func.Name)
	if len(call.Args) != arity {
		return false
	}
	_, ok = peelWrappers(call.Args[matrixArg]).(*parser.SubqueryExpr)
	return ok
}

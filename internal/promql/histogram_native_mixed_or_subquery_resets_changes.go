package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_subquery_resets_changes.go composes
// `resets`/`changes` over a sum/avg-wrapped mixed float/histogram `or`
// subquery — `resets((sum by (series) ((a) or (b)))[range:step])` — the
// second of the three gaps cerberus issue #2615 named as deliberately
// unattempted by this package's sibling
// (histogram_native_mixed_or_subquery_aggregate_range_fn.go's own doc
// comment, "Scope" section).
//
// # Why this needs its own machinery rather than reusing the FOLD family's
//
// Reference Prometheus's funcResets/funcChanges (github.com/tsouza/
// prometheus fork, promql/functions.go) do NOT apply the seven FOLD-family
// names' "drop the whole series on any window mix" rule
// (extrapolatedRate / funcSumOverTime / funcAvgOverTime's
// `len(samples.Histograms) > 0 && len(samples.Floats) > 0` check,
// [windowPurityUnless]'s own reproduction of it). Instead they walk the
// window's float and histogram samples MERGED by timestamp — the exact
// interleave a two-armed matrix iterator performs — and compare every
// CONSECUTIVE pair regardless of which arm either one came from:
//
//	switch {
//	case prevSample.H == nil && curSample.H == nil:
//	    // both float: resets on curr < prev; changes on curr != prev
//	    // (NaN-aware).
//	case prevSample.H != nil && curSample.H == nil, prevSample.H == nil && curSample.H != nil:
//	    // a type FLIP between consecutive samples: always a
//	    // reset/change, unconditionally.
//	case prevSample.H != nil && curSample.H != nil:
//	    // both histogram: resets on curr.H.DetectReset(prev.H); changes
//	    // on !curr.H.Equals(prev.H) — exactly
//	    // [expHistogramResetVerdictExpr] / [expHistogramChangeVerdictExpr].
//	}
//
// So this file needs a type-aware SEQUENTIAL per-pair verdict, not a
// window-wide drop test, and it gets one directly from
// [sumOrAvgOverMixedExpHistogramSetOp]'s ALREADY-Mixed-shaped subquery
// inner ([lowerSumOrAvgOverMixedExpHistogramSetOp] — the SAME per-(group,
// subquery-anchor) relation the four type-blind SELECT-family names
// already fold over, [lowerSumOrAvgMixedOrSubquerySelectFn]) instead of
// this file's FOLD-family sibling's two-separate-branches shape
// ([shadowResolveMixedExpHistogramOperands]): every row already carries
// [chplan.MixedDiscriminatorColumn], which is exactly the "prevSample.H
// == nil" test above, so the per-pair verdict reads it directly instead
// of re-deriving it.
//
// # Why this DOES reach true query_range fan-out, unlike its FOLD-family sibling
//
// The FOLD family's remaining gap (cerberus issue #2615's third item) is
// that [windowPurityUnless] — a window-WIDE VectorSetOp UNLESS — has no
// way to scope itself PER OUTER ANCHOR, which a genuine fan-out (one
// [range] window per output step) needs. This file's own verdict is a
// per-PAIR comparison with no window-wide drop test at all, so it needs
// no such scoping: [chplan.RangeBucketFanout] already reduces each
// (series, anchor) bucket to its own groupArrays independently — exactly
// the shape [lowerSelectFnOverSubqueryRange]'s own resets/changes branch
// already uses for a PURE histogram-native subquery inner (cerberus issue
// #2545/#2569). This file's fan-out mode ([lowerMixedOrSubqueryResetsRange])
// is that same fan-out, fed [mixedPairCountAggs] instead of
// [expHistogramPairCountAggs] so it also collects the Value / discriminator
// arrays the mixed verdict needs. All three grid modes therefore compose
// here, unlike the FOLD family's two.
//
// # Column reuse
//
// [mixedPairCountAggs] widens [expHistogramPairCountAggs] (the SAME
// groupArrays [selectFnOverSubqueryWindowed] already collects for the
// pure-histogram-subquery-inner case, reading through the fixed
// HistogramProjection aliases via histSchema) with two more groupArrays:
// the real Value column and [chplan.MixedDiscriminatorColumn]. Every
// histogram-shaped groupArray a float-typed row contributes is a
// placeholder ([chplan.VectorSetOp.Mixed]'s own doc) — read but never
// SELECTED by [mixedPairVerdictExpr]'s multiIf, since that only reaches
// [expHistogramResetVerdictExpr] / [expHistogramChangeVerdictExpr] for a
// pair whose discriminator marks BOTH rows histogram-typed.

// mixedPairValueArrayAlias / mixedPairDiscrArrayAlias hold the group's
// groupArray of each row's real Value and
// [chplan.MixedDiscriminatorColumn] reading, positionally parallel to
// every array [expHistogramPairCountAggs] already collects (same
// grouping, same row order — see that function's own siblings for why
// parallel groupArrays over one Aggregate stay aligned).
const (
	mixedPairValueArrayAlias = "_mixed_pair_value_list"
	mixedPairDiscrArrayAlias = "_mixed_pair_is_hist_list"
)

// mixedDiscriminatorHistogramValue / mixedDiscriminatorFloatValue mirror
// [chplan.MixedDiscriminatorColumn]'s own two literal values (internal/
// chsql's setOpMixedIsHistogramTrue / setOpMixedIsHistogramFalse, local to
// that package) so this file's per-pair verdict can branch on which arm a
// row came from without importing chsql — chplan declares no chsql
// dependency (.go-arch-lint.yml), and this package (internal/promql)
// consumes chplan, not chsql, directly.
const (
	mixedDiscriminatorHistogramValue = 1
	mixedDiscriminatorFloatValue     = 0
)

// paramMixedOrderedRows / paramMixedRowPos / paramMixedRowTime are
// [mixedPairVerdictExpr]'s own row-ordering lambda parameter names —
// distinct from histogram_native_reset.go's / histogram_native_resets.go's
// sets on purpose, matching the convention each per-pair mask file already
// follows so no reader assumes a shared binding across masks rendered by
// disjoint queries.
const (
	paramMixedOrderedRows = "mwrp"
	paramMixedRowPos      = "mrp"
	paramMixedRowTime     = "mrt"
)

// mixedPairCountAggs is [expHistogramPairCountAggs] (the resets/changes
// groupArray set an all-histogram window already collects, reading
// through histSchema's fixed HistogramProjection aliases) widened by the
// two arrays [mixedPairVerdictExpr] needs beyond it: the group's real
// Value readings and its per-row [chplan.MixedDiscriminatorColumn]
// readings.
func mixedPairCountAggs(windowFn string, histSchema schema.Metrics) []chplan.AggFunc {
	return append(
		expHistogramPairCountAggs(windowFn, histSchema),
		chplan.AggFunc{
			Fn:    chplan.FnGroupArray,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: histSchema.ValueColumn}},
			Alias: mixedPairValueArrayAlias,
		},
		chplan.AggFunc{
			Fn:    chplan.FnGroupArray,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: chplan.MixedDiscriminatorColumn}},
			Alias: mixedPairDiscrArrayAlias,
		},
	)
}

// mixedPairCountExpr reduces the type-aware per-pair mask
// [mixedPairVerdictExpr] renders to the single float reference publishes
// — the number of condemned pairs — the same `arraySum` + Float64 cast
// [expHistogramPairCountExpr] applies to the all-histogram mask.
func mixedPairCountExpr(windowFn string, histSchema schema.Metrics) chplan.Expr {
	mask := mixedPairVerdictExpr(windowFn, histSchema)
	return toFloat64Expr(&chplan.FuncCall{Fn: chplan.FnArraySum, Args: []chplan.Expr{mask}})
}

// mixedPairCountStage is [expHistogramPairCountStage] fed
// [mixedPairCountExpr] instead of the all-histogram
// [expHistogramPairCountExpr] — the type-aware sibling projecting the
// per-series pair count alongside the grouping's own key columns.
func mixedPairCountStage(input chplan.Node, windowFn string, keyAliases []string, histSchema schema.Metrics) chplan.Node {
	projs := make([]chplan.Projection, 0, len(keyAliases)+1)
	for _, name := range keyAliases {
		projs = append(projs, chplan.Projection{Expr: &chplan.ColumnRef{Name: name}, Alias: name})
	}
	return &chplan.Project{
		Input: input,
		Projections: append(projs, chplan.Projection{
			Expr:  mixedPairCountExpr(windowFn, histSchema),
			Alias: histSchema.ValueColumn,
		}),
	}
}

// mixedPairVerdictExpr renders one boolean per consecutive sample pair
// over a Mixed-shaped window, transcribing reference's own three-way
// switch (this file's own top-level doc) as a `multiIf`:
//
//   - both rows histogram-typed → [expHistogramResetVerdictExpr] /
//     [expHistogramChangeVerdictExpr], reused verbatim by naming this
//     lambda's own (prev, curr) parameters identically to what those
//     functions' hardcoded identifiers expect, so the two shapes cannot
//     drift apart about what a hist/hist reset or change IS.
//   - both rows float-typed → [mixedFloatPairVerdictExpr].
//   - one of each (a type FLIP) → always true — reference's
//     unconditional reset/change on a type transition, with no further
//     comparison.
//
// The row-ordering permutation ([hqLet]-bound once as paramMixedOrderedRows)
// is derived from [hqWindowTsListAlias] exactly like every sibling mask —
// [mixedPairCountAggs] collects it as part of the [expHistogramPairCountAggs]
// set it wraps, so it is positionally aligned with the Value / discriminator
// arrays this function reads by the same construction those siblings rely
// on.
func mixedPairVerdictExpr(windowFn string, histSchema schema.Metrics) chplan.Expr {
	prevParam, currParam := paramResetPrevRow, paramResetCurrRow
	histVerdict := expHistogramResetVerdictExpr()
	if windowFn == changesWindowFn {
		prevParam, currParam = paramChangePrevRow, paramChangeCurrRow
		histVerdict = expHistogramChangeVerdictExpr(histSchema)
	}
	prev := chplan.Expr(&chplan.BareIdent{Name: prevParam})
	curr := chplan.Expr(&chplan.BareIdent{Name: currParam})

	discrAt := func(pos chplan.Expr) chplan.Expr {
		return &chplan.Subscript{Container: &chplan.ColumnRef{Name: mixedPairDiscrArrayAlias}, Key: pos}
	}
	isHistAt := func(pos chplan.Expr) chplan.Expr {
		return &chplan.Binary{Op: chplan.OpEq, Left: discrAt(pos), Right: &chplan.LitInt{V: mixedDiscriminatorHistogramValue}}
	}
	isFloatAt := func(pos chplan.Expr) chplan.Expr {
		return &chplan.Binary{Op: chplan.OpEq, Left: discrAt(pos), Right: &chplan.LitInt{V: mixedDiscriminatorFloatValue}}
	}
	bothHist := &chplan.Binary{Op: chplan.OpAnd, Left: isHistAt(prev), Right: isHistAt(curr)}
	bothFloat := &chplan.Binary{Op: chplan.OpAnd, Left: isFloatAt(prev), Right: isFloatAt(curr)}

	body := &chplan.FuncCall{Fn: chplan.FnMultiIf, Args: []chplan.Expr{
		bothHist, histVerdict,
		bothFloat, mixedFloatPairVerdictExpr(windowFn, prev, curr),
		&chplan.LitBool{V: true},
	}}

	tsList := chplan.Expr(&chplan.ColumnRef{Name: hqWindowTsListAlias})
	orderedRows := &chplan.FuncCall{Fn: chplan.FnArraySort, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramMixedRowPos, paramMixedRowTime},
			Body:   &chplan.BareIdent{Name: paramMixedRowTime},
		},
		&chplan.FuncCall{Fn: chplan.FnArrayEnumerate, Args: []chplan.Expr{tsList}},
		tsList,
	}}

	return hqLet(paramMixedOrderedRows, orderedRows, func(rows chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{prevParam, currParam}, Body: body},
			&chplan.FuncCall{Fn: chplan.FnArrayPopBack, Args: []chplan.Expr{rows}},
			&chplan.FuncCall{Fn: chplan.FnArrayPopFront, Args: []chplan.Expr{rows}},
		}}
	})
}

// mixedFloatPairVerdictExpr renders reference's float/float branch for
// ONE consecutive pair: `curr.F < prev.F` for resets (a value DECREASE,
// matching [emitRangeWindowResets]'s own plain-float kernel), `curr.F !=
// prev.F` with the identical both-NaN carve-out
// [expHistogramSumDiffersExpr] applies for changes (matching
// [emitRangeWindowChanges]'s own plain-float kernel and cerberus issue
// #1489). Reusing the SAME two conditions the plain-float RangeWindow
// emitter already renders keeps a float/float pair's verdict identical
// whichever of the two lowerings answers it.
func mixedFloatPairVerdictExpr(windowFn string, prev, curr chplan.Expr) chplan.Expr {
	valAt := func(pos chplan.Expr) chplan.Expr {
		return &chplan.Subscript{Container: &chplan.ColumnRef{Name: mixedPairValueArrayAlias}, Key: pos}
	}
	prevVal, currVal := valAt(prev), valAt(curr)
	if windowFn == changesWindowFn {
		isNaN := func(e chplan.Expr) chplan.Expr { return &chplan.FuncCall{Fn: chplan.FnIsNaN, Args: []chplan.Expr{e}} }
		bothNaN := &chplan.Binary{Op: chplan.OpAnd, Left: isNaN(currVal), Right: isNaN(prevVal)}
		return &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.Binary{Op: chplan.OpNe, Left: currVal, Right: prevVal},
			Right: &chplan.FuncCall{Fn: chplan.FnNot, Args: []chplan.Expr{bothNaN}},
		}
	}
	return &chplan.Binary{Op: chplan.OpLt, Left: currVal, Right: prevVal}
}

// lowerMixedOrSubqueryResetsOrChanges lowers the
// [sumOrAvgMixedOrSubqueryShape] shape for windowFn resets/changes across
// all three grid modes [sumOrAvgMixedOrSubqueryOuterFnRecognized] admits
// for these two names (instant, `@`-pinned broadcast, AND true
// query_range fan-out — see this file's own top-level doc for why the
// third mode is reachable here but not for this file's FOLD-family
// sibling). mixedRel, the subquery's own inner aggregation lowered ONCE
// across the whole grid via the exact existing root-only composer
// ([lowerSumOrAvgOverMixedExpHistogramSetOp]), is the same correct
// per-(group, subquery anchor) Mixed relation
// [lowerSumOrAvgMixedOrSubquerySelectFn] folds over for its own four
// type-blind names.
func lowerMixedOrSubqueryResetsOrChanges(shape sumOrAvgMixedOrSubqueryShape, gridCtx lowerCtx, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	mixedRel, err := lowerSumOrAvgOverMixedExpHistogramSetOp(shape.agg, shape.b, s, gridCtx)
	if err != nil {
		return nil, err
	}
	if chplan.RowShapeOf(mixedRel) != chplan.MixedRowShape {
		return nil, fmt.Errorf("promql: internal invariant violated: sum/avg-mixed-or subquery input is %T with %s row shape", mixedRel, chplan.RowShapeOf(mixedRel))
	}
	return lowerMixedOrSubqueryResetsOrChangesInput(mixedRel, shape.sub, shape.windowFn, s, ctx)
}

// lowerMixedOrSubqueryResetsOrChangesInput is
// [lowerMixedOrSubqueryResetsOrChanges] split at the one point that
// actually varies by caller — see
// [lowerSelectFnOverExpHistogramSubqueryInput]'s identical doc for why.
// Cerberus issue #2724 reuses this continuation for a further
// `and`/`unless`/`or` wrapping a mixed-or subquery inner.
func lowerMixedOrSubqueryResetsOrChangesInput(mixedRel chplan.Node, sub *parser.SubqueryExpr, windowFn string, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}
	histSchema := histogramProjectionSchema(s)
	histSchema.AggregationTemporalityColumn = ""

	if ctx.rangeMode() && !subqueryPinned(sub) {
		return lowerMixedOrSubqueryResetsRange(mixedRel, sub, windowFn, anchor, histSchema, s, ctx), nil
	}

	group := &chplan.Aggregate{
		Input:              mixedRel,
		GroupBy:            []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
		GroupByAliases:     []string{s.AttributesColumn},
		AggFuncs:           append(mixedPairCountAggs(windowFn, histSchema), windowSampleCountAgg(s)),
		DropEmptyOnNoGroup: true,
	}
	windowed := mixedPairCountStage(minSamplesFilter(group, stalenessMinSamples), windowFn, []string{s.AttributesColumn}, histSchema)

	if ctx.rangeMode() && subqueryPinned(sub) {
		grid := &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step}
		return expHistogramPairCountProjection(
			&chplan.CrossJoin{Left: grid, Right: windowed},
			&chplan.ColumnRef{Name: stepGridAnchorColumn},
			s,
		), nil
	}
	tsExpr := chplan.NowNano()
	if !anchor.End.IsZero() {
		tsExpr = windowRightBoundExpr(evalAnchor{End: anchor.End})
	}
	return expHistogramPairCountProjection(windowed, tsExpr, s), nil
}

// lowerMixedOrSubqueryResetsRange is the true query_range fan-out mode:
// one `[sub.Range]` window per output step anchor via
// [chplan.RangeBucketFanout] over mixedRel directly — the mixed-aware
// counterpart of [lowerSelectFnOverSubqueryRange]'s own resets/changes
// branch, which this mirrors rung for rung (same MinSamples floor
// [stalenessMinSamples], same AnchorAlias / TimestampCol wiring) except
// for the AggFuncs set: [mixedPairCountAggs] in place of
// [expHistogramPairCountAggs], so the per-anchor collapse also carries
// the Value / discriminator arrays [mixedPairVerdictExpr] needs.
func lowerMixedOrSubqueryResetsRange(mixedRel chplan.Node, sub *parser.SubqueryExpr, windowFn string, anchor evalAnchor, histSchema, s schema.Metrics, ctx lowerCtx) chplan.Node {
	anchorRef := &chplan.ColumnRef{Name: stepGridAnchorColumn}
	fanout := &chplan.RangeBucketFanout{
		Input:          mixedRel,
		Start:          ctx.start.UTC(),
		End:            ctx.end.UTC(),
		Step:           ctx.step,
		Lookback:       sub.Range,
		Offset:         anchor.Offset,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
		GroupByAliases: []string{s.AttributesColumn},
		AggFuncs:       mixedPairCountAggs(windowFn, histSchema),
		MinSamples:     stalenessMinSamples,
		AnchorAlias:    stepGridAnchorColumn,
		TimestampCol:   s.TimestampColumn,
	}
	perSeries := mixedPairCountStage(fanout, windowFn, []string{stepGridAnchorColumn, s.AttributesColumn}, histSchema)
	return expHistogramPairCountProjection(perSeries, anchorRef, s)
}

package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_subquery_further_setop_range_fn.go answers
// cerberus issue #2724: a SELECT/FOLD-family outer function over a subquery
// whose own inner is a further `and`/`unless`/`or` wrapping a mixed
// float/histogram `or` — `<fn>((((a) or (b)) and/unless/or c)[range:step])`,
// either order — the one gap
// histogram_native_mixed_or_subquery_range_fn.go's own doc names as
// deliberately unattempted by that file's distribute-then-recombine
// mechanism (a first attempt hit cerberus issue #2589's own
// and/unless-forwarded-histogram-subquery-inner rejection one level down;
// see that doc for the full account).
//
// lowerHistogramOrMixedSubqueryOuterFnInput is called from
// [lowerOuterRangeFnOverSubquery], right before its own catch-all
// rejection, with inner ALREADY lowered by the ordinary [lowerSubquery]
// dispatch — [lowerSubqueryOverBinary] (cerberus issue #2589) already
// resolves and/unless-forwarding correctly, and #2555's own nested-Mixed-
// operand handling already resolves a further `or` correctly, so inner is
// already a genuinely correct HistogramRowShape or MixedRowShape relation
// by the time it reaches here — no AST-level recognizer is needed at all,
// only a dispatch on windowFn and the row shape [lowerOuterRangeFnOverSubquery]
// already computed. This is why the fix answers EVERY shape that resolves
// this way, not only the further-and/unless/or one #2724 names: a bare
// and/unless-forwarded histogram selector composes here too, for free.
//
// Every one of the fifteen SELECT/FOLD-family names delegates to the SAME
// continuation the relevant DIRECT recognizer already built, so none of the
// actual window-fold logic is duplicated:
//
//   - count_over_time / present_over_time / ts_of_first_over_time /
//     ts_of_last_over_time (type-blind) and last_over_time / first_over_time /
//     resets / changes over a HistogramRowShape input all reuse
//     [lowerSelectFnOverExpHistogramSubqueryInput] — that continuation
//     already only reads the Attributes/Timestamp/Value columns for the
//     four type-blind names, so a Mixed-shaped input reduces identically to
//     a Histogram-shaped one there too.
//   - last_over_time / first_over_time over a MixedRowShape input reuse
//     [lowerMixedOrSubqueryLastFirstInput] (cerberus issue #2714).
//   - resets / changes over a MixedRowShape input reuse
//     [lowerMixedOrSubqueryResetsOrChangesInput] (cerberus issue #2615).
//   - rate / increase / delta / irate / idelta / sum_over_time /
//     avg_over_time over a HistogramRowShape input reuse
//     [lowerExpHistogramRangeFnOverSubqueryInput]. Over a MixedRowShape
//     input they answer through [lowerFurtherWrapMixedOrSubqueryFoldFn],
//     below — genuinely new machinery, since none of the existing FOLD-
//     family continuations accept an already-COMBINED Mixed relation (the
//     sum/avg-wrapped composer's own FOLD-family lowering keeps the two
//     arms separate the whole way through, precisely so its window-purity
//     test can filter each one before folding).
//
// # A THIRD level of nesting
//
// When sub is ITSELF cerberus issue #2726's doubly-nested shape
// (`<fn>(<inner-sub>)[<outer-range>:<step>]`, [nestedCallSubqueryShape]),
// inner is a per-(series, OUTER-subquery-anchor) already-REDUCED relation
// rather than a raw per-sample one. That is not a reason to refuse it:
// what every continuation below folds over is exactly "the samples sub's
// range vector holds", and for this shape those samples ARE inner's
// per-outer-anchor rows. What the doubly-nested shape does need, and a
// singly-nested one does not, is [widenNestedCallSubqueryInner] — its own
// outer-subquery grid is built against sub's anchor alone
// ([lowerSubqueryOverCallSubquery] leaves the re-anchoring to its caller,
// exactly as the plain-float sibling does), so it must be re-anchored onto
// THIS reduction's window before any continuation reads it. See that
// function for the three grid modes, and for why the classic ambient-grid
// fan-outs buried inside its own wideInner stay untouched (cerberus issue
// #2728).
func lowerHistogramOrMixedSubqueryOuterFnInput(inner chplan.Node, shape chplan.RowShape, windowFn string, sub *parser.SubqueryExpr, s schema.Metrics, ctx lowerCtx) (node chplan.Node, matched bool, err error) {
	if nestedCallSubqueryShape(sub.Expr) {
		// Widening mutates inner in place, so it must not run for a name
		// the switch below leaves unmatched (deriv, predict_linear, …) —
		// those fall through to the caller's own float-only-drop /
		// rejection handling over the UNwidened relation.
		if !histogramSubqueryOuterFnName(windowFn) {
			return nil, false, nil
		}
		if err := widenNestedCallSubqueryInner(inner, sub, ctx); err != nil {
			return nil, false, err
		}
	}
	switch windowFn {
	case countOverTimeWindowFn, presentOverTimeWindowFn, tsOfFirstOverTimeExpHistFn, tsOfLastOverTimeExpHistFn:
		node, err = lowerSelectFnOverExpHistogramSubqueryInput(inner, sub, windowFn, s, ctx)
		return node, true, err
	case lastOverTimeWindowFn, firstOverTimeWindowFn:
		if shape == chplan.HistogramRowShape {
			node, err = lowerSelectFnOverExpHistogramSubqueryInput(inner, sub, windowFn, s, ctx)
			return node, true, err
		}
		node, err = lowerMixedOrSubqueryLastFirstInput(inner, sub, windowFn, s, ctx)
		return node, true, err
	case resetsWindowFn, changesWindowFn:
		if shape == chplan.HistogramRowShape {
			node, err = lowerSelectFnOverExpHistogramSubqueryInput(inner, sub, windowFn, s, ctx)
			return node, true, err
		}
		node, err = lowerMixedOrSubqueryResetsOrChangesInput(inner, sub, windowFn, s, ctx)
		return node, true, err
	case rateWindowFn, increaseWindowFn, deltaWindowFn, irateWindowFn, ideltaWindowFn, sumOverTimeWindowFn, avgOverTimeWindowFn:
		if shape == chplan.HistogramRowShape {
			node, err = lowerExpHistogramRangeFnOverSubqueryInput(inner, sub, windowFn, s, ctx)
			return node, true, err
		}
		node, err = lowerFurtherWrapMixedOrSubqueryFoldFn(inner, sub, windowFn, s, ctx)
		return node, true, err
	default:
		return nil, false, nil
	}
}

// lowerFurtherWrapMixedOrSubqueryFoldFn answers the seven type-preserving
// FOLD-family names over a MixedRowShape inner — the "further and/unless/or"
// composition's own genuinely new machinery. Unlike the sum/avg-wrapped
// composer's identically-named sibling
// (histogram_native_mixed_or_subquery_aggregate_range_fn.go), no
// window-purity test is needed at all: this shape carries no `by`/`without`
// grouping, so every output series maps 1:1 to exactly one source series
// from EITHER of the mixed `or`'s two operands (they are drawn from
// different metrics and so never share a series — the same invariant
// histogram_native_mixed_or_subquery_range_fn.go's own top doc derives),
// meaning a series is homogeneously histogram- or float-typed for its
// ENTIRE window by construction. [splitMixedRelByDiscriminator] partitions
// inner on its own [chplan.MixedDiscriminatorColumn] — a structural no-op
// disjoint split, not a purity test — and each side folds through the
// UNCHANGED single-type continuation
// ([lowerExpHistogramRangeFnOverSubqueryInput] for the histogram side,
// [lowerFloatFoldOverSubqueryInput] for the float side, both already
// three-grid-mode capable) before recombining via
// [combineMixedAggregateBranches]'s own "structural no-op" reuse (the two
// folded branches are disjoint by construction, so its drop-on-collision
// rule never has a collision to drop and every row survives).
func lowerFurtherWrapMixedOrSubqueryFoldFn(mixedRel chplan.Node, sub *parser.SubqueryExpr, windowFn string, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	histSchema := histogramProjectionSchema(s)
	histSchema.AggregationTemporalityColumn = ""
	histBranch, floatBranch := splitMixedRelByDiscriminator(mixedRel, histSchema, s)

	histFolded, err := lowerExpHistogramRangeFnOverSubqueryInput(histBranch, sub, windowFn, s, ctx)
	if err != nil {
		return nil, err
	}
	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}
	floatFolded := lowerFloatFoldOverSubqueryInput(floatBranch, sub, windowFn, anchor, s, ctx)

	return combineMixedAggregateBranches(histFolded, floatFolded, s, ctx.step > 0), nil
}

// splitMixedRelByDiscriminator partitions mixedRel — the fourteen-column
// Mixed contract [chplan.VectorSetOp.Mixed]'s own doc describes — into its
// two single-type halves by [chplan.MixedDiscriminatorColumn], capping each
// to the plain contract its own consumer expects: the histogram half keeps
// the thirteen-column HistogramRowShape contract (the canonical quartet
// plus the nine Histogram*Column fields, matching what
// [lowerExpHistogramRangeFnOverSubqueryInput] already reads via histSchema);
// the float half drops down to the ordinary four-column canonical Sample
// contract [lowerFloatFoldOverSubqueryInput] expects. Every row in mixedRel
// already carries all fourteen columns regardless of which arm it came from
// ([chplan.VectorSetOp.Mixed]'s own placeholder convention), so this is a
// plain Filter + explicit-column Project on each side — no new chplan or
// chsql surface.
func splitMixedRelByDiscriminator(mixedRel chplan.Node, histSchema, s schema.Metrics) (histBranch, floatBranch chplan.Node) {
	isHist := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: chplan.MixedDiscriminatorColumn},
		Right: &chplan.LitInt{V: mixedDiscriminatorHistogramValue},
	}
	isFloat := &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: chplan.MixedDiscriminatorColumn},
		Right: &chplan.LitInt{V: mixedDiscriminatorFloatValue},
	}

	histProjs := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
		{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
		{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		{Expr: &chplan.ColumnRef{Name: histSchema.CountColumn}, Alias: histSchema.CountColumn},
		{Expr: &chplan.ColumnRef{Name: histSchema.SumColumn}, Alias: histSchema.SumColumn},
		{Expr: &chplan.ColumnRef{Name: histSchema.ScaleColumn}, Alias: histSchema.ScaleColumn},
		{Expr: &chplan.ColumnRef{Name: histSchema.ZeroCountColumn}, Alias: histSchema.ZeroCountColumn},
	}
	if histSchema.ZeroThresholdColumn != "" {
		histProjs = append(histProjs, chplan.Projection{
			Expr: &chplan.ColumnRef{Name: histSchema.ZeroThresholdColumn}, Alias: histSchema.ZeroThresholdColumn,
		})
	}
	histProjs = append(
		histProjs,
		chplan.Projection{Expr: &chplan.ColumnRef{Name: histSchema.PositiveOffsetColumn}, Alias: histSchema.PositiveOffsetColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: histSchema.PositiveBucketCountsColumn}, Alias: histSchema.PositiveBucketCountsColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: histSchema.NegativeOffsetColumn}, Alias: histSchema.NegativeOffsetColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: histSchema.NegativeBucketCountsColumn}, Alias: histSchema.NegativeBucketCountsColumn},
	)
	histBranch = &chplan.Project{
		Input:       &chplan.Filter{Input: mixedRel, Predicate: isHist},
		Projections: histProjs,
	}

	floatBranch = &chplan.Project{
		Input: &chplan.Filter{Input: mixedRel, Predicate: isFloat},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
	return histBranch, floatBranch
}

// lowerFloatFoldOverSubqueryInput folds input — an ordinary four-column
// canonical Sample relation, already scoped to exactly the window this
// reduction needs — with windowFn over sub.Range, across all three grid
// modes. Structurally [lowerFloatFoldOverPureSubqueryBranch]'s
// (histogram_native_mixed_or_subquery_aggregate_range_fn.go) own
// single-window logic widened by [applyStepGridFanout] for the true
// fan-out mode — the identical widening [mixedOrSubqueryFloatRangeWindow]
// (cerberus issue #2715) already applies for its own FOLD-family fan-out.
func lowerFloatFoldOverSubqueryInput(input chplan.Node, sub *parser.SubqueryExpr, windowFn string, anchor evalAnchor, s schema.Metrics, ctx lowerCtx) chplan.Node {
	rw := &chplan.RangeWindow{
		Input:           input,
		Func:            windowFn,
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
	if ctx.rangeMode() {
		applyStepGridFanout(rw, ctx)
		return rw
	}
	if !anchor.End.IsZero() {
		rw.End = anchor.End
	}
	return rw
}

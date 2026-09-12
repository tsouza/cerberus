package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_subquery_outer_aggregate_fold.go composes
// `sum`/`avg` [by/without] wrapping a FOLD-family range function over a
// subquery whose OWN inner is a bare mixed float/histogram `or` —
// `sum(rate((a or b)[range:step]))` — cerberus issue #3265 Part 2.
//
// This is the mirror image of this package's other subquery/aggregate
// composers, and easy to confuse with them:
//
//   - histogram_native_mixed_or_subquery_further_setop_range_fn.go answers
//     a FOLD wrapping a mixed-or subquery with NO outer aggregation at all
//     (`rate((a or b)[range:step])` alone) —
//     [lowerFurtherWrapMixedOrSubqueryFoldFn] recombines the two folded
//     branches with `or`'s own left-biased union
//     ([combineMixedFoldBranches]), since there is no grouping aggregate to
//     apply a collision-drop rule for.
//   - histogram_native_mixed_or_subquery_aggregate_range_fn.go answers a
//     FOLD wrapping a subquery whose inner is ALREADY `sum`/`avg` wrapping a
//     mixed `or` (`rate((sum by(x) (a or b))[range:step])`) — the
//     aggregation is INSIDE the subquery, so [combineMixedAggregateBranches]
//     applies at the SUBQUERY's own grid, before this file's outer fold ever
//     runs.
//
// This file is neither: here the aggregation is OUTSIDE the fold, and the
// fold's own subquery inner is the bare mixed `or` with NO aggregation
// inside it. Before this file existed, `sum(rate((a or b)[range:step]))`
// matched no histogram-native recognizer at all — [rangeFnOverExpHistogramSubquery]
// requires its subquery's inner to be PURELY histogram-valued
// ([isExpHistogramValuedShape]), which a mixed `or` never is — so the whole
// expression fell through to the ordinary, non-histogram-aware aggregate
// path: `Value` read from whatever plan the mixed-or fold happened to
// publish, silently mixing a real float rate with a histogram row's
// placeholder-zero `Value` (or, with no `by`/`without` clause, silently
// discarding the histogram side of the sum entirely — reference instead
// raises `vector cannot contain metrics with the same labelset` when a
// `by`/`without` clause maps both arms onto the SAME group, or drops a
// GROUP whose members disagree on value type when they do not, per
// [combineMixedAggregateBranches]'s own doc).
//
// # Composition
//
// Sound for the identical reason [lowerSumOrAvgOverMixedExpHistogramSetOp]
// (this same family's ROOT-only, no-subquery composer) is: fold each of the
// mixed `or`'s two shadow-resolved arms independently over the OUTER fold's
// window, THEN apply the outer `sum`/`avg` to each folded branch
// separately, THEN recombine with [combineMixedAggregateBranches]'s
// drop-on-collision rule — never fold the ALREADY-unioned Mixed relation and
// apply the outer aggregation to that, which is exactly the shape with no
// type-aware treatment at all.
//
//  1. [shadowResolveMixedExpHistogramOperands] lowers b's two operands and
//     resolves the `or`'s own shadow rule, at the SUBQUERY's own grid
//     (gridCtx) — the identical helper
//     [lowerSumOrAvgMixedOrSubqueryFoldFn] (this package's aggregate-inside-
//     the-subquery sibling) uses for the same purpose.
//  2. Each shadow-resolved arm folds over sub.Range through the SAME
//     per-arm continuations [lowerFurtherWrapMixedOrSubqueryFoldFn] uses:
//     [lowerExpHistogramRangeFnOverSubqueryInput] for the histogram side,
//     [lowerFloatFoldOverSubqueryInput] for the float side — both already
//     three-grid-mode capable (instant, `@`-pinned broadcast, true
//     query_range fan-out), so this composer inherits all three for free.
//  3. The OUTER `agg` (sum/avg [by/without]) reduces each folded branch
//     independently — [lowerExpHistogramSumOrAvgOverPlan] for the histogram
//     branch (the SAME cross-series merge `sum(rate(m_exp_hist[5m]))`
//     already uses), [lowerPlainAggOverMixedFloatArm] for the float branch
//     (the SAME CH-native aggregate the root-only composer's float arm
//     uses) — exactly mirroring [lowerSumOrAvgOverMixedExpHistogramSetOp]'s
//     own three-and-four steps, just over already-folded input instead of a
//     freshly lowered selector.
//  4. [combineMixedAggregateBranches] recombines the two REDUCED branches
//     with reference's drop-on-collision rule, identically to every other
//     sibling composer in this family.
//
// No new chplan or chsql surface: every node built here is one this
// package's other mixed-or composers already build.
func sumOrAvgOverMixedOrSubqueryFoldFn(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (agg *parser.AggregateExpr, sub *parser.SubqueryExpr, windowFn string, b *parser.BinaryExpr, ok bool) {
	agg, ok = unwrapAggregateExpr(expr)
	if !ok || !expHistogramAggOpIsMergeable(agg.Op) || agg.Param != nil {
		return nil, nil, "", nil, false
	}
	call, ok := peelWrappers(agg.Expr).(*parser.Call)
	if !ok || len(call.Args) != 1 {
		return nil, nil, "", nil, false
	}
	switch call.Func.Name {
	case rateWindowFn, increaseWindowFn, deltaWindowFn, irateWindowFn, ideltaWindowFn, sumOverTimeWindowFn, avgOverTimeWindowFn:
	default:
		return nil, nil, "", nil, false
	}
	sub, ok = peelWrappers(call.Args[0]).(*parser.SubqueryExpr)
	if !ok || sub.Range <= 0 || !subqueryHasEvalAnchor(sub, ctx) {
		return nil, nil, "", nil, false
	}
	b, ok = mixedExpHistogramSetOp(sub.Expr, s, ctx)
	if !ok {
		return nil, nil, "", nil, false
	}
	return agg, sub, call.Func.Name, b, true
}

// lowerSumOrAvgOverMixedOrSubqueryFoldFn lowers the shape
// [sumOrAvgOverMixedOrSubqueryFoldFn] recognised. See this file's header for
// the four-stage composition.
func lowerSumOrAvgOverMixedOrSubqueryFoldFn(agg *parser.AggregateExpr, sub *parser.SubqueryExpr, windowFn string, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}
	if sub.Step < 0 {
		return nil, fmt.Errorf("promql: subquery step must be positive, got %s", sub.Step)
	}
	step := sub.Step
	if step == 0 {
		step = defaultSubqueryStep
	}
	gridCtx, state, err := subqueryGridCtx(sub, step, ctx)
	if err != nil {
		return nil, err
	}
	if state == subqueryGridUnavailable {
		return nil, fmt.Errorf("promql: histogram-valued subquery requires query eval-time context (use LowerAt)")
	}

	histForAgg, floatForAgg, err := shadowResolveMixedExpHistogramOperands(b, s, gridCtx)
	if err != nil {
		return nil, err
	}

	histFolded, err := lowerExpHistogramRangeFnOverSubqueryInput(histForAgg, sub, windowFn, s, ctx)
	if err != nil {
		return nil, err
	}
	anchor, err := subqueryAnchor(sub, ctx)
	if err != nil {
		return nil, err
	}
	floatFolded := lowerFloatFoldOverSubqueryInput(floatForAgg, sub, windowFn, anchor, s, ctx)

	histBranch, err := lowerExpHistogramSumOrAvgOverPlan(agg, histFolded, s, ctx.resourceBounds.HistogramMergeMaxCostUnits)
	if err != nil {
		return nil, err
	}
	floatBranch, err := lowerPlainAggOverMixedFloatArm(agg, floatFolded, s, ctx)
	if err != nil {
		return nil, err
	}

	node := combineMixedAggregateBranches(histBranch, floatBranch, s, ctx.step > 0)
	// Mirrors [lowerExpHistogramRangeFnOverSubquery]'s own cap: every
	// windowFn this recognizer admits is a per-series fold over the
	// subquery's own grid, so an empty grid carries no series into either
	// branch and the reduced, recombined result is empty regardless —
	// capping here is the same answer as capping the matrix and folding.
	if state == subqueryGridEmpty {
		node = emptySubqueryGrid(node)
	}
	return node, nil
}

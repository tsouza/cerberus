package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_aggregate_topk.go lowers `topk`/`bottomk`
// [by/without] wrapping a mixed float/histogram `or` — cerberus issue
// #2600, the last of cerberus issue #2595's three siblings
// (histogram_native_mixed_or_aggregate_float_only.go's own doc comment
// named `topk`/`bottomk`/`quantile` as needing bespoke K-selection
// machinery rather than a small addition to the generic buildAggFunc
// dispatch that file's four ops and #2595's other two sibling files
// reuse). `quantile()` turned out to share the float-only family's exact
// shape (see that file's doc comment) and was folded in there instead;
// this file is topk/bottomk alone.
//
// Reference Prometheus (promql/engine.go's aggregationK) gives topk/
// bottomk the IDENTICAL "drop every histogram sample" rule as min/max/
// stddev/stdvar/quantile — verified directly against
// github.com/tsouza/prometheus's promql/engine.go:
//
//	case parser.TOPK:
//	    switch {
//	    case s.H != nil:
//	        // Ignore histogram sample and add info annotation.
//	        annos.Add(annotations.NewHistogramIgnoredInAggregationInfo("topk", ...))
//	    case int64(len(group.heap)) < k:
//	        heap.Push(&group.heap, &s)
//	    ...
//
// but topk/bottomk are RANK/SELECT operators, not REDUCE operators: they
// pick K rows out of the input and forward those rows' own labels and
// values unchanged, rather than folding every row down to one value per
// group. That is exactly [chplan.TopK]'s own shape ([lowerTopK]'s CH
// `LIMIT K BY <partition>` translation) — a plan node that reads an
// arbitrary already-lowered `chplan.Node` as its Input, entirely
// independent of how that Input was produced. So the correct composition
// needs NO reduction machinery at all, and — unlike SUM/AVG's collision-
// drop recombine or even the float-only family's single-arm reduce — it
// does not need to build anything histogram-shaped either:
//
//  1. [shadowResolveMixedExpHistogramOperands] resolves the `or`'s own
//     shadow rule for both arms exactly as every other mixed-or aggregate
//     composition in this package does. The histogram-valued return
//     (`_` below) is unused: reference's aggregationK drops every
//     histogram sample from K-selection UNCONDITIONALLY, so a histogram
//     row can never survive into the topk/bottomk output regardless of
//     which side of the `or` it shadow-resolved to keep. Concretely: if a
//     label signature collides across the `or`'s two arms, the SAME
//     signature is absent from [floatForAgg] whichever side "wins" the
//     collision — the LHS-histogram-wins case shadows the float row out
//     of floatForAgg via the UNLESS below, and the LHS-float-wins case
//     never had a competing histogram row in floatForAgg's own selector
//     to begin with. A label signature present ONLY on the histogram
//     side never appears in floatForAgg either way (it never had a float
//     counterpart), matching reference dropping that row from K-selection
//     with no candidate to replace it. So floatForAgg alone — with no
//     histogram branch and no post-hoc recombine — already IS the set of
//     rows topk/bottomk could ever select from.
//  2. [buildTopKLiteral] / [buildTopKComputed] (lower.go) apply the
//     ordinary topk/bottomk K-domain, partition and column-shape rules to
//     floatForAgg exactly as they do for a freshly lowered `a.Expr` —
//     these two functions were split out of [lowerTopK] /
//     [lowerTopKComputed] for exactly this reuse.
func topKOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.AggregateExpr, *parser.BinaryExpr, bool) {
	agg, ok := unwrapAggregateExpr(expr)
	if !ok || (agg.Op != parser.TOPK && agg.Op != parser.BOTTOMK) {
		return nil, nil, false
	}
	b, ok := mixedExpHistogramSetOp(agg.Expr, s, ctx)
	if !ok {
		return nil, nil, false
	}
	return agg, b, true
}

// lowerTopKOverMixedExpHistogramSetOp lowers the shape
// [topKOverMixedExpHistogramSetOp] recognised. See this file's header
// for why the shadow-resolved float arm alone, fed through the ordinary
// topk/bottomk K-selection, already answers reference's semantics.
func lowerTopKOverMixedExpHistogramSetOp(agg *parser.AggregateExpr, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}

	_, floatForAgg, err := shadowResolveMixedExpHistogramOperands(b, s, ctx)
	if err != nil {
		return nil, err
	}

	kF, ok := tryScalarLiteral(agg.Param)
	if !ok {
		return buildTopKComputed(agg, s, ctx, floatForAgg, false, false)
	}
	k, empty, err := topKDomain(kF)
	if err != nil {
		return nil, err
	}
	return buildTopKLiteral(agg, s, ctx, floatForAgg, k, empty), nil
}

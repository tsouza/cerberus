package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_aggregate_float_only.go lowers
// `min`/`max`/`stddev`/`stdvar` [by/without] wrapping a mixed
// float/histogram `or` — cerberus issue #2595, the second of the
// sum/avg-wrapped composition's (histogram_native_mixed_or_aggregate.go,
// cerberus issue #2346) deliberately-unattempted sibling aggregation ops.
// `quantile()` shares the identical reference behaviour these four ops
// have (see below) but takes a Param, so it is out of THIS issue's scope
// (its own Acceptance section names only these four plus `count`/`group`/
// `count_values`, which the two sibling files in this package answer).
//
// Reference Prometheus (promql/engine.go's aggregation()) DROPS a
// histogram sample outright for every one of these four ops, with an
// info-level annotation rather than an error or a whole-group drop:
//
//	case parser.MIN, parser.MAX:
//	    if h != nil {
//	        group.seen = false
//	        annos.Add(annotations.NewHistogramIgnoredInAggregationInfo(...))
//	    }
//	...
//	case parser.STDVAR, parser.STDDEV:
//	    switch {
//	    case h != nil:
//	        // Ignore histograms for STDVAR and STDDEV.
//	        group.seen = false
//	        annos.Add(annotations.NewHistogramIgnoredInAggregationInfo(...))
//
// Unlike SUM/AVG (which merge same-type histogram groups and drop only
// the groups that mix types) and unlike COUNT/GROUP
// (histogram_native_mixed_or_aggregate_presence.go's sibling file, which
// never look at the sample's type at all), these four ops NEVER read a
// histogram sample's value — every histogram-shaped row a mixed `or`
// produces is unconditionally excluded from the result, at every group,
// regardless of whether that group also has float members. So the
// correct composition needs no histogram-side branch and no
// "drop on collision" recombine at all: reduce the `or`'s shadow-resolved
// FLOAT arm alone with the ordinary CH-native aggregate, exactly the way
// [lowerPlainAggOverMixedFloatArm] already does for SUM/AVG's float
// branch — just without a histogram branch to build or recombine with.
//
//  1. [shadowResolveMixedExpHistogramOperands] (histogram_native_mixed_or_
//     aggregate.go) lowers the `or`'s two operands and resolves the shadow
//     rule for both — the histogram-valued half is computed but simply
//     discarded here, since these four ops have no use for it.
//  2. [lowerPlainAggOverMixedFloatArm] applies the ordinary
//     min/max/stddev/stdvar CH-native aggregate directly over the
//     shadow-resolved float arm. A group whose only surviving member (post
//     shadow-resolution) is the discarded histogram row never reaches this
//     step, so it produces no output — matching reference's "group never
//     seen" outcome for an all-histogram group under these four ops.
func floatOnlyAggOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.AggregateExpr, *parser.BinaryExpr, bool) {
	agg, ok := unwrapAggregateExpr(expr)
	if !ok || !expHistogramAggDropsHistogramSamples(agg.Op) || agg.Param != nil {
		return nil, nil, false
	}
	b, ok := mixedExpHistogramSetOp(agg.Expr, s, ctx)
	if !ok {
		return nil, nil, false
	}
	return agg, b, true
}

// expHistogramAggDropsHistogramSamples reports whether op is one of the
// four aggregations reference Prometheus reduces by unconditionally
// ignoring every histogram sample (ops which are the identical shape
// [floatOnlyAggOverMixedExpHistogramSetOp] recognises over a mixed `or`
// aggregand) — see this file's header for the citation. Distinct from
// [expHistogramAggOpIsMergeable] (histogram_quantile.go): that predicate
// answers "does this op MERGE same-type histogram groups" (true only for
// SUM/AVG), a different question from "does this op drop every histogram
// sample outright" (true for these four, false for COUNT/GROUP, which
// never inspect the sample's type at all —
// histogram_native_mixed_or_aggregate_presence.go).
func expHistogramAggDropsHistogramSamples(op parser.ItemType) bool {
	switch op {
	case parser.MIN, parser.MAX, parser.STDDEV, parser.STDVAR:
		return true
	}
	return false
}

// lowerFloatOnlyAggOverMixedExpHistogramSetOp lowers the shape
// [floatOnlyAggOverMixedExpHistogramSetOp] recognised. See this file's
// header for the two-stage reduction.
func lowerFloatOnlyAggOverMixedExpHistogramSetOp(agg *parser.AggregateExpr, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}

	_, floatForAgg, err := shadowResolveMixedExpHistogramOperands(b, s, ctx)
	if err != nil {
		return nil, err
	}
	return lowerPlainAggOverMixedFloatArm(agg, floatForAgg, s, ctx)
}

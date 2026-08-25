package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_aggregate_presence.go lowers `count`/`group`
// [by/without] wrapping a mixed float/histogram `or` — cerberus issue
// #2595, the first of the sum/avg-wrapped composition's
// (histogram_native_mixed_or_aggregate.go, cerberus issue #2346) six
// deliberately-unattempted sibling aggregation ops.
//
// Reference Prometheus (promql/engine.go's aggregation()) treats `count`
// and `group` as entirely TYPE-AGNOSTIC — neither op ever inspects
// whether a group member is a float or a native histogram sample:
//
//	case parser.COUNT:
//	    group.groupCount++
//
//	case parser.GROUP:
//	    // Do nothing. Required to avoid the panic in `default:` below.
//	    ...
//	    group.floatValue = 1
//
// There is no `if h != nil` guard anywhere in either op's path and no
// `MixedFloatsHistogramsAggWarning` — that annotation is SUM/AVG's own
// (a mixed-type group is a genuine ambiguity only for an op that has to
// combine the VALUES; `count`/`group` never read a value, so a group
// mixing a histogram sample and a float sample is no different from one
// with two float samples). A sample is a sample; both ops count/mark
// EVERY row a mixed `or` produces, never dropping either arm.
//
// That makes the correct composition simpler than SUM/AVG's four-stage
// reduction (histogram_native_mixed_or_aggregate.go's own header): there
// is no histogram-side merge and no "drop a group present on both
// branches" recombine, because there is only ONE branch — the `or`'s own
// shadow-resolved union, unmodified, aggregated directly:
//
//  1. [lowerMixedExpHistogramSetOp] — literally the SAME leaf lowering
//     the bare `(a) or (b)` shape uses — lowers the `or`'s two operands
//     and resolves its shadow rule (LHS keeps every row; RHS keeps only
//     the rows whose label signature is absent from LHS), all inside the
//     single Mixed [chplan.VectorSetOp] union its own
//     [chplan.VectorSetOp.Mixed] contract already encodes. Reusing it
//     here — rather than re-deriving the two arms via
//     [shadowResolveMixedExpHistogramOperands] and recombining them by
//     hand — is exactly what [lowerCountOrGroupOverMixedExpHistogramSetOp]
//     below builds on: no new shadow-resolution logic at all.
//  2. [lowerPlainAggOverMixedFloatArm] (histogram_native_mixed_or_aggregate.go)
//     applies the ordinary COUNT/GROUP CH-native aggregate directly over
//     that union. Its own doc comment covers why passing it a Mixed node
//     is safe for exactly these two ops: COUNT reads `count(Value)`,
//     and [chplan.VectorSetOp.Mixed] publishes a non-NULL Value on every
//     row — the real magnitude on float-shaped rows, the
//     [histogramSampleValuePlaceholder] `0.0` on histogram-shaped rows —
//     so the count is correct regardless of which placeholder some rows
//     carry; GROUP never reads Value at all
//     (`any(toFloat64(1))`, buildAggFunc's own [chplan.AggFunc]).
func countOrGroupOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.AggregateExpr, *parser.BinaryExpr, bool) {
	agg, ok := unwrapAggregateExpr(expr)
	if !ok || (agg.Op != parser.COUNT && agg.Op != parser.GROUP) || agg.Param != nil {
		return nil, nil, false
	}
	b, ok := mixedExpHistogramSetOp(agg.Expr, s, ctx)
	if !ok {
		return nil, nil, false
	}
	return agg, b, true
}

// lowerCountOrGroupOverMixedExpHistogramSetOp lowers the shape
// [countOrGroupOverMixedExpHistogramSetOp] recognised. See this file's
// header for the two-stage reduction.
func lowerCountOrGroupOverMixedExpHistogramSetOp(agg *parser.AggregateExpr, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if b.ReturnBool {
		return nil, fmt.Errorf("promql: 'bool' modifier is only allowed on comparison binary ops")
	}

	unioned, err := lowerMixedExpHistogramSetOp(b, s, ctx)
	if err != nil {
		return nil, err
	}
	return lowerPlainAggOverMixedFloatArm(agg, unioned, s, ctx)
}

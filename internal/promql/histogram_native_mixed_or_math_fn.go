package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Math functions use Prometheus's simpleFloatFunc/clamp rule: discard
// histogram samples after union shadowing, then transform real float values.
// This root-only adapter preserves that admission boundary; nested math does
// not gain admission merely because the shared kernel can handle its payload.
func mathCallOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, *parser.BinaryExpr, bool) {
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok {
		return nil, nil, false
	}
	switch call.Func.Name {
	case "clamp_min", "clamp_max":
		if len(call.Args) != 2 {
			return nil, nil, false
		}
	case "clamp":
		if len(call.Args) != 3 {
			return nil, nil, false
		}
	case "round":
		if len(call.Args) != 1 && len(call.Args) != 2 {
			return nil, nil, false
		}
	default:
		if _, known := instantFnCH[call.Func.Name]; !known || len(call.Args) != 1 {
			return nil, nil, false
		}
	}
	b, ok := mixedExpHistogramSetOp(call.Args[0], s, ctx)
	if !ok {
		return nil, nil, false
	}
	return call, b, true
}

func lowerMathCallOverMixedExpHistogramSetOp(call *parser.Call, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	// The policy must authorize this root before the union lowerer runs.
	inner, err := prepareMixedMathOperand(mixedRootAdmission, func() (chplan.Node, error) {
		return lowerMixedExpHistogramSetOp(b, s, ctx)
	})
	if err != nil {
		return nil, err
	}
	return lowerMathCall(call, s, ctx, instantFnCH[call.Func.Name],
		func() (chplan.Node, error) { return inner, nil }, directCanonical)
}

// prepareMixedMathOperand implements the table's payload mode, not merely
// admission. It retains the union before strict narrowing, so a histogram on
// the left still shadows a colliding float on the right. Existing-plan callers
// invoke this only for Mixed inputs and before adding a bounds Filter.
func prepareMixedMathOperand(site mixedAdmissionSite, load mathOperandLoader) (chplan.Node, error) {
	prepare, err := mathPayloadPreparation(site)
	if err != nil {
		return nil, err
	}
	inner, err := load()
	if err != nil {
		return nil, err
	}
	return prepare(inner), nil
}

// mathPayloadPreparation resolves executable payload behavior. An admission
// check may retain this policy without running it for a terminal empty result.
func mathPayloadPreparation(site mixedAdmissionSite) (func(chplan.Node) chplan.Node, error) {
	key := mixedWrapperKey{family: mixedMathFamily, site: site}
	switch mixedOperandPolicies[key] {
	case mixedFloatOnly:
		return mixedRowsFloatOnly, nil
	default:
		return nil, fmt.Errorf("promql: mixed operand is not admitted for %s at %s", key.family, key.site)
	}
}

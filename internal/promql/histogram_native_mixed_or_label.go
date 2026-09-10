package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Label rewrites preserve both sample kinds: Prometheus changes labels without
// reading or transforming a sample's value or histogram. Root admission remains
// separate from recursive operand admission. The mixed union retains its own
// shadowing before the shared label kernel forwards payload roles unchanged.
func labelCallOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, *parser.BinaryExpr, bool) {
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok {
		return nil, nil, false
	}
	switch call.Func.Name {
	case fnLabelReplace:
		if len(call.Args) != 5 {
			return nil, nil, false
		}
	case fnLabelJoin:
		if len(call.Args) < 3 {
			return nil, nil, false
		}
	default:
		return nil, nil, false
	}
	b, ok := mixedExpHistogramSetOp(call.Args[0], s, ctx)
	if !ok {
		return nil, nil, false
	}
	return call, b, true
}

func lowerLabelCallOverMixedExpHistogramSetOp(call *parser.Call, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	// Preserve policy-before-validation, then validation-before-operand order.
	if _, err := labelPayloadPolicy(mixedRootAdmission); err != nil {
		return nil, err
	}
	return lowerLabelCall(call, s, func() (chplan.Node, error) {
		return lowerMixedExpHistogramSetOp(b, s, ctx)
	})
}

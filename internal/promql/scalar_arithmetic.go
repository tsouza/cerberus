package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Scalar arithmetic has two established projection boundaries. Direct mixed
// roots materialize the canonical aliases; ordinary operands retain their
// temporal envelope and name-drop collision guard. Neither choice admits a new
// operand shape or changes how the operand's set operation resolves shadowing.
type scalarArithmeticBoundary uint8

const (
	scalarArithmeticGuarded scalarArithmeticBoundary = iota
	scalarArithmeticCanonical
)

func scalarBinaryValue(value chplan.Expr, op chplan.BinaryOp, scalar float64, scalarOnLeft bool) chplan.Expr {
	return scalarBinaryValueExpr(value, op, &chplan.LitFloat{V: scalar}, scalarOnLeft)
}

func scalarBinaryValueExpr(value chplan.Expr, op chplan.BinaryOp, scalar chplan.Expr, scalarOnLeft bool) chplan.Expr {
	left, right := value, scalar
	if scalarOnLeft {
		left, right = right, left
	}
	return &chplan.Binary{Op: op, Left: left, Right: right}
}

func finishScalarArithmetic(inner chplan.Node, arg parser.Expr, s schema.Metrics, ctx lowerCtx,
	op chplan.BinaryOp, scalar float64, scalarOnLeft bool, boundary scalarArithmeticBoundary,
) (chplan.Node, error) {
	if boundary == scalarArithmeticCanonical {
		if err := requireMixedOperandPolicy(mixedArithmeticFamily, mixedRootAdmission, mixedFloatOnly); err != nil {
			return nil, err
		}
	} else if mixedRowsNeedPreparation(inner) {
		if err := requireMixedOperandPolicy(mixedArithmeticFamily, mixedPlanAdmission, mixedFloatOnly); err != nil {
			return nil, err
		}
	}
	inner = mixedRowsFloatOnly(inner)
	layout := sampleProjectionLayout{canonical: true, materializeAliases: true}
	if boundary == scalarArithmeticGuarded {
		inner = guardNameDropCollision(inner, arg, s, ctx)
		layout = derivedSampleProjectionLayout(inner)
	}
	return projectValueOverInner(inner, s, layout, func(refs sampleRoleRefs) chplan.Expr {
		return scalarBinaryValue(refs.Value, op, scalar, scalarOnLeft)
	}), nil
}

package promql

import (
	"fmt"

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

func requireFloatArithmeticPolicy(site mixedAdmissionSite) error {
	key := mixedWrapperKey{family: mixedArithmeticFamily, site: site}
	if mixedOperandPolicies[key] != mixedFloatOnly {
		return fmt.Errorf("promql: mixed operand is not admitted for %s at %s", key.family, key.site)
	}
	return nil
}

// lowerArithmeticRoot checks authority before the original union loader runs.
// The loader keeps its histogram-first error order and completes shadow
// resolution before the common finalizer discards histogram rows.
func lowerArithmeticRoot(build func() (chplan.Node, error)) (chplan.Node, error) {
	if err := requireFloatArithmeticPolicy(mixedRootAdmission); err != nil {
		return nil, err
	}
	return build()
}

func scalarArithmeticValue(value chplan.Expr, op chplan.BinaryOp, scalar float64, scalarOnLeft bool) chplan.Expr {
	var left, right chplan.Expr = value, &chplan.LitFloat{V: scalar}
	if scalarOnLeft {
		left, right = right, left
	}
	return &chplan.Binary{Op: op, Left: left, Right: right}
}

func finishScalarArithmetic(inner chplan.Node, arg parser.Expr, s schema.Metrics, ctx lowerCtx,
	op chplan.BinaryOp, scalar float64, scalarOnLeft bool, boundary scalarArithmeticBoundary,
) (chplan.Node, error) {
	if boundary != scalarArithmeticGuarded && boundary != scalarArithmeticCanonical {
		return nil, fmt.Errorf("promql: unknown scalar arithmetic projection boundary %d", boundary)
	}
	if boundary == scalarArithmeticCanonical {
		if err := requireFloatArithmeticPolicy(mixedRootAdmission); err != nil {
			return nil, err
		}
	}
	if boundary == scalarArithmeticGuarded && chplan.RowShapeOf(inner) == chplan.MixedRowShape {
		if err := requireFloatArithmeticPolicy(mixedPlanAdmission); err != nil {
			return nil, err
		}
	}
	inner = mixedRowsFloatOnly(inner)
	layout := sampleProjectionLayout{canonical: true, materializeAliases: true}
	if boundary == scalarArithmeticGuarded {
		inner = guardNameDropCollision(inner, arg, s, ctx)
		layout = legacySampleProjectionLayout(inner)
	}
	return projectValueOverInner(inner, s, layout, func(refs sampleRoleRefs) chplan.Expr {
		return scalarArithmeticValue(refs.Value, op, scalar, scalarOnLeft)
	}), nil
}

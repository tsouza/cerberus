package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Scalar comparisons retain their established boundary: direct mixed roots
// materialize a canonical quartet, while ordinary non-bool comparisons only
// filter and ordinary bool comparisons retain their collision guard and layout.
type scalarComparisonBoundary uint8

const (
	scalarComparisonGuarded scalarComparisonBoundary = iota
	scalarComparisonCanonical
)

func requireFloatComparisonPolicy(site mixedAdmissionSite) error {
	key := mixedWrapperKey{family: mixedComparisonFamily, site: site}
	if mixedOperandPolicies[key] != mixedFloatOnly {
		return fmt.Errorf("promql: mixed operand is not admitted for %s at %s", key.family, key.site)
	}
	return nil
}

// lowerComparisonRoot authorizes the route before the original union loader,
// preserving its error order and histogram/float shadow resolution.
func lowerComparisonRoot(build func() (chplan.Node, error)) (chplan.Node, error) {
	if err := requireFloatComparisonPolicy(mixedRootAdmission); err != nil {
		return nil, err
	}
	return build()
}

func finishScalarComparison(inner chplan.Node, arg parser.Expr, s schema.Metrics, ctx lowerCtx,
	op chplan.BinaryOp, scalar float64, scalarOnLeft, returnBool bool, boundary scalarComparisonBoundary,
) (chplan.Node, error) {
	if boundary != scalarComparisonGuarded && boundary != scalarComparisonCanonical {
		return nil, fmt.Errorf("promql: unknown scalar comparison projection boundary %d", boundary)
	}
	if boundary == scalarComparisonCanonical {
		if err := requireFloatComparisonPolicy(mixedRootAdmission); err != nil {
			return nil, err
		}
	} else if chplan.RowShapeOf(inner) == chplan.MixedRowShape {
		if err := requireFloatComparisonPolicy(mixedPlanAdmission); err != nil {
			return nil, err
		}
	}
	// Narrow only after the original operand has resolved union shadowing.
	inner = mixedRowsFloatOnly(inner)
	layout := sampleProjectionLayout{canonical: true, materializeAliases: true}
	if !returnBool {
		valueRef := requireSampleRole(inner.RowType(), chplan.RoleValue)
		inner = &chplan.Filter{Input: inner, Predicate: scalarBinaryValue(valueRef, op, scalar, scalarOnLeft)}
		if boundary == scalarComparisonGuarded {
			return inner, nil
		}
		projected := projectSampleRoles(inner, s, sampleProjectionPolicy{name: preserveSampleName, payload: floatSamplePayload}, layout, func(sampleRoleRefs) sampleRoleRewrite { return sampleRoleRewrite{} })
		// The direct comparison boundary historically materializes the name alias,
		// too; generic preserve-name forwarders intentionally leave it implicit.
		projected.Projections[0].Alias = s.MetricNameColumn
		return projected, nil
	}
	if boundary == scalarComparisonGuarded {
		inner = guardNameDropCollision(inner, arg, s, ctx)
		layout = legacySampleProjectionLayout(inner)
	}
	return projectValueOverInner(inner, s, layout, func(refs sampleRoleRefs) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{scalarBinaryValue(refs.Value, op, scalar, scalarOnLeft)}}
	}), nil
}

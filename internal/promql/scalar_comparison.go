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

// scalarComparisonValueRef keeps the common fully-declared Project path from
// allocating a derived schema only to recover a role its projection already
// declares. Any incomplete declaration falls back to the canonical RowType
// resolver, preserving its inheritance and ambiguity checks.
func scalarComparisonValueRef(inner chplan.Node) *chplan.ColumnRef {
	project, ok := inner.(*chplan.Project)
	if !ok || len(project.Projections) == 0 {
		return requireSampleRole(inner.RowType(), chplan.RoleValue)
	}

	declared := chplan.Schema{Columns: project.Roles}
	var valueName string
	for _, projection := range project.Projections {
		name := chplan.ProjectionOutputName(projection)
		column, found := declared.ByName(name)
		if !found {
			return requireSampleRole(inner.RowType(), chplan.RoleValue)
		}
		if column.Role != chplan.RoleValue {
			continue
		}
		if valueName != "" || name == "" {
			return requireSampleRole(inner.RowType(), chplan.RoleValue)
		}
		valueName = name
	}
	if valueName == "" {
		return requireSampleRole(inner.RowType(), chplan.RoleValue)
	}
	return &chplan.ColumnRef{Name: valueName}
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
	} else if mixedRowsNeedPreparation(inner) {
		if err := requireFloatComparisonPolicy(mixedPlanAdmission); err != nil {
			return nil, err
		}
	}
	// Narrow only after the original operand has resolved union shadowing.
	inner = mixedRowsFloatOnly(inner)
	layout := sampleProjectionLayout{canonical: true, materializeAliases: true}
	if !returnBool {
		valueRef := scalarComparisonValueRef(inner)
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

// See aggregate.go for the no-reflection / no-pointer-aliasing rule
// covering this file.

package traceql

import (
	"fmt"

	"github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerSelect handles `| select(.attr1, .attr2, ...)` projection.
// The attribute list is read via the upstream-fork-exposed
// SelectOperation.Attrs() accessor
// (github.com/tsouza/tempo:cerberus-accessors) — see
// docs/upstream-forks.md.
//
// Output: a chplan.Project that emits the standard span-identity
// columns (TraceId, SpanId, Timestamp) plus the selected attribute
// expressions, so downstream search results can render exactly the
// columns the caller asked for.
func lowerSelect(prev chplan.Node, sel traceql.SelectOperation, s schema.Traces) (chplan.Node, error) {
	attrs := sel.Attrs()
	if len(attrs) == 0 {
		return nil, fmt.Errorf("traceql: `| select(...)` requires at least one attribute")
	}

	// Identity columns first; selected attributes after. Each attribute
	// projection is aliased to its TraceQL name so the row decoder can
	// pick them out.
	projections := []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: s.TraceIDColumn}},
		{Expr: &chplan.ColumnRef{Name: s.SpanIDColumn}},
		{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
	}
	for _, a := range attrs {
		expr, err := lowerAttribute(a, s)
		if err != nil {
			return nil, err
		}
		projections = append(projections, chplan.Projection{
			Expr:  expr,
			Alias: a.Name,
		})
	}
	return &chplan.Project{Input: prev, Projections: projections}, nil
}

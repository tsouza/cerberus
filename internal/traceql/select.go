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
//
// Nested-set intrinsics (nestedSetParent / nestedSetLeft /
// nestedSetRight — what Grafana's Traces Drilldown "Structure" tab
// selects to rebuild the service tree) have no OTel-CH backing column;
// when any of them appears the input is wrapped in a
// chplan.NestedSetAnnotate, which recomputes Tempo's ingest-time
// nested-set numbering from the (TraceId, SpanId, ParentSpanId)
// adjacency at query time, and the projection reads the node's
// synthetic Int64 columns.
func lowerSelect(prev chplan.Node, sel traceql.SelectOperation, s schema.Traces) (chplan.Node, error) {
	attrs := sel.Attrs()
	if len(attrs) == 0 {
		return nil, fmt.Errorf("traceql: `| select(...)` requires at least one attribute")
	}

	input := prev
	if selectsNestedSet(attrs) {
		input = &chplan.NestedSetAnnotate{
			Input:              prev,
			SpansTable:         s.SpansTable,
			TraceIDColumn:      s.TraceIDColumn,
			SpanIDColumn:       s.SpanIDColumn,
			ParentSpanIDColumn: s.ParentSpanIDColumn,
			TimestampColumn:    s.TimestampColumn,
		}
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
		var expr chplan.Expr
		if col, ok := nestedSetColumn(a.Intrinsic); ok {
			expr = &chplan.ColumnRef{Name: col}
		} else {
			var err error
			expr, err = lowerAttribute(a, s)
			if err != nil {
				return nil, err
			}
		}
		projections = append(projections, chplan.Projection{
			Expr:  expr,
			Alias: a.Name,
		})
	}
	return &chplan.Project{Input: input, Projections: projections}, nil
}

// selectsNestedSet reports whether any selected attribute is one of
// the nested-set intrinsics.
func selectsNestedSet(attrs []traceql.Attribute) bool {
	for _, a := range attrs {
		if _, ok := nestedSetColumn(a.Intrinsic); ok {
			return true
		}
	}
	return false
}

// nestedSetColumn maps a nested-set intrinsic onto the synthetic
// column NestedSetAnnotate exposes for it.
func nestedSetColumn(i traceql.Intrinsic) (string, bool) {
	switch i {
	case traceql.IntrinsicNestedSetLeft:
		return chplan.NestedSetLeftColumn, true
	case traceql.IntrinsicNestedSetRight:
		return chplan.NestedSetRightColumn, true
	case traceql.IntrinsicNestedSetParent:
		return chplan.NestedSetParentColumn, true
	}
	return "", false
}

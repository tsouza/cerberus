// See aggregate.go for the no-reflection / no-pointer-aliasing rule
// covering this file.

package traceql

import (
	"fmt"

	traceql "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerSelect handles `| select(.attr1, .attr2, ...)` projection.
// The attribute list is read via the in-house
// SelectOperation.Attrs() accessor (internal/traceql/ast).
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
		input = nestedSetAnnotate(prev, s)
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
	readSelectedAttributesFrom(projections, input.RowType())
	return &chplan.Project{Input: input, Projections: projections}, nil
}

// readSelectedAttributesFrom routes every materialized attribute read in
// projections back to its attribute map unless a select() over rows of type
// row may read the column in the map cell's place (readAttributeOutputFrom).
// projections is rewritten in place.
func readSelectedAttributesFrom(projections []chplan.Projection, row chplan.Schema) {
	for _, projection := range projections {
		readAttributeOutputFrom(projection.Expr, row)
	}
}

// readAttributeOutputFrom routes every materialized attribute read in e (a
// FieldAccess whose MaterializedColumn is set) back to its attribute map
// unless an expression over rows of type row may read the column in the map
// cell's place. e is rewritten in place. It applies to an expression whose
// value is an output, such as a selected value or a by() group key,
// so the column stands in for the cell only when it is value-identical to it
// and the rows carry it:
//
//   - String-kind columns only. A numeric-kind column
//     (FieldAccess.MaterializedColumnNumeric) holds toInt32OrNull of the
//     cell — NULL for a malformed value, and not the cell's own spelling. A
//     group key read from it folds a malformed value into the absent one.
//   - The column must be one of row's columns. An open row type is the
//     span table itself — a Scan, under any Filter, trace-scoped lookup or
//     nested-set annotation — which carries every column. A closed one
//     carries only the columns it names: the structural-join span envelope
//     and a set operation's aligned arms name the attribute maps but no
//     materialized column.
func readAttributeOutputFrom(e chplan.Expr, row chplan.Schema) {
	chplan.InspectExpr(e, func(e chplan.Expr) bool {
		f, ok := e.(*chplan.FieldAccess)
		if !ok || f.MaterializedColumn == "" {
			return true
		}
		if _, carried := row.ByName(f.MaterializedColumn); f.MaterializedColumnNumeric || (!carried && !row.Open) {
			f.MaterializedColumn = ""
			f.MaterializedColumnNumeric = false
		}
		return true
	})
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

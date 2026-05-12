package traceql

import (
	"fmt"
	"reflect"

	"github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerSelect handles `| select(.attr1, .attr2, ...)` projection.
// Tempo's SelectOperation keeps its attribute list on an unexported
// field; we reflect over it to read the slice elements without
// calling .Interface() (which panics on unexported fields).
//
// Output: a chplan.Project that emits the standard span-identity
// columns (TraceId, SpanId, Timestamp) plus the selected attribute
// expressions, so downstream search results can render exactly the
// columns the caller asked for.
func lowerSelect(prev chplan.Node, sel traceql.SelectOperation, s schema.Traces) (chplan.Node, error) {
	attrs, err := readSelectAttrs(sel, s)
	if err != nil {
		return nil, err
	}
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
		projections = append(projections, chplan.Projection{
			Expr:  lowerAttribute(a, s),
			Alias: a.Name,
		})
	}
	return &chplan.Project{Input: prev, Projections: projections}, nil
}

// readSelectAttrs reflects out SelectOperation's unexported `attrs`
// slice. We iterate via reflect.Value.Index() and read each Attribute's
// exported fields (Name, Scope, Parent, Intrinsic) directly — no
// .Interface() call so the unexported-field protection doesn't fire.
func readSelectAttrs(sel traceql.SelectOperation, _ schema.Traces) ([]traceql.Attribute, error) {
	v := reflect.ValueOf(sel)
	if v.Kind() != reflect.Struct {
		return nil, fmt.Errorf("traceql: SelectOperation is not a struct (parser changed?)")
	}
	field := v.FieldByName("attrs")
	if !field.IsValid() {
		return nil, fmt.Errorf("traceql: SelectOperation.attrs not extractable from parser AST")
	}

	out := make([]traceql.Attribute, field.Len())
	for i := 0; i < field.Len(); i++ {
		elem := field.Index(i)
		// AttributeScope and Intrinsic are int8 on Tempo's side; reflect
		// hands us int64 from .Int() — narrowing is safe here (the values
		// are bounded enums) but gosec flags the conversion. The bounds
		// check below makes the narrowing explicit.
		scope := elem.FieldByName("Scope").Int()
		intrinsic := elem.FieldByName("Intrinsic").Int()
		if scope < -128 || scope > 127 || intrinsic < -128 || intrinsic > 127 {
			return nil, fmt.Errorf("traceql: SelectOperation Attribute fields out of int8 range — parser changed?")
		}
		out[i] = traceql.Attribute{
			Name:      elem.FieldByName("Name").String(),
			Scope:     traceql.AttributeScope(int8(scope)),
			Parent:    elem.FieldByName("Parent").Bool(),
			Intrinsic: traceql.Intrinsic(int8(intrinsic)),
		}
	}
	return out, nil
}

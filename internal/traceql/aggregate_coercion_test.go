package traceql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestCoerceMapNumericAggInputLeavesNonAttributeFuncCallAlone(t *testing.T) {
	t.Parallel()

	expr := &chplan.FuncCall{Fn: chplan.FnToFloat64OrNull, Args: []chplan.Expr{
		&chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "SpanAttributes"}, Path: "payload_bytes"},
	}}
	got, nullable := coerceMapNumericAggInput(expr)
	if got != expr {
		t.Fatalf("coerceMapNumericAggInput(non-attribute FuncCall) = %T %p, want original %T %p", got, got, expr, expr)
	}
	if nullable {
		t.Fatal("coerceMapNumericAggInput(non-attribute FuncCall) reported nullable")
	}
}

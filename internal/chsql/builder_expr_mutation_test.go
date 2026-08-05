package chsql

import (
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// errExpr returns a chplan.Expr that Builder.Expr is guaranteed to reject.
// A zero-parameter Lambda is the cheapest such shape: exprLambda rejects it
// up front with ErrUnsupported, so wrapping it lets a test drive the error
// path of any expression renderer that recurses into a child.
func errExpr() chplan.Expr { return &chplan.Lambda{} }

// TestMutation_ExprInSubquery_LeftErrorPropagates pins that a render error on
// the LEFT side of `<left> IN (<subquery>)` reaches the caller. exprInSubquery
// renders the left side inside a Frag closure, which cannot return, so it
// stashes the error in leftErr and returns it after the Frag has run.
//
// Kills builder.go exprInSubquery's CONDITIONALS_NEGATION (`err != nil` ->
// `err == nil`): under the negation the capture only fires on success, where
// err is nil, so leftErr stays nil and a broken left operand renders as
// silently truncated SQL instead of an error.
func TestMutation_ExprInSubquery_LeftErrorPropagates(t *testing.T) {
	t.Parallel()

	err := NewBuilder().Expr(&chplan.InSubquery{
		Left:     errExpr(),
		Subquery: &chplan.Scan{Table: "otel_traces"},
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("InSubquery with an unrenderable Left must return ErrUnsupported, got %v", err)
	}
}

// TestMutation_ExprInSubquery_RendersLeftAndSubquery pins the success shape so
// the error assertion above cannot pass by rejecting every InSubquery.
func TestMutation_ExprInSubquery_RendersLeftAndSubquery(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	if err := b.Expr(&chplan.InSubquery{
		Left:     &chplan.ColumnRef{Name: "TraceId"},
		Subquery: &chplan.Scan{Table: "otel_traces"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, _, _ := b.Build()
	if !strings.Contains(sql, "TraceId") || !strings.Contains(sql, "IN (") || !strings.Contains(sql, "otel_traces") {
		t.Fatalf("expected `<col> IN (<SELECT … otel_traces>)`, got %q", sql)
	}
}

// TestMutation_ExprLambda_MultiParamCommaSeparator pins the parameter-list
// rendering of a multi-parameter lambda: `(k, v) -> <body>`. The separator is
// emitted before every parameter EXCEPT the first.
//
// Kills builder.go exprLambda's `i > 0` guard under both mutators —
// CONDITIONALS_BOUNDARY (`i >= 0`, which prefixes a comma before the first
// parameter: `(, k, v)`) and CONDITIONALS_NEGATION (`i <= 0`, which moves the
// comma to the front and drops the one between: `(, kv)`).
func TestMutation_ExprLambda_MultiParamCommaSeparator(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	if err := b.Expr(&chplan.Lambda{
		Params: []string{"k", "v"},
		Body:   &chplan.ColumnRef{Name: "v"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, _, _ := b.Build()
	if !strings.HasPrefix(sql, "(k, v) -> ") {
		t.Fatalf("multi-parameter lambda must render `(k, v) -> `, got %q", sql)
	}
}

// TestMutation_ExprLambda_SingleParamNoParens pins the sibling shape: a
// one-parameter lambda renders bare, with no parentheses and no separator.
func TestMutation_ExprLambda_SingleParamNoParens(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	if err := b.Expr(&chplan.Lambda{
		Params: []string{"i"},
		Body:   &chplan.ColumnRef{Name: "i"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, _, _ := b.Build()
	if !strings.HasPrefix(sql, "i -> ") {
		t.Fatalf("single-parameter lambda must render `i -> `, got %q", sql)
	}
}

// TestMutation_ExprMapWithoutEmptyValues_InnerErrorPropagates pins that a
// render error on the wrapped map reaches the caller rather than producing a
// truncated `mapFilter((k, v) -> v != ”, ` with no closing argument.
//
// Kills builder.go exprMapWithoutEmptyValues' CONDITIONALS_NEGATION
// (`err != nil` -> `err == nil`), which returns nil on failure and — because
// the renderer has already written the opening call — hands back SQL that
// cannot parse.
func TestMutation_ExprMapWithoutEmptyValues_InnerErrorPropagates(t *testing.T) {
	t.Parallel()

	err := NewBuilder().Expr(&chplan.MapWithoutEmptyValues{Map: errExpr()})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("MapWithoutEmptyValues over an unrenderable Map must return ErrUnsupported, got %v", err)
	}
}

// TestMutation_ExprMapWithoutEmptyValues_RendersFilter pins the success shape
// so the error assertion above cannot pass by rejecting every map.
func TestMutation_ExprMapWithoutEmptyValues_RendersFilter(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	if err := b.Expr(&chplan.MapWithoutEmptyValues{Map: &chplan.ColumnRef{Name: "Attributes"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, _, _ := b.Build()
	if !strings.HasPrefix(sql, "mapFilter((k, v) -> v != '', ") || !strings.HasSuffix(sql, ")") {
		t.Fatalf("expected a closed mapFilter call, got %q", sql)
	}
}

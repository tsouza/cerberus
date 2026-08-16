package chsql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestExprFuncResolvesSealedFn(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	err := b.Expr(&chplan.FuncCall{
		Fn:   chplan.FnArrayMap,
		Args: []chplan.Expr{&chplan.ColumnRef{Name: "x"}},
	})
	if err != nil {
		t.Fatalf("Expr: %v", err)
	}
	if got, want := b.String(), "arrayMap(`x`)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestExprFuncRejectsUndeclaredFn(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	if err := b.Expr(&chplan.FuncCall{Fn: chplan.Fn("not-a-declared-fn")}); err == nil {
		t.Fatal("Expr: got nil error, want an unresolved-Fn error")
	}
}

func TestAggFuncFragResolvesSealedFn(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	aggFuncFrag(chplan.AggFunc{
		Fn:    chplan.FnSum,
		Args:  []chplan.Expr{&chplan.ColumnRef{Name: "Value"}},
		Alias: "Value",
	})(b)
	got, _, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := "sum(`Value`) AS `Value`"; got != want {
		t.Errorf("Build() = %q, want %q", got, want)
	}
}

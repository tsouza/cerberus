package chsql

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestExprFunc_DualMode pins FuncCall's Fn/Name contract (#2060 PR 1,
// point 4): Fn resolves through fnSpellings, Name is still the legacy
// verbatim passthrough, and setting both is a render-time failure rather
// than one silently winning.
func TestExprFunc_DualMode(t *testing.T) {
	t.Parallel()

	t.Run("Fn resolves through fnSpellings", func(t *testing.T) {
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
	})

	t.Run("Name still passes through verbatim (legacy path)", func(t *testing.T) {
		t.Parallel()
		b := NewBuilder()
		err := b.Expr(&chplan.FuncCall{
			Name: "arrayMap",
			Args: []chplan.Expr{&chplan.ColumnRef{Name: "x"}},
		})
		if err != nil {
			t.Fatalf("Expr: %v", err)
		}
		if got, want := b.String(), "arrayMap(`x`)"; got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	})

	t.Run("both Fn and Name set is a render error, not a silent pick", func(t *testing.T) {
		t.Parallel()
		b := NewBuilder()
		err := b.Expr(&chplan.FuncCall{Fn: chplan.FnArrayMap, Name: "arrayMap"})
		if err == nil {
			t.Fatal("Expr: got nil error, want a dual-mode conflict error")
		}
		if !strings.Contains(err.Error(), "both Fn=") || !strings.Contains(err.Error(), "Name=") {
			t.Errorf("Expr err = %q, want it to name both the Fn and Name it rejected", err.Error())
		}
	})

	t.Run("an undeclared Fn value fails render instead of emitting an empty name", func(t *testing.T) {
		t.Parallel()
		b := NewBuilder()
		err := b.Expr(&chplan.FuncCall{Fn: chplan.Fn("not-a-declared-fn")})
		if err == nil {
			t.Fatal("Expr: got nil error, want an unresolved-Fn error")
		}
	})
}

// TestAggFuncFrag_DualMode is TestExprFunc_DualMode's AggFunc analogue.
// aggFuncFrag has no error return (see its doc comment), so the dual-mode
// conflict surfaces through Builder.err — the same first-error-wins path
// Builder.Expr uses for an ordinary render error inside a Frag closure.
func TestAggFuncFrag_DualMode(t *testing.T) {
	t.Parallel()

	t.Run("Fn resolves through fnSpellings", func(t *testing.T) {
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
	})

	t.Run("both Fn and Name set fails the enclosing Build", func(t *testing.T) {
		t.Parallel()
		b := NewBuilder()
		aggFuncFrag(chplan.AggFunc{Fn: chplan.FnSum, Name: "sum", Alias: "Value"})(b)
		_, _, err := b.Build()
		if err == nil {
			t.Fatal("Build: got nil error, want a dual-mode conflict error")
		}
		if !strings.Contains(err.Error(), "both Fn=") || !strings.Contains(err.Error(), "Name=") {
			t.Errorf("Build err = %q, want it to name both the Fn and Name it rejected", err.Error())
		}
	})
}

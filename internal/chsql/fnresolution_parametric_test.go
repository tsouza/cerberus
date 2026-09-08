package chsql

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestArrayReduceParametricFragTakesItsNameFromTheResolutionTable pins the
// parametric-aggregate spec to fnResolutions rather than to a name retyped at
// the emitter. `want` is DERIVED from the table entry, so renaming
// fnResolutions[chplan.FnQuantile] without the emitter following fails here —
// which is the drift this helper exists to make impossible (issue #3186).
func TestArrayReduceParametricFragTakesItsNameFromTheResolutionTable(t *testing.T) {
	t.Parallel()

	name, render, err := resolveFn(chplan.FnQuantile)
	if err != nil {
		t.Fatalf("resolveFn(FnQuantile): %v", err)
	}
	if render != nil {
		t.Fatalf("FnQuantile resolves via a render hook; the parametric spelling has no name to take")
	}

	f, err := arrayReduceParametricFrag(chplan.FnQuantile, []Frag{InlineLit(0.5)}, BareIdent("window_vals"))
	if err != nil {
		t.Fatalf("arrayReduceParametricFrag: %v", err)
	}
	sql, args := Render(f)
	if len(args) != 0 {
		t.Fatalf("parametric spec bound %d positional args, want 0: %v", len(args), args)
	}

	want := "arrayReduce('" + name + "(0.5)', window_vals)"
	if sql != want {
		t.Fatalf("rendered spec\n got: %s\nwant: %s", sql, want)
	}
}

// TestMedianOverArrayFragRendersThroughTheResolvedName is the same pin for
// mad_over_time's median, whose phi is a compiled-in 0.5 rather than a plan
// scalar. It also fixes the exact SQL both mad_over_time medians emit, so a
// silent change to the median spelling is caught here and not only in a
// TXTAR golden.
func TestMedianOverArrayFragRendersThroughTheResolvedName(t *testing.T) {
	t.Parallel()

	name, _, err := resolveFn(chplan.FnQuantile)
	if err != nil {
		t.Fatalf("resolveFn(FnQuantile): %v", err)
	}
	f, err := medianOverArrayFrag(BareIdent("window_vals"))
	if err != nil {
		t.Fatalf("medianOverArrayFrag: %v", err)
	}
	sql, _ := Render(f)
	want := "arrayReduce('" + name + "(0.5)', window_vals)"
	if sql != want {
		t.Fatalf("median frag\n got: %s\nwant: %s", sql, want)
	}
}

// TestArrayReduceParametricFragRejectsABoundParameter pins the fail-closed
// arm: a parameter that renders as a `?` placeholder would bind at the outer
// statement's nesting level while its text sits inside a quoted string, so the
// spec must be refused rather than emitted.
func TestArrayReduceParametricFragRejectsABoundParameter(t *testing.T) {
	t.Parallel()

	_, err := arrayReduceParametricFrag(chplan.FnQuantile, []Frag{Lit(0.5)}, BareIdent("window_vals"))
	if err == nil {
		t.Fatal("a `?`-bound parameter was accepted into a quoted aggregate spec")
	}
	if !strings.Contains(err.Error(), "inline literals") {
		t.Fatalf("rejection does not explain the constraint: %v", err)
	}
}

// TestArrayReduceParametricFragRejectsAnUnresolvedFn pins the other
// fail-closed arm: an Fn with no fnResolutions entry must not render an empty
// or raw aggregate name.
func TestArrayReduceParametricFragRejectsAnUnresolvedFn(t *testing.T) {
	t.Parallel()

	_, err := arrayReduceParametricFrag(chplan.Fn("no_such_fn"), []Frag{InlineLit(0.5)}, BareIdent("v"))
	if err == nil {
		t.Fatal("an unresolved chplan.Fn produced a parametric aggregate spec")
	}
}

package traceql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestFoldTrivialBoolConjunct exercises foldTrivialBoolConjunct directly over
// all eight algebraic arms plus the non-logical-op and nested cases, asserting
// the CONCRETE returned Expr (LitBool.V or operand identity) rather than an
// emitted-SQL substring. The single SQL-substring test in
// metrics_compare_test.go only covered the `x AND true` arm; this kills the
// mutants on the other seven branches (e.g. flipping a `return rhs` to
// `return lhs`, or a `LitBool{V:false}` to `LitBool{V:true}`).
func TestFoldTrivialBoolConjunct(t *testing.T) {
	t.Parallel()

	// x is the meaningful, non-constant operand; identity comparisons below
	// assert the fold returns THIS pointer, not a fresh node.
	x := &chplan.ColumnRef{Name: "ParentSpanId"}
	tru := &chplan.LitBool{V: true}
	fls := &chplan.LitBool{V: false}

	// wantKind selects which assertion an arm expects.
	type wantKind int
	const (
		wantOperand   wantKind = iota // fold returns the x operand (identity)
		wantLitTrue                   // fold returns a constant-true LitBool
		wantLitFalse                  // fold returns a constant-false LitBool
		wantNotFolded                 // ok == false; caller keeps the Binary
	)

	cases := []struct {
		name string
		op   chplan.BinaryOp
		lhs  chplan.Expr
		rhs  chplan.Expr
		want wantKind
	}{
		// AND true → the other operand (both orderings).
		{"and_true_rhs", chplan.OpAnd, x, tru, wantOperand},
		{"and_true_lhs", chplan.OpAnd, tru, x, wantOperand},
		// AND false → constant false (both orderings).
		{"and_false_rhs", chplan.OpAnd, x, fls, wantLitFalse},
		{"and_false_lhs", chplan.OpAnd, fls, x, wantLitFalse},
		// OR false → the other operand (both orderings).
		{"or_false_rhs", chplan.OpOr, x, fls, wantOperand},
		{"or_false_lhs", chplan.OpOr, fls, x, wantOperand},
		// OR true → constant true (both orderings).
		{"or_true_rhs", chplan.OpOr, x, tru, wantLitTrue},
		{"or_true_lhs", chplan.OpOr, tru, x, wantLitTrue},
		// Non-logical op never folds, even with a bare LitBool operand.
		{"non_logical_op", chplan.OpEq, x, tru, wantNotFolded},
		// Logical op with no constant operand never folds.
		{"no_const_operand", chplan.OpAnd, x, &chplan.ColumnRef{Name: "Other"}, wantNotFolded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := foldTrivialBoolConjunct(tc.op, tc.lhs, tc.rhs)
			switch tc.want {
			case wantNotFolded:
				if ok {
					t.Fatalf("foldTrivialBoolConjunct(%s) folded to %#v; want ok=false", tc.op, got)
				}
				return
			case wantOperand:
				if !ok {
					t.Fatalf("foldTrivialBoolConjunct(%s) ok=false; want fold to operand", tc.op)
				}
				if got != chplan.Expr(x) {
					t.Errorf("foldTrivialBoolConjunct(%s) = %#v; want the x operand (identity)", tc.op, got)
				}
			case wantLitTrue, wantLitFalse:
				if !ok {
					t.Fatalf("foldTrivialBoolConjunct(%s) ok=false; want a constant LitBool", tc.op)
				}
				lit, isLit := got.(*chplan.LitBool)
				if !isLit {
					t.Fatalf("foldTrivialBoolConjunct(%s) = %T; want *chplan.LitBool", tc.op, got)
				}
				wantV := tc.want == wantLitTrue
				if lit.V != wantV {
					t.Errorf("foldTrivialBoolConjunct(%s) = LitBool{V:%v}; want V:%v", tc.op, lit.V, wantV)
				}
			}
		})
	}
}

// TestFoldTrivialBoolConjunct_Nested confirms the fold is shape-local: a
// pre-folded inner Binary fed back as one operand of an outer `AND true` folds
// to that inner Binary verbatim — i.e. AND(AND(x,true),true) collapses to x's
// AND node, not a doubly-wrapped one. (The lowering applies the fold per Binary
// as it builds bottom-up, so this mirrors how AND(AND(x,true),true) reduces in
// two passes: the inner to x, then the outer to x.)
func TestFoldTrivialBoolConjunct_Nested(t *testing.T) {
	t.Parallel()

	x := &chplan.ColumnRef{Name: "ParentSpanId"}
	tru := &chplan.LitBool{V: true}

	// Inner fold: AND(x, true) → x.
	inner, ok := foldTrivialBoolConjunct(chplan.OpAnd, x, tru)
	if !ok || inner != chplan.Expr(x) {
		t.Fatalf("inner AND(x,true) = (%#v, %v); want x, true", inner, ok)
	}
	// Outer fold: AND(inner, true) → inner (== x). The `&& true` Drilldown
	// appends collapses fully away, leaving just the meaningful conjunct.
	outer, ok := foldTrivialBoolConjunct(chplan.OpAnd, inner, tru)
	if !ok || outer != chplan.Expr(x) {
		t.Fatalf("outer AND(AND(x,true),true) = (%#v, %v); want x, true", outer, ok)
	}
}

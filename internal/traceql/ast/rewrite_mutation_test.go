package ast

import "testing"

// Mutation-coverage tests for rewrite.go: the meaning-preserving array-fold
// rewrites that collapse homogeneous comparison chains into a single array
// comparison.

// foldedBin parses a span filter and returns its (post-rewrite) top-level
// BinaryOperation.
func foldedBin(t *testing.T, q string) *BinaryOperation {
	t.Helper()
	sf, ok := firstElem(t, q).(*SpansetFilter)
	if !ok {
		t.Fatalf("Parse(%q): element is not *SpansetFilter", q)
	}
	bin, ok := sf.Expression.(*BinaryOperation)
	if !ok {
		t.Fatalf("Parse(%q): expression = %T; want *BinaryOperation", q, sf.Expression)
	}
	return bin
}

// TestArrayFoldRewritesApplied pins each of the four fold rules. If
// applyRewrites is short-circuited (its `if r == nil` guard negated) or
// foldArrayComparison's outer-operator match is broken, the chain stays as a
// raw boolean operation instead of folding.
func TestArrayFoldRewritesApplied(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query   string
		wantOp  Operator
		wantArr StaticType
		wantLen int
	}{
		// `||` of `=` → IN.
		{`{ .x = 1 || .x = 2 }`, OpIn, TypeIntArray, 2},
		// `&&` of `!=` → NOT IN. This rule is the SECOND entry in
		// arrayFoldRules, so reaching it requires the loop to `continue`
		// past the first (OR/Eq) rule — turning that continue into a break
		// drops the fold entirely.
		{`{ .x != 1 && .x != 2 }`, OpNotIn, TypeIntArray, 2},
		// `||` of `=~` → regex match-any.
		{`{ .x =~ "a" || .x =~ "b" }`, OpRegexMatchAny, TypeStringArray, 2},
		// `&&` of `!~` → regex match-none.
		{`{ .x !~ "a" && .x !~ "b" }`, OpRegexMatchNone, TypeStringArray, 2},
		// 3-wide chain folds fully via the post-order walk.
		{`{ .x = 1 || .x = 2 || .x = 3 }`, OpIn, TypeIntArray, 3},
	}
	for _, c := range cases {
		bin := foldedBin(t, c.query)
		if bin.Op != c.wantOp {
			t.Errorf("Parse(%q): Op = %v; want %v", c.query, bin.Op, c.wantOp)
			continue
		}
		arr, ok := bin.RHS.(Static)
		if !ok || arr.Type != c.wantArr {
			t.Errorf("Parse(%q): RHS = %T (%v); want %v array", c.query, bin.RHS, arr.Type, c.wantArr)
			continue
		}
		n := 0
		for range arr.Elements() {
			n++
		}
		if n != c.wantLen {
			t.Errorf("Parse(%q): array len = %d; want %d", c.query, n, c.wantLen)
		}
	}
}

// TestArrayFoldNotAppliedAcrossAttributes pins that the fold only triggers when
// both comparisons reference the SAME attribute; a mismatched attribute leaves
// the boolean operation intact.
func TestArrayFoldNotAppliedAcrossAttributes(t *testing.T) {
	t.Parallel()
	bin := foldedBin(t, `{ .x = 1 || .y = 2 }`)
	if bin.Op != OpOr {
		t.Errorf("Op = %v; want OpOr (no fold across differing attributes)", bin.Op)
	}
}

// TestArrayFoldAttributeOnEitherSide pins that the fold extracts the attribute
// whether it sits on the left or the right of each comparison: `1 = .x || 2 = .x`
// folds the same as `.x = 1 || .x = 2`.
func TestArrayFoldAttributeOnEitherSide(t *testing.T) {
	t.Parallel()
	bin := foldedBin(t, `{ 1 = .x || 2 = .x }`)
	if bin.Op != OpIn {
		t.Errorf("Op = %v; want OpIn (attr on right side still folds)", bin.Op)
	}
	if _, ok := bin.LHS.(Attribute); !ok {
		t.Errorf("folded LHS = %T; want Attribute", bin.LHS)
	}
}

// TestRegexFoldRejectsNonStringOperands pins the regex fold's type restriction:
// `=~` chains only fold when both operands are strings. Numeric operands
// (`.x =~ 1 || .x =~ 2`) must be left as a raw boolean operation — the
// `staticTypeAllowed` membership check is what blocks the fold.
func TestRegexFoldRejectsNonStringOperands(t *testing.T) {
	t.Parallel()
	bin := foldedBin(t, `{ .x =~ 1 || .x =~ 2 }`)
	if bin.Op != OpOr {
		t.Errorf("Op = %v; want OpOr (non-string regex operands must not fold)", bin.Op)
	}
}

// TestArrayFoldNotAppliedAcrossOperators pins that mixing `=` and `!=` under a
// single boolean operator does not fold (no rule matches the operator pair).
func TestArrayFoldNotAppliedAcrossOperators(t *testing.T) {
	t.Parallel()
	bin := foldedBin(t, `{ .x = 1 || .x != 2 }`)
	if bin.Op != OpOr {
		t.Errorf("Op = %v; want OpOr (no fold for mixed =/!=)", bin.Op)
	}
}

package ast

import (
	"errors"
	"strings"
	"testing"
)

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

// TestRegexFoldRejectsNonStringOperands pins that a numeric operand under
// `=~` (`.x =~ 1 || .x =~ 2`) never reaches the array fold at all: the
// operand-type rule validate.go added for #2035 (Operator.binaryTypesValid)
// requires OpRegex's operands to be String (or query-time TypeAttribute),
// and validate() runs over the WHOLE tree before any rewrite — so Parse
// rejects the query outright. The fold's own `restrict` membership check
// on arrayFoldRules' regex entries, which existed to guard exactly this
// case, is now unreachable from Parse and stays only as defence in depth
// for a hand-built tree that skips validate().
func TestRegexFoldRejectsNonStringOperands(t *testing.T) {
	t.Parallel()
	_, err := Parse(`{ .x =~ 1 || .x =~ 2 }`)
	if err == nil {
		t.Fatal("Parse(`{ .x =~ 1 || .x =~ 2 }`) = nil error, want an operand-type rejection")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Parse error = %T (%v), want *ValidationError", err, err)
	}
	if !strings.Contains(verr.Error(), "illegal operation for the given types") {
		t.Errorf("message = %q, want the operand-type wording", verr.Error())
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

// TestStaticTypeAllowedMatchesExactly pins staticTypeAllowed's membership
// test. It is the gate on the two regex fold rules' `restrict` lists, and a
// negated comparison (`t == a` -> `t != a`) turns it inside out: the loop then
// returns true on the FIRST entry that does not match, so a single-entry
// allow-list reports the opposite answer for both a member and a non-member.
func TestStaticTypeAllowedMatchesExactly(t *testing.T) {
	t.Parallel()
	stringFamily := []StaticType{TypeString, TypeStringArray}
	tests := []struct {
		name    string
		t       StaticType
		allowed []StaticType
		want    bool
	}{
		{"first entry matches", TypeString, stringFamily, true},
		{"later entry matches", TypeStringArray, stringFamily, true},
		{"no entry matches", TypeInt, stringFamily, false},
		{"single-entry allow-list, member", TypeString, []StaticType{TypeString}, true},
		{"single-entry allow-list, non-member", TypeInt, []StaticType{TypeString}, false},
		{"empty allow-list admits nothing", TypeString, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := staticTypeAllowed(tc.t, tc.allowed); got != tc.want {
				t.Fatalf("staticTypeAllowed(%v, %v) = %v; want %v", tc.t, tc.allowed, got, tc.want)
			}
		})
	}
}

// NOT KILLABLE — documented, not defended by a test.
//
// rewrite.go:115:4 (INVERT_LOOPCTRL, the `continue` under `if !ok || attrL !=
// attrR`). Reaching that branch means attrLiteralOperands accepted op.LHS
// against the CURRENT rule, so op.LHS's operator is that rule's `scalar` or
// `array`. arrayFoldRules pairs the only two rules that share an outer
// operator as {OpEqual, OpIn} with {OpRegex, OpRegexMatchAny} under `||`, and
// {OpNotEqual, OpNotIn} with {OpNotRegex, OpRegexMatchNone} under `&&` — two
// disjoint operand sets each time. Every later rule therefore fails at the
// EARLIER `continue` (the op.LHS extraction) or at the outer-operator match,
// so a `break` here reaches the same `return nil, false`.
//
// rewrite.go:123:52 (INVERT_LOGICAL, `if !staticTypeAllowed(valL.Type,
// rule.restrict) || !staticTypeAllowed(valR.Type, rule.restrict)` -> `&&`).
// Both rules that carry a `restrict` list carry the same one — the string
// family, {TypeString, TypeStringArray} — and that family is exactly the set
// staticMerge will merge a string with. So the two operands are either both
// allowed (the mutant and the original both fall through to staticMerge and
// fold) or at least one is outside the string family, in which case
// staticMerge's own family check rejects the pair and the mutant reaches the
// `continue` two branches later. There is no static type that is disallowed
// by `restrict` yet mergeable with an allowed one.

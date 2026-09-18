package chsql

import (
	"strings"
	"testing"
)

// TestPrecedenceGuard_DetectsReassociation proves the guard itself: every
// mis-nesting the builder can render is reported, and every correctly
// parenthesised or associative shape is not.
func TestPrecedenceGuard_DetectsReassociation(t *testing.T) {
	x, y, z := Col("x"), Col("y"), Col("z")
	cases := []struct {
		name string
		frag Frag
		want int
	}{
		{"sum inside product, left", Mul(Add(x, y), z), 1},
		{"sum inside product, right", Mul(x, Add(y, z)), 1},
		{"difference on the right of a difference", Sub(x, Sub(y, z)), 1},
		{"sum on the right of a difference", Sub(x, Add(y, z)), 1},
		{"product on the right of a division", Div(x, Mul(y, z)), 1},
		{"modulo on the right of a product", Mul(x, Mod(y, z)), 1},
		{"comparison inside arithmetic", Add(Eq(x, y), z), 1},
		{"disjunction inside a conjunction", And(Or(x, y), z), 1},
		{"conjunction under NOT", Not(And(x, y)), 1},
		{"arithmetic inside a comparison is fine", Gt(Add(x, y), Mul(y, z)), 0},
		{"parenthesised sum inside product", Mul(Paren(Add(x, y)), z), 0},
		{"difference on the left of a difference", Sub(Sub(x, y), z), 0},
		{"sum on the right of a sum", Add(x, Sub(y, z)), 0},
		{"division on the right of a product", Mul(x, Div(y, z)), 0},
		{"conjunction inside a disjunction", Or(And(x, y), z), 0},
		{"comparison under NOT", Not(Eq(x, y)), 0},
		{"a string literal spelling an operator", Eq(x, InlineLit("a - b")), 0},
		{"a lambda arrow", Call("arrayMap", Lambda1("i", Add(BareIdent("i"), x)), y), 0},
		{"a negative literal", Add(x, Neg(y)), 0},
		{"operators inside a subscript", Add(Subscript(x, Sub(y, z)), y), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := InstallPrecedenceGuard()
			b := NewBuilder()
			tc.frag(b)
			got := report()
			if len(got) != tc.want {
				t.Fatalf("rendered %q: %d violation(s), want %d:\n%s", b.String(), len(got), tc.want, strings.Join(got, "\n"))
			}
		})
	}
}

// TestDepthZeroOperatorsIn pins the tokenizer the guard reads operands with:
// an operator inside a string literal (including behind a backslash escape),
// inside a backtick-quoted identifier, or under any parenthesis/bracket depth
// is not a depth-0 operator, and adjacent operators are each found once.
func TestDepthZeroOperatorsIn(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"a + b", []string{" + "}},
		{"a + b - c", []string{" + ", " - "}},
		{"'x + y' + b", []string{" + "}},
		{`'x \' + y' + b`, []string{" + "}},
		{"`a + b` + c", []string{" + "}},
		{"`a\\` + b", []string{" + "}},
		{"'a\\' + b", nil},
		{"(a + b) * c", []string{" * "}},
		{"f(a + b)", nil},
		{"m[a + b] * c", []string{" * "}},
		{"a <= b AND c", []string{" <= ", " AND "}},
		{"a -> b", nil},
		{"a NOT LIKE b", []string{" NOT LIKE "}},
		{"a NOT IN (b)", []string{" NOT IN "}},
		{"a OR b OR c", []string{" OR ", " OR "}},
	}
	for _, tc := range cases {
		got := depthZeroOperatorsIn(tc.text)
		if len(got) != len(tc.want) {
			t.Fatalf("%q: got %q, want %q", tc.text, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%q: got %q, want %q", tc.text, got, tc.want)
			}
		}
	}
}

// TestRightAssociates pins the one relaxation the guard grants: a same-class
// operator on the RIGHT of an associative operator renders correctly, and
// nothing else does.
func TestRightAssociates(t *testing.T) {
	yes := [][2]string{{"+", "+"}, {"+", "-"}, {"*", "*"}, {"*", "/"}, {"AND", "AND"}, {"OR", "OR"}}
	no := [][2]string{{"-", "-"}, {"-", "+"}, {"/", "/"}, {"/", "*"}, {"AND", "OR"}, {"OR", "AND"}, {"+", "*"}, {"*", "+"}, {"=", "="}, {"NOT", "NOT"}}
	for _, p := range yes {
		if !rightAssociates(p[0], p[1]) {
			t.Fatalf("rightAssociates(%q, %q) = false, want true", p[0], p[1])
		}
	}
	for _, p := range no {
		if rightAssociates(p[0], p[1]) {
			t.Fatalf("rightAssociates(%q, %q) = true, want false", p[0], p[1])
		}
	}
}

// TestPrecedenceViolations_EqualPrecedenceOnTheLeftIsFine pins the boundary
// the violation check draws: an operator of EQUAL precedence on the left
// operand renders correctly (`a - b - c` is `(a - b) - c`), one of LOWER
// precedence anywhere does not, and an unknown operator reports nothing.
func TestPrecedenceViolations_EqualPrecedenceOnTheLeftIsFine(t *testing.T) {
	if vs := precedenceViolations("-", []string{"a - b", "c"}); len(vs) != 0 {
		t.Fatalf("equal precedence on the left must not be a violation: %v", vs)
	}
	if vs := precedenceViolations("-", []string{"a", "b - c"}); len(vs) != 1 {
		t.Fatalf("equal precedence on the right of a non-associative operator must be one violation: %v", vs)
	}
	if vs := precedenceViolations("*", []string{"a + b", "c"}); len(vs) != 1 {
		t.Fatalf("lower precedence on the left must be one violation: %v", vs)
	}
	if vs := precedenceViolations("??", []string{"a + b", "c"}); len(vs) != 0 {
		t.Fatalf("an operator the table does not know must report nothing: %v", vs)
	}
	g := &precedenceGuard{}
	g.observe("*", []string{"a + b", "c"})
	g.observe("*", []string{"a", "b"})
	if len(g.violations) != 1 {
		t.Fatalf("observe must record exactly the violating operand once, got %d", len(g.violations))
	}
	if s := g.violations[0].String(); !strings.Contains(s, `operator "*"`) || !strings.Contains(s, "left operand") {
		t.Fatalf("violation text does not name the operator and side: %q", s)
	}
}

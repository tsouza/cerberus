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

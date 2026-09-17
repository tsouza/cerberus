package chsql

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// The operator constructors in builder.go render `<l> <op> <r>` and never
// parenthesise a compound operand ([Paren] is the caller's job). Correct
// precedence is therefore a property of every CALL SITE, not of the API,
// and a Frag handed through a parameter can carry a lower-precedence
// operator into an operand without either the caller or a reader of the
// rendered SQL noticing — `((last - first) + resets) * scale` rendering as
// `last - first + resets * scale` is the shape that shipped once. This
// guard makes that class visible at render time: it observes every
// operator with the exact text of each operand and reports any operand
// carrying, at parenthesis depth 0, an operator that binds looser than
// the one consuming it (or, on the right of a non-associative operator,
// one that binds the same).

// operatorPrecedence orders the infix operators the builder renders, from
// loosest to tightest, the way ClickHouse parses them.
var operatorPrecedence = map[string]int{
	"OR":       1,
	"AND":      2,
	"NOT":      3,
	"=":        4,
	"!=":       4,
	"<":        4,
	"<=":       4,
	">":        4,
	">=":       4,
	"LIKE":     4,
	"NOT LIKE": 4,
	"IN":       4,
	"NOT IN":   4,
	"+":        5,
	"-":        5,
	"*":        6,
	"/":        6,
	"%":        6,
}

// depthZeroOperators are the operator tokens looked for inside an operand's
// text, longest first so `<=` is not read as `<`. Every one of them is
// rendered by the builder with a single space on each side, which is what
// keeps `->` (a lambda arrow), a negative literal and an identifier
// character from matching.
var depthZeroOperators = []string{
	" NOT LIKE ", " NOT IN ", " LIKE ", " IN ", " AND ", " OR ",
	" != ", " <= ", " >= ", " = ", " < ", " > ", " + ", " - ", " * ", " / ", " % ",
}

// precedenceViolation names one operand that would be re-associated.
type precedenceViolation struct {
	op, operand, found string
	right              bool
}

func (v precedenceViolation) String() string {
	side := "left"
	if v.right {
		side = "right"
	}
	return fmt.Sprintf("operator %q: %s operand carries depth-0 %q — %s", v.op, side, strings.TrimSpace(v.found), v.operand)
}

// precedenceViolations reports every operand of op that ClickHouse would
// re-associate: a depth-0 operator looser than op on either side, or one of
// the same precedence on the right of a non-associative op. NOT takes its
// single operand on the right.
func precedenceViolations(op string, operands []string) []precedenceViolation {
	level, ok := operatorPrecedence[op]
	if !ok {
		return nil
	}
	var out []precedenceViolation
	for i, operand := range operands {
		right := i > 0 || op == "NOT"
		for _, found := range depthZeroOperatorsIn(operand) {
			foundLevel := operatorPrecedence[strings.TrimSpace(found)]
			if foundLevel < level || (right && foundLevel == level && !rightAssociates(op, strings.TrimSpace(found))) {
				out = append(out, precedenceViolation{op: op, operand: operand, found: found, right: right})
			}
		}
	}
	return out
}

// rightAssociates reports whether `a op (b inner c)` renders correctly as
// `a op b inner c`: only when op is associative and inner binds at least as
// tightly in the same class (`+` over `+`/`-`, `*` over `*`/`/`, AND over
// AND, OR over OR).
func rightAssociates(op, inner string) bool {
	switch op {
	case "+":
		return inner == "+" || inner == "-"
	case "*":
		return inner == "*" || inner == "/"
	case "AND", "OR":
		return inner == op
	}
	return false
}

// depthZeroOperatorsIn returns every operator token that sits at
// parenthesis/bracket depth 0 of text, outside single-quoted string
// literals and backtick-quoted identifiers.
func depthZeroOperatorsIn(text string) []string {
	var found []string
	depth := 0
	var quote byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		if quote != 0 {
			if c == '\\' && quote == '\'' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '`':
			quote = c
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		default:
			if depth != 0 {
				continue
			}
			for _, tok := range depthZeroOperators {
				if strings.HasPrefix(text[i:], tok) {
					found = append(found, tok)
					i += len(tok) - 2 // leave the trailing space for the next token's leading one
					break
				}
			}
		}
	}
	return found
}

// precedenceGuard collects violations from an installed operandObserver.
type precedenceGuard struct {
	mu         sync.Mutex
	violations []precedenceViolation
}

func (g *precedenceGuard) observe(op string, operands []string) {
	if vs := precedenceViolations(op, operands); len(vs) > 0 {
		g.mu.Lock()
		g.violations = append(g.violations, vs...)
		g.mu.Unlock()
	}
}

// InstallPrecedenceGuardForTest installs the render-time precedence
// observer and returns a function that uninstalls it and reports every
// violation seen since. Exported from a test file so the corpus sweep in
// package chsql_test can drive it; production code never sees it.
func InstallPrecedenceGuardForTest() (report func() []string) {
	g := &precedenceGuard{}
	observe := g.observe
	operandObserver.Store(&observe)
	return func() []string {
		operandObserver.Store(nil)
		g.mu.Lock()
		defer g.mu.Unlock()
		out := make([]string, 0, len(g.violations))
		for _, v := range g.violations {
			out = append(out, v.String())
		}
		return out
	}
}

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
			report := InstallPrecedenceGuardForTest()
			b := NewBuilder()
			tc.frag(b)
			got := report()
			if len(got) != tc.want {
				t.Fatalf("rendered %q: %d violation(s), want %d:\n%s", b.String(), len(got), tc.want, strings.Join(got, "\n"))
			}
		})
	}
}

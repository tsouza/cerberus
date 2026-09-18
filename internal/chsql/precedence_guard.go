package chsql

import (
	"fmt"
	"strings"
	"sync"
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
			if depth == 0 {
				if tok := operatorAt(text[i:]); tok != "" {
					found = append(found, tok)
					i += len(tok) - 2 // leave the trailing space for the next token's leading one
				}
			}
		}
	}
	return found
}

// operatorAt returns the depthZeroOperators token that text begins with, or
// "" when none does. The table is ordered longest first, so the first match
// is the only one: no shorter token is a prefix of the same text.
func operatorAt(text string) string {
	for _, tok := range depthZeroOperators {
		if strings.HasPrefix(text, tok) {
			return tok
		}
	}
	return ""
}

// precedenceGuard collects violations from an installed operandObserver.
type precedenceGuard struct {
	mu         sync.Mutex
	violations []precedenceViolation
}

func (g *precedenceGuard) observe(op string, operands []string) {
	vs := precedenceViolations(op, operands)
	g.mu.Lock()
	g.violations = append(g.violations, vs...)
	g.mu.Unlock()
}

// InstallPrecedenceGuard installs the render-time precedence observer and
// returns a function that uninstalls it and reports every violation seen
// since. It is test support that lives in the package because the observer
// it hooks is unexported: the three heads' TestLower harnesses install it
// around their fixture walks — the one place every plan shape is rendered
// AND its SQL asserted — and package chsql's own emit suite installs it
// around the plans map. Production never calls it; the observer stays nil.
func InstallPrecedenceGuard() (report func() []string) {
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

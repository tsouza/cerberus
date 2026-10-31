package traceql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestEvalRootlessPredicateClosedSet pins evalRootlessPredicate on the
// shapes no query lowering can reach today but the evaluator must still
// refuse: each one is outside the closed set lowerTraceScopedBinary,
// lowerInOperation and lowerRegexMatchArray build, so it must report
// ok=false and keep the plain `TraceId IN (...)` form. Each case is built so
// that evaluating the shape anyway would report a rootless match, which is
// what rootlessTraceMatches would turn into the negated membership form.
func TestEvalRootlessPredicateClosedSet(t *testing.T) {
	t.Parallel()
	val := &chplan.ColumnRef{Name: traceScopedValueAlias}
	eq := func(left chplan.Expr, rhs string) chplan.Expr {
		return &chplan.Binary{Op: chplan.OpEq, Left: left, Right: &chplan.LitString{V: rhs}}
	}
	matches := eq(val, rootlessRootIdentity) // holds for a rootless trace
	misses := eq(val, "GET /home")           // fails for a rootless trace
	perSpan := &chplan.Binary{Op: chplan.OpEq, Left: val, Right: &chplan.ColumnRef{Name: "SpanName"}}

	for _, tc := range []struct {
		name string
		pred chplan.Expr
	}{
		// A one-argument call that is not NOT: negating its argument would
		// turn a failing comparison into a match.
		{"non-NOT call", &chplan.FuncCall{Fn: chplan.FnFromUnixNanos, Args: []chplan.Expr{misses}}},
		// NOT with the wrong arity is not a negation either.
		{"NOT of two args", &chplan.FuncCall{Fn: chplan.FnNot, Args: []chplan.Expr{misses, misses}}},
		// An OR with one decidable matching arm and one undecidable arm.
		{"OR with undecidable right", &chplan.Binary{Op: chplan.OpOr, Left: matches, Right: perSpan}},
		{"OR with undecidable left", &chplan.Binary{Op: chplan.OpOr, Left: perSpan, Right: matches}},
		// A comparison whose left side is a column other than the
		// per-trace value: `SpanName = ""` would evaluate as a match.
		{"other column vs literal", eq(&chplan.ColumnRef{Name: "SpanName"}, rootlessRootIdentity)},
		// A comparison whose left side is not a column at all.
		{"literal vs literal", eq(&chplan.LitString{V: "x"}, rootlessRootIdentity)},
		// A comparison against a per-span operand (#4012).
		{"per-span operand", perSpan},
	} {
		v, ok := evalRootlessPredicate(tc.pred)
		if ok {
			t.Errorf("%s: evalRootlessPredicate = (%v, true), want ok=false (shape outside the closed set)", tc.name, v)
		}
		if rootlessTraceMatches(tc.pred) {
			t.Errorf("%s: rootlessTraceMatches = true, want false (an undecidable shape keeps the plain IN form)", tc.name)
		}
	}
}

// TestEvalRootlessPredicateConnectives pins the AND / OR / NOT truth table
// with one arm the empty root identity satisfies and one it fails.
func TestEvalRootlessPredicateConnectives(t *testing.T) {
	t.Parallel()
	val := &chplan.ColumnRef{Name: traceScopedValueAlias}
	matches := &chplan.Binary{Op: chplan.OpEq, Left: val, Right: &chplan.LitString{V: rootlessRootIdentity}}
	misses := &chplan.Binary{Op: chplan.OpEq, Left: val, Right: &chplan.LitString{V: "GET /home"}}
	not := func(e chplan.Expr) chplan.Expr { return &chplan.FuncCall{Fn: chplan.FnNot, Args: []chplan.Expr{e}} }

	for _, tc := range []struct {
		name string
		pred chplan.Expr
		want bool
	}{
		{"match AND miss", &chplan.Binary{Op: chplan.OpAnd, Left: matches, Right: misses}, false},
		{"miss AND match", &chplan.Binary{Op: chplan.OpAnd, Left: misses, Right: matches}, false},
		{"match AND match", &chplan.Binary{Op: chplan.OpAnd, Left: matches, Right: matches}, true},
		{"match OR miss", &chplan.Binary{Op: chplan.OpOr, Left: matches, Right: misses}, true},
		{"miss OR match", &chplan.Binary{Op: chplan.OpOr, Left: misses, Right: matches}, true},
		{"miss OR miss", &chplan.Binary{Op: chplan.OpOr, Left: misses, Right: misses}, false},
		{"NOT miss", not(misses), true},
		{"NOT match", not(matches), false},
	} {
		v, ok := evalRootlessPredicate(tc.pred)
		if !ok || v != tc.want {
			t.Errorf("%s: evalRootlessPredicate = (%v, %v), want (%v, true)", tc.name, v, ok, tc.want)
		}
	}
}

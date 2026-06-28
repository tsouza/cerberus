package chsql

import (
	"strings"
	"testing"
)

// renderFragToSQL renders a single Frag in isolation and returns its SQL.
func renderFragToSQL(f Frag) string {
	b := NewBuilder()
	f(b)
	sql, _ := b.Build()
	return sql
}

// TestMutation_WriteCTEs_NonRecursiveHead pins the WITH-head selection for a
// chain that holds ONLY a non-recursive (Body-only) CTE: Anchor == nil and
// Recursive == nil, so the recursive-detection condition at builder.go:2073
// (`c.Anchor != nil || c.Recursive != nil`) must evaluate false and the head
// must render bare "WITH " (never "WITH RECURSIVE ").
//
// Kills builder.go:2073:15 (CONDITIONALS_NEGATION `c.Anchor != nil` ->
// `c.Anchor == nil`) and 2073:37 (CONDITIONALS_NEGATION `c.Recursive != nil`
// -> `c.Recursive == nil`): under either negation both operands of the OR flip
// from false to true, so `recursive` becomes true and the head wrongly renders
// "WITH RECURSIVE ".
func TestMutation_WriteCTEs_NonRecursiveHead(t *testing.T) {
	t.Parallel()

	qb := &QueryBuilder{ctes: []cteClause{{Name: "cte0", Body: Col("a")}}}
	b := NewBuilder()
	qb.writeCTEs(b)
	head, _ := b.Build()

	if !strings.HasPrefix(head, "WITH ") {
		t.Fatalf("expected head to start with %q, got %q", "WITH ", head)
	}
	if strings.Contains(head, "RECURSIVE") {
		t.Fatalf("non-recursive (Body-only) CTE chain must render bare WITH, got %q", head)
	}
	if !strings.Contains(head, "cte0 AS") {
		t.Fatalf("expected CTE name + AS in head, got %q", head)
	}
}

// TestMutation_WriteCTEs_RecursiveOrLeftBranch pins the OR semantics of the
// recursive-detection condition with a clause that sets ONLY Anchor (Recursive
// == nil). A Body is also set so writeBody renders without panicking; the
// recursive-detection loop still inspects Anchor/Recursive independently.
//
// With the original `Anchor != nil || Recursive != nil` the left operand is
// true, so `recursive` is true and the head renders "WITH RECURSIVE ".
//
// Kills builder.go:2073:22 (INVERT_LOGICAL `||` -> `&&`): `true && false`
// becomes false, dropping the RECURSIVE keyword. Also kills 2073:15
// (CONDITIONALS_NEGATION `Anchor != nil` -> `Anchor == nil`): `false || false`
// becomes false, likewise dropping RECURSIVE.
func TestMutation_WriteCTEs_RecursiveOrLeftBranch(t *testing.T) {
	t.Parallel()

	qb := &QueryBuilder{ctes: []cteClause{{
		Name:   "cte0",
		Anchor: NewQuery().Select(Col("a")),
		Body:   Col("a"),
	}}}
	b := NewBuilder()
	qb.writeCTEs(b)
	head, _ := b.Build()

	if !strings.HasPrefix(head, "WITH RECURSIVE ") {
		t.Fatalf("clause with a non-nil Anchor must render the RECURSIVE head, got %q", head)
	}
}

// TestMutation_ArrayJoin_CommaSeparator pins the comma-joining of a multi-term
// ARRAY JOIN. With two terms the loop at builder.go:2153 must emit the first
// term with no leading comma (i == 0) and the second with ", " (i > 0),
// producing "ARRAY JOIN <a>, <b>".
//
// Kills builder.go:2154:9 CONDITIONALS_BOUNDARY (`i > 0` -> `i >= 0`: a comma
// is wrongly prefixed before the first term -> "ARRAY JOIN , a, b") and
// CONDITIONALS_NEGATION (`i > 0` -> `i <= 0`: comma before first term and none
// between -> "ARRAY JOIN , ab").
func TestMutation_ArrayJoin_CommaSeparator(t *testing.T) {
	t.Parallel()

	sql, _ := NewQuery().
		Select(Col("x")).
		From(Col("t")).
		ArrayJoin(Col("a"), Col("b")).
		Build()

	want := "ARRAY JOIN " + renderFragToSQL(Col("a")) + ", " + renderFragToSQL(Col("b"))
	if !strings.Contains(sql, want) {
		t.Fatalf("expected SQL to contain %q, got %q", want, sql)
	}
}

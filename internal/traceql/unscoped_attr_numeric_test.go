package traceql

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql/ast"
)

// lowerQuerySQL lowers q and renders it, so these assertions read the SQL a
// deployment would actually send rather than an intermediate the emitter is
// still free to change.
func lowerQuerySQL(t *testing.T, q string) string {
	t.Helper()
	root, err := ast.Parse(q)
	if err != nil {
		t.Fatalf("parse %s: %v", q, err)
	}
	plan, err := Lower(t.Context(), root, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("lower %s: %v", q, err)
	}
	sql, _, err := chsql.Emit(t.Context(), plan)
	if err != nil {
		t.Fatalf("emit %s: %v", q, err)
	}
	return sql
}

// TestUnscopedAttributeIsNotNumericByShape pins the discrimination that a
// regression erased: an UNSCOPED attribute read lowers to a FuncCall (the
// `if(mapContains(span,'k'), span['k'], resource['k'])` span-then-resource
// coalesce), and isNumericExpr answered "numeric" for every FuncCall. That
// made `{ .duration = "slow" }` coerce BOTH sides through toFloat64OrNull, so
// a string compare became NULL = 'slow' and matched nothing where reference
// Tempo matches the span.
//
// The cases below are deliberately paired: the same unscoped attribute must
// NOT coerce against a string and MUST coerce against a number, so neither
// assertion can pass by the emitter simply never coercing (or always doing
// so).
func TestUnscopedAttributeIsNotNumericByShape(t *testing.T) {
	t.Parallel()

	const coercion = "toFloat64OrNull"
	for _, c := range []struct {
		name       string
		query      string
		wantCoerce bool
		why        string
	}{
		{
			name:  "string compare against an attribute named like a numeric intrinsic",
			query: `{ .duration = "slow" }`,
			why:   "`.duration` is an ATTRIBUTE, not the duration intrinsic; comparing it to a string is a string compare",
		},
		{
			name:  "string compare against a plain unscoped attribute",
			query: `{ .status = "degraded" }`,
			why:   "attribute equality is label-matcher semantics, never numeric",
		},
		{
			name:       "ordering compare against a numeric literal",
			query:      `{ .timeout >= 100 }`,
			wantCoerce: true,
			why:        "the numeric literal peer is what carries numeric intent",
		},
		{
			name:       "equality against a numeric literal",
			query:      `{ .retries = 3 }`,
			wantCoerce: true,
			why:        "a numeric literal peer carries numeric intent under equality too",
		},
		{
			name:       "ordering compare between two unscoped attributes",
			query:      `{ .a > .b }`,
			wantCoerce: true,
			why:        "two bare attribute reads under an ORDERING compare carry numeric intent; a raw compare would be lexicographic",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			sql := lowerQuerySQL(t, c.query)
			got := strings.Contains(sql, coercion)
			if got != c.wantCoerce {
				verb := "coerced"
				if c.wantCoerce {
					verb = "did NOT coerce"
				}
				t.Fatalf("%s %s through %s, but %s\n%s", c.query, verb, coercion, c.why, sql)
			}
		})
	}
}

// TestIsNumericExprRejectsTheUnscopedAttributeShape asserts the predicate
// directly, so the classification is pinned even if a future lowering stops
// routing this shape through a comparison.
func TestIsNumericExprRejectsTheUnscopedAttributeShape(t *testing.T) {
	t.Parallel()

	unscoped := &chplan.FuncCall{
		Fn: chplan.FnIf,
		Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnMapContainsKey, Args: []chplan.Expr{
				&chplan.ColumnRef{Name: "SpanAttributes"}, &chplan.LitString{V: "k"},
			}},
			&chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "SpanAttributes"}, Path: "k"},
			&chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "ResourceAttributes"}, Path: "k"},
		},
	}
	if isNumericExpr(unscoped) {
		t.Fatal("the unscoped span-then-resource attribute read is String-valued; classifying it numeric coerces string compares to NULL")
	}
	if !isAttributeRead(unscoped) {
		t.Fatal("the fixture is not the shape isAttributeRead recognises, so the test above proves nothing")
	}

	// A FuncCall that is NOT an attribute read stays numeric, so the fix
	// narrows the classification rather than deleting it.
	other := &chplan.FuncCall{Fn: chplan.FnToFloat64OrNull, Args: []chplan.Expr{
		&chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "SpanAttributes"}, Path: "k"},
	}}
	if !isNumericExpr(other) {
		t.Fatal("a non-attribute-read FuncCall must still classify as numeric")
	}
}

// TestIsAttributeReadRecognisesOnlyTheCoalesceShape pins every conjunct of
// isAttributeRead's FuncCall arm. The shape it recognises is exactly the
// span-then-resource coalesce unscopedAttributeExpr builds — `if()`, three
// arguments, and BOTH value arms themselves attribute reads — and each of
// those conditions must be able to fail on its own, or the predicate extends
// the "bare attribute" numeric intent to calls this lowering never produced.
//
// The function-identity case is what tells the conjunction apart from a
// disjunction over the same operands. `multiIf(cond, then, else)` has the
// coalesce's arity and two attribute-read arms and is still not the coalesce,
// so it answers false only because the `if()` check is AND-ed with the rest.
// Without it, `v.Fn == chplan.FnIf && len(v.Args) == 3` and
// `v.Fn == chplan.FnIf || len(v.Args) == 3` agree on every case here.
func TestIsAttributeReadRecognisesOnlyTheCoalesceShape(t *testing.T) {
	t.Parallel()

	attr := func() chplan.Expr {
		return &chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "SpanAttributes"}, Path: "k"}
	}
	notAttr := func() chplan.Expr { return &chplan.LitString{V: "not an attribute read"} }
	contains := &chplan.FuncCall{Fn: chplan.FnMapContainsKey, Args: []chplan.Expr{
		&chplan.ColumnRef{Name: "SpanAttributes"}, &chplan.LitString{V: "k"},
	}}

	for _, c := range []struct {
		name      string
		fn        chplan.Fn
		then, els chplan.Expr
		want      bool
	}{
		{"both arms are attribute reads", chplan.FnIf, attr(), attr(), true},
		{"only the then-arm is an attribute read", chplan.FnIf, attr(), notAttr(), false},
		{"only the else-arm is an attribute read", chplan.FnIf, notAttr(), attr(), false},
		{"neither arm is an attribute read", chplan.FnIf, notAttr(), notAttr(), false},
		{"a three-argument call that is not if()", chplan.FnMultiIf, attr(), attr(), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := &chplan.FuncCall{Fn: c.fn, Args: []chplan.Expr{contains, c.then, c.els}}
			if got := isAttributeRead(e); got != c.want {
				t.Fatalf("isAttributeRead(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

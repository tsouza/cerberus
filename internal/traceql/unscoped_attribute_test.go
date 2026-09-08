package traceql_test

import (
	"context"
	"strings"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestUnscopedAttribute_ReadsResourceAsWellAsSpan pins the semantics
// reference Tempo gives an unscoped attribute.
//
// `.foo` is not a span attribute. Tempo's categorizeConditions appends a
// scope-none condition to BOTH the span and the resource collectors, and
// its span.AttributeFor resolves the value by NAME with span-first
// precedence, then resource. cerberus read the span map alone, so
// `{ .service.name = "gateway" }` — an idiomatic query, and the form
// Grafana's editor produces for a bare attribute — matched nothing,
// because service.name is a RESOURCE attribute. Silent under-matching,
// not an error.
//
// It is a coalesce and not an OR of two predicates on purpose: with span
// attrs {k: "a"} and resource attrs {k: "b"}, reference answers NO to
// `.k = "b"`, because the span value wins the name lookup.
func TestUnscopedAttribute_ReadsResourceAsWellAsSpan(t *testing.T) {
	t.Parallel()

	sql := lowerSearchSQL(t, `{ .service.name = "gateway" }`)

	if !strings.Contains(sql, "`ResourceAttributes`") {
		t.Errorf(
			"an unscoped attribute never reads ResourceAttributes, so a resource-scoped key "+
				"like service.name can never match:\n%s",
			sql,
		)
	}
	if !strings.Contains(sql, "`SpanAttributes`") {
		t.Errorf("an unscoped attribute must still read SpanAttributes:\n%s", sql)
	}
	if !strings.Contains(sql, "mapContains(`SpanAttributes`") {
		t.Errorf(
			"the span map must be tested for the key so the SPAN value wins when both scopes "+
				"carry it, matching reference's name-lookup precedence:\n%s",
			sql,
		)
	}
}

// TestScopedAttribute_StaysScoped is the other half: making `.foo` read
// both maps must not make `span.foo` or `resource.foo` do the same, or an
// explicitly-scoped query starts matching values from the other scope.
func TestScopedAttribute_StaysScoped(t *testing.T) {
	t.Parallel()

	spanOnly := lowerSearchSQL(t, `{ span.service.name = "gateway" }`)
	if strings.Contains(spanOnly, "`ResourceAttributes`") {
		t.Errorf("span.<attr> must not read ResourceAttributes:\n%s", spanOnly)
	}

	resourceOnly := lowerSearchSQL(t, `{ resource.service.name = "gateway" }`)
	if strings.Contains(resourceOnly, "`SpanAttributes`") {
		t.Errorf("resource.<attr> must not read SpanAttributes:\n%s", resourceOnly)
	}
}

// TestUnscopedAttribute_KeepsNumericCoercion pins that the coalesce did not
// cost the numeric coercion an unscoped attribute needs. OTel-CH stores
// every attribute as a String, so an uncoerced ordering compare is
// lexicographic — `'90' > '400'` is true — and the coercion has to reach
// BOTH arms of the coalesce, not the if() around them.
func TestUnscopedAttribute_KeepsNumericCoercion(t *testing.T) {
	t.Parallel()

	sql := lowerSearchSQL(t, `{ .http.status_code >= 500 }`)

	if strings.Count(sql, "toFloat64OrNull") < 2 {
		t.Errorf(
			"numeric coercion must wrap both arms of the unscoped coalesce, or an unscoped "+
				"attribute compares lexicographically where a scoped one compares numerically:\n%s",
			sql,
		)
	}
}

func lowerSearchSQL(t *testing.T, query string) string {
	t.Helper()

	s := schema.DefaultOTelTraces()
	expr, err := tempo.Parse(query)
	if err != nil {
		t.Fatalf("Parse(%q): %v", query, err)
	}
	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower(%q): %v", query, err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	return sql
}

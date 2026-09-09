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

// TestUnscopedReadBindsLiteralsLikeItsScopedSpelling is the ratchet on the
// one shape that keeps reopening the same class of bug.
//
// `.foo` does not lower to a FieldAccess. It lowers to
// unscopedAttributeExpr's coalesce — `if(mapContains(span,'foo'),
// span['foo'], resource['foo'])` — a FuncCall whose two VALUE arms are the
// FieldAccess reads. Every helper that decides how the OTHER operand must
// be rendered was written against the scoped shape first and silently
// mishandled this one:
//
//   - numeric comparison reverted to a lexicographic string compare until
//     coerceFieldAccess grew an arm for the coalesce;
//   - `{ .attr = "string" }` matched nothing until #3203 stopped
//     isNumericExpr classifying the coalesce as numeric;
//   - a boolean literal stayed a ClickHouse UInt8 against a String column,
//     so `{ .cache.hit = true }` came back 502 (NO_COMMON_TYPE, code 386)
//     where `{ span.cache.hit = true }` answered — until
//     coerceBoolFieldAccess grew one.
//
// All three now route through attributeReadArms, which is the only place
// that knows the shape. This test is what stops a fourth operand kind
// reopening it: it asserts the two spellings bind the SAME literal, for
// every literal kind the grammar has, deriving the expectation from the
// scoped lowering rather than hardcoding it. A helper that learns about a
// new literal but not about the coalesce fails here.
//
// The non-bool rows are the negative half and are load-bearing. Issue #3226
// is exactly ONE cell of the scope x literal-kind matrix — unscoped int,
// float, string, regex and ordered comparison all answered 200 before the
// fix, because they route through coerceFieldAccess / lowerStatic, which
// already knew the coalesce. Keeping them here is what stops the fix
// over-reaching into paths that were already right.
func TestUnscopedReadBindsLiteralsLikeItsScopedSpelling(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	for _, tc := range []struct {
		name   string
		scoped string
		// unscoped is the same query with the `span.` scope dropped.
		unscoped string
	}{
		{"bool_true", `{ span.cache.hit = true }`, `{ .cache.hit = true }`},
		{"bool_false", `{ span.cache.hit = false }`, `{ .cache.hit = false }`},
		{"bool_not_equal", `{ span.cache.hit != true }`, `{ .cache.hit != true }`},
		// The resource-scoped spelling answered too, so the unscoped read
		// must agree with BOTH scoped arms, not just the span one.
		{"bool_true_against_resource_scope", `{ resource.cache.hit = true }`, `{ .cache.hit = true }`},
		{"bool_false_against_resource_scope", `{ resource.cache.hit = false }`, `{ .cache.hit = false }`},
		{"int", `{ span.code = 500 }`, `{ .code = 500 }`},
		{"int_ordering", `{ span.code > 500 }`, `{ .code > 500 }`},
		{"float", `{ span.ratio = 0.5 }`, `{ .ratio = 0.5 }`},
		{"duration", `{ span.dur = 5ms }`, `{ .dur = 5ms }`},
		{"string", `{ span.name2 = "x" }`, `{ .name2 = "x" }`},
		{"string_regex", `{ span.name2 =~ "x.*" }`, `{ .name2 =~ "x.*" }`},
		{"status_enum", `{ span.st = ok }`, `{ .st = ok }`},
		{"kind_enum", `{ span.k = server }`, `{ .k = server }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, scopedArgs := emitTraceQLWithArgs(t, tc.scoped, s)
			_, unscopedArgs := emitTraceQLWithArgs(t, tc.unscoped, s)
			if len(scopedArgs) == 0 || len(unscopedArgs) == 0 {
				t.Fatalf("both spellings must bind at least the compared literal; scoped=%#v unscoped=%#v", scopedArgs, unscopedArgs)
			}
			// The compared literal is the last bound argument in both
			// spellings; everything before it is attribute-key plumbing,
			// which legitimately differs (the coalesce names the key on
			// each arm, and the existence guard names it again).
			scopedLit := scopedArgs[len(scopedArgs)-1]
			unscopedLit := unscopedArgs[len(unscopedArgs)-1]
			if scopedLit != unscopedLit {
				t.Fatalf("unscoped %s bound %#v where scoped %s bound %#v — the coalesce is being rendered as a different type from the map cells it reads",
					tc.unscoped, unscopedLit, tc.scoped, scopedLit)
			}
		})
	}
}

// TestUnscopedBoolLiteralBindsAsAString is the narrow, explicit form of the
// case above for the defect that was live on the wire: the OTel-CH exporter
// stringifies a bool attribute into the Map(String, String) carrier as
// "true" / "false", so the literal must reach ClickHouse as that string. A
// Go bool binds as UInt8 and ClickHouse refuses `String = UInt8` outright
// (code 386, NO_COMMON_TYPE), which is a 502 rather than a wrong answer.
func TestUnscopedBoolLiteralBindsAsAString(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	for _, tc := range []struct {
		query string
		want  string
	}{
		{`{ .cache.hit = true }`, "true"},
		{`{ .cache.hit = false }`, "false"},
		{`{ .cache.hit != true }`, "true"},
		{`{ true = .cache.hit }`, "true"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			_, args := emitTraceQLWithArgs(t, tc.query, s)
			for _, a := range args {
				if _, isBool := a.(bool); isBool {
					t.Fatalf("a bool literal must be bound as the string OTel-CH stores, not a Go bool: args %#v", args)
				}
			}
			// Position is not asserted: the flipped spelling binds the
			// literal FIRST, which is itself the point — it exercises the
			// other operand arm of coerceBoolFieldAccess.
			found := false
			for _, a := range args {
				if a == tc.want {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("bound args %#v do not carry the string %q the map cell holds", args, tc.want)
			}
		})
	}
}

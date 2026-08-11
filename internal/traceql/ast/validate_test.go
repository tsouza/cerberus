package ast

import (
	"errors"
	"strings"
	"testing"
)

// TestParseRejectsTypeMismatch pins the static operand-type rule: a binary
// operation whose two sides have known, incompatible types is rejected by
// Parse itself, so the Tempo head answers it 400 rather than emitting SQL
// ClickHouse aborts at execution time (issue #2033).
func TestParseRejectsTypeMismatch(t *testing.T) {
	cases := []struct {
		query string
		want  string // the expression the message must name
	}{
		// The issue's own repro: `name` is always a String.
		{`{ name > 3 }`, "name > 3"},
		// The mirror image — a duration intrinsic against a string.
		{`{ duration > "abc" }`, `duration > ` + "`abc`"},
		// status / kind are their own types, distinct from the string
		// spelling of their values.
		{`{ kind = "client" }`, "kind = " + "`client`"},
		{`{ status = 3 }`, "status = 3"},
		// The rule reaches inside a boolean tree, and names the offending
		// sub-expression rather than the whole filter.
		{`{ resource.service.name = "api" && name > 3 }`, "name > 3"},
		// …and through a structural operation's nested filters.
		{`{ .a = 1 } >> { name > 3 }`, "name > 3"},
		// …and a scalar filter's aggregate side.
		{`{ } | max(duration) > "slow"`, `max(duration) > ` + "`slow`"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			_, err := Parse(tc.query)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want a type mismatch", tc.query)
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Parse(%q) error = %T (%v), want *ValidationError", tc.query, err, err)
			}
			msg := verr.Error()
			// The wording is the reference backend's own, so a client
			// that reads the message sees one sentence from both.
			if !strings.Contains(msg, "invalid TraceQL query: binary operations must operate on the same type: ") {
				t.Errorf("Parse(%q) message = %q, want the reference wording", tc.query, msg)
			}
			if !strings.Contains(msg, tc.want) {
				t.Errorf("Parse(%q) message = %q, want it to name %q", tc.query, msg, tc.want)
			}
		})
	}
}

// TestParseAcceptsCompatibleOperands is the other half of the ratchet: the
// rule must reject only the provably impossible. Every query here is one
// the reference backend answers, and a rule that over-rejected any of them
// would take a working query off a user's dashboard.
func TestParseAcceptsCompatibleOperands(t *testing.T) {
	queries := []string{
		// A scoped attribute is typed per span at query time, so no
		// static verdict is possible on either side — this is the query
		// the issue reports cerberus 502'ing on.
		`{ duration > span.foo }`,
		`{ duration > resource.service.name }`,
		`{ span.foo > duration }`,
		`{ name > span.foo }`,
		// The numeric family is inter-comparable.
		`{ duration > 100ms }`,
		`{ duration > 100 }`,
		`{ duration > 1.5 }`,
		`{ span:childCount > 2 }`,
		`{ nestedSetParent < 0 }`,
		// nil compares against anything — the existence idiom.
		`{ span.foo != nil }`,
		`{ kind != nil }`,
		// Same types, including the enum-valued intrinsics in their own
		// spelling.
		`{ name = "GET /home" }`,
		`{ kind = client }`,
		`{ status = error }`,
		`{ statusMessage =~ "timeout.*" }`,
		// Boolean composition: an operand of && / || is boolean-typed.
		`{ name = "GET" && duration > 1s }`,
		`{ (duration > 1s || duration < 10ms) && span.foo = 1 }`,
		// The array-fold shapes, which type-check element-wise.
		`{ .x = "a" || .x = "b" }`,
		`{ .x != 1 && .x != 2 }`,
		// Scalar filters and aggregates.
		`{ } | count() > 2`,
		`{ } | avg(duration) > 1s`,
		`{ } | max(duration) + 1 > 2`,
		// Metrics pipelines.
		`{ duration > 1s } | rate() by (resource.service.name)`,
		`{ } | compare({ status = error })`,
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			if _, err := Parse(q); err != nil {
				t.Errorf("Parse(%q) = %v, want it accepted", q, err)
			}
		})
	}
}

// TestIsMatchingOperandSymmetry pins that the rule reads the same in both
// directions. An asymmetric implementation would accept `{ 3 < name }`
// while rejecting `{ name > 3 }` — the same query written the other way
// round — which is the shape a hand-written type table gets wrong.
func TestIsMatchingOperandSymmetry(t *testing.T) {
	all := []StaticType{
		TypeNil, TypeAttribute, TypeInt, TypeFloat, TypeString, TypeBoolean,
		TypeIntArray, TypeFloatArray, TypeStringArray, TypeBooleanArray,
		TypeDuration, TypeStatus, TypeKind,
	}
	for _, a := range all {
		for _, b := range all {
			if a.isMatchingOperand(b) != b.isMatchingOperand(a) {
				t.Errorf("isMatchingOperand asymmetric for %v / %v", a, b)
			}
		}
	}
}

// TestParseRejectsFlippedTypeMismatch is the behavioural half of the
// symmetry property: writing the mismatched comparison the other way round
// must not slip past.
func TestParseRejectsFlippedTypeMismatch(t *testing.T) {
	for _, q := range []string{`{ name > 3 }`, `{ 3 < name }`} {
		if _, err := Parse(q); err == nil {
			t.Errorf("Parse(%q) = nil error, want a type mismatch", q)
		}
	}
}

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
		// sub-expression rather than the whole filter — on either side, so
		// a walk that recursed into one operand only still fails here.
		{`{ resource.service.name = "api" && name > 3 }`, "name > 3"},
		{`{ name > 3 && resource.service.name = "api" }`, "name > 3"},
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
		// Unary operands are walked through to the expression beneath.
		`{ !(kind = server) }`,
		`{ -duration > 1s }`,
		// Scalar filters and aggregates, including a bare aggregate stage
		// with no comparison after it.
		`{ } | count()`,
		`{ } | avg(duration)`,
		`{ } | count() > 2`,
		`{ } | avg(duration) > 1s`,
		`{ } | max(duration) + 1 > 2`,
		`{ } | 1 + max(duration) > 2`,
		// A parenthesised pipeline, both as a spanset operand and as a
		// stage of an outer pipeline.
		`({ .a = 1 } | count() > 1) >> { .b = 2 }`,
		`({ .a = 1 } | count() > 1) | count() > 1`,
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

// TestIsMatchingOperandArrayElement pins each array type against the
// ELEMENT type it stands for. The symmetry property above cannot tell
// these apart — mapping TypeIntArray to the wrong element type is
// symmetric — so the mapping needs naming per case.
func TestIsMatchingOperandArrayElement(t *testing.T) {
	cases := []struct {
		a, b StaticType
		want bool
	}{
		{TypeStringArray, TypeString, true},
		{TypeStringArray, TypeInt, false},
		{TypeStringArray, TypeBoolean, false},
		{TypeIntArray, TypeInt, true},
		// The element rule composes with the numeric-family rule.
		{TypeIntArray, TypeFloat, true},
		{TypeIntArray, TypeDuration, true},
		{TypeIntArray, TypeString, false},
		{TypeFloatArray, TypeFloat, true},
		{TypeFloatArray, TypeString, false},
		{TypeBooleanArray, TypeBoolean, true},
		{TypeBooleanArray, TypeInt, false},
		// Two arrays never meet: neither is the other's element.
		{TypeIntArray, TypeStringArray, false},
		{TypeStringArray, TypeStringArray, true}, // …except by identity.
	}
	for _, tc := range cases {
		t.Run(tc.a.String()+"/"+tc.b.String(), func(t *testing.T) {
			if got := tc.a.isMatchingOperand(tc.b); got != tc.want {
				t.Errorf("%v.isMatchingOperand(%v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			if got := tc.b.isMatchingOperand(tc.a); got != tc.want {
				t.Errorf("%v.isMatchingOperand(%v) = %v, want %v", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

// TestParseRejectsFoldedArrayMismatch pins that the array-fold rewrites
// cannot smuggle an ill-typed chain past the rule. `{ name = 1 || name =
// 2 }` folds to a single `name in [1, 2]` before the check runs, so the
// check has to reach the array's element type to still reject it.
func TestParseRejectsFoldedArrayMismatch(t *testing.T) {
	_, err := Parse(`{ name = 1 || name = 2 }`)
	if err == nil {
		t.Fatal("Parse(`{ name = 1 || name = 2 }`) = nil error, want a type mismatch")
	}
	if !strings.Contains(err.Error(), "name in [1, 2]") {
		t.Errorf("message = %q, want it to name the folded expression", err)
	}
	// The same fold over the matching element type stays accepted.
	if _, err := Parse(`{ name = "a" || name = "b" }`); err != nil {
		t.Errorf("Parse of a well-typed fold = %v, want it accepted", err)
	}
}

// TestValidateReachesEveryPipelinePosition pins that the walk visits each
// position a binary operation can occupy, by putting the SAME ill-typed
// comparison in each one and requiring a rejection every time. A missing
// arm is a silent false negative — the check simply does not run — which
// no accept-path test can detect.
func TestValidateReachesEveryPipelinePosition(t *testing.T) {
	positions := map[string]string{
		"spanset filter":             `{ name > 3 }`,
		"nested spanset operand":     `{ .x = 1 } >> { name > 3 }`,
		"grouping expression":        `{ } | by(name > 3)`,
		"aggregate inner expression": `{ } | max(name > 3) > 1`,
		"scalar filter":              `{ } | max(name) > 1s`,
		"scalar operation operand":   `{ } | 1 + max(name > 3) > 2`,
		"parenthesised pipeline":     `({ name > 3 } | count() > 1) && { .x = 1 }`,
		"pipeline stage":             `({ name > 3 } | count() > 1) | count() > 1`,
		"unary negation":             `{ !(name > 3) }`,
		"metrics compare filter":     `{ } | compare({ name > 3 })`,
	}
	for position, query := range positions {
		t.Run(position, func(t *testing.T) {
			if _, err := Parse(query); err == nil {
				t.Errorf("Parse(%q) = nil error — the walk does not reach the %s", query, position)
			}
		})
	}
}

// TestValidateNilRoot pins the degenerate receiver rather than leaving it
// to a nil dereference: RootExpr is reachable as a nil pointer through the
// parser's own error path.
func TestValidateNilRoot(t *testing.T) {
	var r *RootExpr
	if err := r.validate(); err != nil {
		t.Errorf("(*RootExpr)(nil).validate() = %v, want nil", err)
	}
}

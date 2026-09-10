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

// TestParseRejectsChainBeforeItFolds pins the ordering that lets the rule
// stay complete without an array case: `{ name = 1 || name = 2 }` would
// fold to a single `name in [1, 2]`, but the rule runs first and rejects
// the scalar comparison the client actually wrote — the same sentence the
// reference produces, which validates through ParseNoOptimizations for
// exactly this reason.
func TestParseRejectsChainBeforeItFolds(t *testing.T) {
	_, err := Parse(`{ name = 1 || name = 2 }`)
	if err == nil {
		t.Fatal("Parse(`{ name = 1 || name = 2 }`) = nil error, want a type mismatch")
	}
	if !strings.Contains(err.Error(), "name = 1") {
		t.Errorf("message = %q, want it to name the unfolded comparison", err)
	}
	if strings.Contains(err.Error(), "in [") {
		t.Errorf("message = %q, want the pre-fold spelling, not the folded one", err)
	}
	// The same chain over the matching type stays accepted, and still
	// folds — the rule must not cost the rewrite.
	expr, err := Parse(`{ name = "a" || name = "b" }`)
	if err != nil {
		t.Fatalf("Parse of a well-typed chain = %v, want it accepted", err)
	}
	if got := expr.String(); !strings.Contains(got, "in [") {
		t.Errorf("parsed to %q, want the array fold still applied", got)
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

// assertValidationError fails t unless err is a *ValidationError whose
// message contains wantSubstr — the shared assertion for every #2035 rule
// test below, mirroring TestParseRejectsTypeMismatch's own pattern.
func assertValidationError(t *testing.T, query string, err error, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Parse(%q) = nil error, want a validation error", query)
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Parse(%q) error = %T (%v), want *ValidationError", query, err, err)
	}
	if !strings.Contains(verr.Error(), wantSubstr) {
		t.Errorf("Parse(%q) message = %q, want it to contain %q", query, verr.Error(), wantSubstr)
	}
}

// TestParseRejectsAggregateNonNumeric pins the reference's aggregate
// result-type rule (issue #2035, `Aggregate.validate`): `max` / `min` /
// `sum` / `avg` must resolve to a number, and `name` is always a String.
func TestParseRejectsAggregateNonNumeric(t *testing.T) {
	cases := []string{
		`{ } | max(name) > "a"`,
		`{ } | avg(name)`,
		`{ } | min(name) > "a"`,
		`{ } | sum(name) > "a"`,
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			_, err := Parse(q)
			assertValidationError(t, q, err, "aggregate field expressions must resolve to a number type")
		})
	}
}

// TestParseRejectsIllegalBinaryOperatorTypes pins the reference's
// operator/operand-validity rule (issue #2035,
// `Operator.binaryTypesValid`): a mismatched-yet-attribute-compatible pair
// can still fail because the OPERATOR itself never accepts one of the
// operand's types — `.foo && 3`: `&&` only accepts boolean operands, and
// `TypeInt` is not one, even though `TypeAttribute` on the other side would
// have matched isMatchingOperand for anything.
func TestParseRejectsIllegalBinaryOperatorTypes(t *testing.T) {
	cases := []string{
		`{ .foo && 3 }`,
		`{ 3 && .foo }`,
		`{ .foo && duration }`,
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			_, err := Parse(q)
			assertValidationError(t, q, err, "illegal operation for the given types")
		})
	}
}

// TestParseRejectsNonBooleanSpanFilter pins the reference's span-filter
// result-type rule (issue #2035, `SpansetFilter.validate`): the filter
// expression must resolve to a boolean — `{ duration }` and `{ 3 }` both
// name a value, not a predicate.
func TestParseRejectsNonBooleanSpanFilter(t *testing.T) {
	cases := []string{
		`{ duration }`,
		`{ 3 }`,
		`{ name }`,
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			_, err := Parse(q)
			assertValidationError(t, q, err, "span filter field expressions must resolve to a boolean")
		})
	}
}

// TestParseRejectsNonBooleanCompareFilter pins that compare()'s filter is
// checked by the same span-filter result-type rule as an ordinary `{ ... }`
// stage — reference's MetricsCompare.validate() calls the same
// SpansetFilter.validate() a plain filter does.
func TestParseRejectsNonBooleanCompareFilter(t *testing.T) {
	q := `{ } | compare({ duration })`
	_, err := Parse(q)
	assertValidationError(t, q, err, "span filter field expressions must resolve to a boolean")
}

// TestParseRejectsIllegalUnaryOperand pins the reference's unary
// operand-type rule (issue #2035, `Operator.unaryTypesValid`): unary `-`
// requires a numeric operand, and `name` is always a String.
func TestParseRejectsIllegalUnaryOperand(t *testing.T) {
	cases := []string{
		`{ -name = "a" }`,
		`{ -status = 1 }`,
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			_, err := Parse(q)
			assertValidationError(t, q, err, "illegal operation for the given type")
		})
	}
}

// TestParseRejectsInvalidRegex pins the reference's compile-time regex
// rule (issue #2035, the regex branch of `BinaryOperation.validate`):
// `=~` / `!~` compile their pattern at parse time, not execution time.
func TestParseRejectsInvalidRegex(t *testing.T) {
	cases := []string{
		`{ span.x =~ "[" }`,
		`{ span.x !~ "(" }`,
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			_, err := Parse(q)
			assertValidationError(t, q, err, "invalid regex")
		})
	}
}

// TestParseAcceptsNewRuleEdgeCases is the acceptance-side ratchet for the
// five rules #2035 adds: each query here is either a cerberus extension the
// reference has no equivalent for (the bare `parent` intrinsic composed
// under `&&`) or an ordinary query the new rules must not over-reject.
func TestParseAcceptsNewRuleEdgeCases(t *testing.T) {
	queries := []string{
		// Bare `parent` is a cerberus-only structural marker (not adopted
		// from the reference at all — see the package doc comment) that
		// must keep composing under `&&` despite its TypeNil implied type.
		`{ parent && resource.service.name = "api" }`,
		`{ parent = "fedcba9876543210" }`,
		// Aggregates over a dynamic (query-time-typed) attribute, or a
		// genuinely numeric intrinsic, stay accepted.
		`{ } | max(span.foo)`,
		`{ } | avg(duration)`,
		`{ } | sum(span:childCount)`,
		// `&&` / `||` composing two boolean sub-expressions, including
		// dynamic attribute operands on either side.
		`{ name = "GET" && duration > 1s }`,
		`{ .a = 1 && .b = 2 }`,
		// Unary `-` over a numeric intrinsic or a dynamic attribute; unary
		// `!` over a boolean sub-expression.
		`{ -duration > 1s }`,
		`{ -span.foo > 1s }`,
		`{ !(kind = server) }`,
		// A span filter resolving through a dynamic attribute stays
		// accepted — its real type is unknown until query time.
		`{ span.foo }`,
		// A valid regex pattern still compiles and is accepted.
		`{ span.x =~ "GET.*" }`,
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			if _, err := Parse(q); err != nil {
				t.Errorf("Parse(%q) = %v, want it accepted", q, err)
			}
		})
	}
}

// TestUnaryTypesValidSymmetryWithAttribute pins that TypeAttribute is
// always a valid unary operand regardless of operator, and that the two
// existence operators (OpExists / OpNotExists) accept every type —
// mirroring the reference's own unaryTypesValid, which never rejects on
// those two axes.
func TestUnaryTypesValidSymmetryWithAttribute(t *testing.T) {
	all := []StaticType{
		TypeNil, TypeAttribute, TypeInt, TypeFloat, TypeString, TypeBoolean,
		TypeDuration, TypeStatus, TypeKind,
	}
	for _, ty := range all {
		if !OpExists.unaryTypesValid(ty) {
			t.Errorf("OpExists.unaryTypesValid(%v) = false, want true", ty)
		}
		if !OpNotExists.unaryTypesValid(ty) {
			t.Errorf("OpNotExists.unaryTypesValid(%v) = false, want true", ty)
		}
	}
	if !OpSub.unaryTypesValid(TypeAttribute) {
		t.Error("OpSub.unaryTypesValid(TypeAttribute) = false, want true")
	}
	if !OpNot.unaryTypesValid(TypeAttribute) {
		t.Error("OpNot.unaryTypesValid(TypeAttribute) = false, want true")
	}
}

// TestValidateRegexPatternSkipsNonStringLiterals pins the type half of
// validateRegexPattern's guard. The compile-time regex check is defined only
// for a literal String pattern: every other static encodes to text that was
// never written as a pattern, so feeding it to regexp.Compile would reject
// queries on the strength of an encoding artefact.
//
// The array-valued node below is built directly rather than parsed, because
// the grammar never puts an array literal on a scalar regex operator — which
// is the point. The guard is what keeps such a node (from a future rewrite, a
// lenient-parse path, or an in-package caller) out of regexp.Compile, and an
// empty array encodes to "[]", which is not a valid regular expression. A
// mutation that drops the type test lets it through and turns this
// valid-by-construction node into a validation error.
//
// The pattern being a Static AT ALL is a separate rule with a separate
// answer — the reference rejects `{ span.a =~ span.b }` outright rather than
// skipping it (see validateRegexPattern and
// TestParseRejectsWhatReferenceRejects/regex_against_attribute) — so this
// test deliberately passes a Static of the wrong TYPE, not a non-Static.
func TestValidateRegexPatternSkipsNonStringLiterals(t *testing.T) {
	t.Parallel()
	attr := NewScopedAttribute(AttributeScopeSpan, false, "http.route")
	for _, op := range []Operator{OpRegex, OpNotRegex} {
		bin := &BinaryOperation{Op: op, LHS: attr, RHS: NewStaticStringArray(nil)}
		if err := validateRegexPattern(bin); err != nil {
			t.Errorf("validateRegexPattern(%s) = %v; want nil (a non-String static is not a pattern)", bin.String(), err)
		}
	}
	// The String half of the guard still reaches the compiler.
	bad := &BinaryOperation{Op: OpRegex, LHS: attr, RHS: NewStaticString("[")}
	if err := validateRegexPattern(bad); err == nil {
		t.Error("validateRegexPattern({ span.http.route =~ `[` }) = nil; want an invalid-regex rejection")
	}
}

// TestParseRejectsWhatReferenceRejects covers five shapes reference Tempo
// declines with a 400 and cerberus, before these rules, accepted — three of
// them answering 200 with a meaningless result and two reaching ClickHouse
// and coming back as a 502 carrying a storage-engine error.
//
// Neither parity ledger could see them: test/rejection-parity enumerates
// cerberus's OWN rejection sites (it cannot know about a site cerberus does
// not have), and test/surface-parity's TraceQL oracle is Parse+Validate
// only, so a query both engines PARSE looks identical there regardless of
// what the reference's validate() then says about it.
func TestParseRejectsWhatReferenceRejects(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{
			// pkg/traceql/ast_validate.go's `Aggregate.validate` —
			// `!a.e.referencesSpan()`. Aggregating a literal says nothing
			// about the trace; cerberus emitted `avg(1)` and answered 200.
			name:  "aggregate_over_constant",
			query: `{} | avg(1) > 2`,
			want:  "aggregate field expressions must reference the span: avg(1)",
		},
		{
			// pkg/traceql/ast_validate.go's `GroupOperation.validate` —
			// `!o.Expression.referencesSpan()`. cerberus projected the
			// literal as the grouping key and answered 200 with one
			// synthetic group covering every span.
			name:  "group_by_constant",
			query: `{} | by(1) | count() > 0`,
			want:  "grouping field expressions must reference the span: by(1)",
		},
		{
			// pkg/traceql/ast_validate.go's `BinaryOperation.validate` — the
			// regex RHS must be a Static. cerberus lowered it to ClickHouse's match() with a
			// per-row pattern, which ClickHouse refuses at execution time:
			// a 502 where the reference gives 400.
			name:  "regex_against_attribute",
			query: `{ span.a =~ span.b }`,
			want:  "invalid type for =~ or !~: span.b",
		},
		{
			// pkg/traceql/ast_metrics.go's `MetricsAggregate.validate` — phi
			// must be in [0,1].
			// cerberus emitted quantileExactInclusive(1.5), which
			// ClickHouse rejects: another 502-for-400.
			name:  "quantile_out_of_range",
			query: `{} | quantile_over_time(duration, 1.5)`,
			want:  "quantile must be between 0 and 1: 1.5",
		},
		{
			// The trailing `len(a.by) > maxGroupBys` check of
			// pkg/traceql/ast_metrics.go's `MetricsAggregate.validate`,
			// against engine_metrics.go's `maxGroupBys = 5`.
			name:  "too_many_group_bys",
			query: `{} | rate() by (span.a, span.b, span.c, span.d, span.e, span.f)`,
			want:  "metrics group by 6 values not yet supported",
		},
		{
			// quantile_over_time reserves one of the five slots for the
			// synthetic __bucket label, so it stops a key earlier
			// (the per-op `len(a.by) >= maxGroupBys` check in
			// ast_metrics.go's `MetricsAggregate.validate`).
			name:  "quantile_group_by_ceiling_is_one_lower",
			query: `{} | quantile_over_time(duration, 0.9) by (span.a, span.b, span.c, span.d, span.e)`,
			want:  "metrics group by 5 values not yet supported",
		},
		{
			// avg_over_time carries a companion count series and applies
			// the same stricter ceiling
			// (engine_metrics_average.go's `averageOverTimeAggregator.validate`).
			name:  "avg_over_time_group_by_ceiling_is_one_lower",
			query: `{} | avg_over_time(duration) by (span.a, span.b, span.c, span.d, span.e)`,
			want:  "metrics group by 5 values not yet supported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.query)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want a rejection", tc.query)
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Parse(%q) error = %T (%v), want *ValidationError", tc.query, err, err)
			}
			if got := verr.Error(); !strings.Contains(got, tc.want) {
				t.Fatalf("Parse(%q) message = %q, want it to contain %q", tc.query, got, tc.want)
			}
		})
	}
}

// TestParseAcceptsWhatReferenceAccepts is the ratchet on the rules above: a
// rule that rejected more than the reference does would be its own
// wrong-rejection divergence. Each case sits one step inside a boundary the
// test above sits one step outside of.
func TestParseAcceptsWhatReferenceAccepts(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		// An aggregate/grouping over anything that names the span is fine,
		// including an arithmetic expression that also carries a literal.
		{"aggregate_over_attribute", `{} | avg(span.size) > 2`},
		{"aggregate_over_intrinsic", `{} | avg(duration) > 2ms`},
		{"aggregate_over_attribute_arithmetic", `{} | avg(span.size * 2) > 2`},
		{"group_by_attribute", `{} | by(span.a) | count() > 0`},
		{"group_by_intrinsic", `{} | by(name) | count() > 0`},
		// count() has no inner expression at all and is exempt from the
		// referencesSpan rule in both engines.
		{"count_takes_no_operand", `{} | count() > 0`},
		// A literal regex pattern is what the rule demands, and a valid
		// one still compiles.
		{"regex_against_literal", `{ span.a =~ "b.*" }`},
		{"not_regex_against_literal", `{ span.a !~ "b.*" }`},
		// Both ends of the phi range are inclusive upstream.
		{"quantile_zero", `{} | quantile_over_time(duration, 0)`},
		{"quantile_one", `{} | quantile_over_time(duration, 1)`},
		// Five keys is the ceiling for the plain reducers…
		{"rate_five_group_bys", `{} | rate() by (span.a, span.b, span.c, span.d, span.e)`},
		{"count_over_time_five_group_bys", `{} | count_over_time() by (span.a, span.b, span.c, span.d, span.e)`},
		{"max_over_time_five_group_bys", `{} | max_over_time(duration) by (span.a, span.b, span.c, span.d, span.e)`},
		// …and four for the two that reserve a slot.
		{"quantile_four_group_bys", `{} | quantile_over_time(duration, 0.9) by (span.a, span.b, span.c, span.d)`},
		{"avg_over_time_four_group_bys", `{} | avg_over_time(duration) by (span.a, span.b, span.c, span.d)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.query); err != nil {
				t.Fatalf("Parse(%q) = %v, want it accepted", tc.query, err)
			}
		})
	}
}

// TestParseRejectsNilComparisonOnIntrinsic pins the reference's
// `<intrinsic> = nil` rejection (pkg/traceql/ast_validate.go's
// `UnaryOperation.validate`, mirrored at fetch time by vparquet4's
// `checkConditions`). Before issue #3260 cerberus enforced this at
// LOWERING instead, which is a different error class: the Tempo head
// answers a lowering error 422 (errclass.go's ErrClassLower) where the
// reference answers 400, so a query the reference calls malformed came
// back to Grafana as "valid TraceQL cerberus cannot serve".
func TestParseRejectsNilComparisonOnIntrinsic(t *testing.T) {
	cases := []struct {
		query string
		want  string // the attribute the message must name
	}{
		// The three forms issue #3260 reports, in the reference's own
		// invalid-query corpus (pkg/traceql/test_examples.yaml).
		{`{ span:status = nil }`, "status"},
		{`{ name = nil }`, "name"},
		// Written the other way round: the grammar folds `nil = x` to the
		// same OpNotExists node, and the reference lists both spellings.
		{`{ nil = span:status }`, "status"},
		// Every intrinsic, not an enumerated subset — the reference's
		// clause is `attr.Intrinsic != IntrinsicNone`.
		{`{ kind = nil }`, "kind"},
		{`{ duration = nil }`, "duration"},
		{`{ span:childCount = nil }`, "span:childCount"},
		{`{ nestedSetLeft = nil }`, "nestedSetLeft"},
		{`{ trace:id = nil }`, "trace:id"},
		// The rule reaches inside a boolean tree and through a pipeline,
		// so a walk that only inspected the top-level filter still fails.
		{`{ span.foo = "a" && name = nil }`, "name"},
		{`{ name = nil } | rate()`, "name"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			_, err := Parse(tc.query)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want an intrinsic-nil rejection", tc.query)
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Parse(%q) error = %T (%v), want *ValidationError", tc.query, err, err)
			}
			msg := verr.Error()
			want := "invalid TraceQL query: " + tc.want + "=nil is not valid because intrinsics cannot be nil"
			if msg != want {
				t.Errorf("Parse(%q) message = %q, want %q", tc.query, msg, want)
			}
		})
	}
}

// TestParseRejectsNilComparisonOnResourceServiceName pins the second
// clause of the same reference rule: resource.service.name is mandatory
// on every OTLP resource, so it can never be absent.
func TestParseRejectsNilComparisonOnResourceServiceName(t *testing.T) {
	for _, q := range []string{
		`{ resource.service.name = nil }`,
		`{ nil = resource.service.name }`,
		// The quoted spelling names the same attribute.
		`{ resource."service.name" = nil }`,
	} {
		t.Run(q, func(t *testing.T) {
			_, err := Parse(q)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want a resource.service.name rejection", q)
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Parse(%q) error = %T (%v), want *ValidationError", q, err, err)
			}
			want := "invalid TraceQL query: resource.service.name=nil is not valid because resource.service.name cannot be nil"
			if got := verr.Error(); got != want {
				t.Errorf("Parse(%q) message = %q, want %q", q, got, want)
			}
		})
	}
}

// TestParseAcceptsLegalNilComparisons is the other half of the ratchet.
// The reference's rule covers `= nil` (OpNotExists) on an intrinsic or on
// resource.service.name and NOTHING else; every query here is one the
// reference answers, and over-rejecting any of them would take a working
// panel off a dashboard — including the `<groupBy> != nil` conjunct
// Grafana Traces Drilldown stamps on every breakdown query.
func TestParseAcceptsLegalNilComparisons(t *testing.T) {
	queries := []string{
		// A user attribute may legitimately be absent, in every scope.
		`{ span.foo = nil }`,
		`{ .foo = nil }`,
		`{ resource.foo = nil }`,
		`{ event.exception.type = nil }`,
		`{ link.foo = nil }`,
		`{ instrumentation.foo = nil }`,
		// service.name is special ONLY in the resource scope: the same
		// name under another scope (or none) is an ordinary attribute.
		`{ span.service.name = nil }`,
		`{ .service.name = nil }`,
		// `!= nil` (OpExists) is untouched by the rule on BOTH clauses —
		// the reference's guard is `o.Op == OpNotExists` alone.
		`{ name != nil }`,
		`{ kind != nil }`,
		`{ status != nil }`,
		`{ span:childCount != nil }`,
		`{ resource.service.name != nil }`,
		`{ nil != name }`,
		`{ nil != resource.service.name }`,
		// A compound operand is not an Attribute, so neither clause
		// applies — the reference folds it to a constant instead.
		`{ (span.a + 1) = nil }`,
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			if _, err := Parse(q); err != nil {
				t.Errorf("Parse(%q) = %v, want it accepted", q, err)
			}
		})
	}
}

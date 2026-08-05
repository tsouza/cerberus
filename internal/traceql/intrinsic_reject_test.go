package traceql_test

import (
	"context"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/test/spec"
)

// TestUnbackedIntrinsicComparisonsLowerConstant pins the parity contract
// for comparisons against intrinsics the OTel-CH span-row schema cannot
// answer at all — no per-span column AND no per-trace aggregate lowering
// (per-span childCount, per-event timeSinceStart, instrumentation-scoped
// attributes). Reference Tempo's `/api/search` ACCEPTS each of these —
// the absent operand resolves to StaticNil and the comparison evaluates
// StaticFalse, so the query returns a 2xx empty result, not a 4xx. The
// rejection-parity layer flagged the old loud-422 behaviour as a
// wrong_rejection. Cerberus now lowers them to a constant-false
// predicate, matching reference's status class (2xx) and its
// matches-nothing semantics. The earlier worry — a silent
// `SpanAttributes['rootName']` map lookup returning a confidently-wrong
// result — does not apply: a constant-false predicate is the *correct*
// answer, not a guess.
//
// The trace-scoped root-identity intrinsics (rootName / rootServiceName
// / traceDuration, bare or trace:-scoped spelling) used to be classified
// here too; they now have a real correlated-subquery lowering instead
// — see TestTraceScopedIntrinsicsLowerRealSubquery and issue #1711.
func TestUnbackedIntrinsicComparisonsLowerConstant(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	for _, q := range []string{
		`{ span:childCount > 0 }`,
		`{ event:timeSinceStart > 1ms }`,
		`{ instrumentation.deployment = "blue" }`,
		`{ instrumentation.deployment != "blue" }`,
	} {
		expr, err := tempo.Parse(q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Errorf("Lower(%q): want successful constant lowering, got error: %v", q, err)
		}
	}
}

// TestTraceScopedIntrinsicsLowerRealSubquery pins the supported half of
// the trace-scoped root-identity surface split out of
// TestUnbackedIntrinsicComparisonsLowerConstant by issue #1711: a
// comparison against rootName / rootServiceName / traceDuration (bare or
// trace:-scoped spelling) now lowers to a real `TraceId IN (<per-trace
// aggregate>)` predicate — never the constant-false fold — regardless of
// which side of the comparison carries the intrinsic, or whether the
// comparison is an equality, an ordering, or a regex match. The exact
// SQL shape is pinned by the test/spec/traceql/trace_scoped_*.txtar
// fixtures; this test pins the flipped-operand and regex forms those
// fixtures don't cover.
func TestTraceScopedIntrinsicsLowerRealSubquery(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	for _, q := range []string{
		`{ rootName = "GET /home" }`,
		`{ "GET /home" = rootName }`,
		`{ rootServiceName =~ "front.*" }`,
		`{ traceDuration > 100ms }`,
		`{ 100ms < traceDuration }`,
		`{ trace:rootName = "GET /home" }`,
		`{ trace:rootService = "frontend" }`,
		`{ trace:duration > 100ms }`,
	} {
		expr, err := tempo.Parse(q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", q, err)
		}
		filter, ok := plan.(*chplan.Filter)
		if !ok {
			t.Fatalf("Lower(%q): want *chplan.Filter, got %T", q, plan)
		}
		if _, isConst := filter.Predicate.(*chplan.LitBool); isConst {
			t.Errorf("Lower(%q): predicate folded to a constant LitBool; want a real TraceId IN (subquery) predicate", q)
		}
		if _, ok := filter.Predicate.(*chplan.InSubquery); !ok {
			t.Errorf("Lower(%q): predicate = %T, want *chplan.InSubquery", q, filter.Predicate)
		}
	}
}

// TestNestedIntrinsicsLowerInComparisons pins the supported half of the
// nested-intrinsic surface: event:name / link:traceID / link:spanID in
// comparisons lower to NestedArrayExists against the Events / Links
// Nested subfields (the SQL shape is pinned by the
// test/spec/traceql/*_intrinsic.txtar fixtures; this test pins the
// flipped-operand form those fixtures don't cover).
func TestNestedIntrinsicsLowerInComparisons(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	for _, q := range []string{
		`{ event:name = "exception" }`,
		`{ event:name =~ "exc.*" }`,
		`{ "exception" = event:name }`,
		`{ link:traceID = "a0000000000000000000000000000001" }`,
		`{ link:spanID != "0000000000000001" }`,
	} {
		expr, err := tempo.Parse(q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
			t.Errorf("Lower(%q): unexpected error: %v", q, err)
		}
	}
}

// TestUnaryNotReferenceBugParity pins the reference-Tempo-bug-compatible
// lowering of the `!(<expr>)` spanset filter shape (issues #1711/#1712):
// a single (odd) level of TOP-LEVEL unary NOT lowers to constant-false,
// while a doubly-negated (even) expression cancels and lowers exactly
// like its un-negated positive form. As an AND/OR operand, a NOT
// wrapping a single comparison is dropped as that combinator's identity
// element instead; a NOT wrapping a compound AND/OR falls back to the
// logically-correct `not(...)` translation. All verified against real
// reference Tempo — see lowerUnaryNot's doc comment. The
// test/spec/traceql/unary_not_*.txtar fixtures pin the chDB-verified
// end-to-end SQL/rows for the base cases; this test pins the deeper
// nesting and operand-position cases those fixtures don't cover.
func TestUnaryNotReferenceBugParity(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	lower := func(t *testing.T, q string) chplan.Node {
		t.Helper()
		expr, err := tempo.Parse(q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", q, err)
		}
		return plan
	}
	predicateOf := func(t *testing.T, q string) chplan.Expr {
		t.Helper()
		filter, ok := lower(t, q).(*chplan.Filter)
		if !ok {
			t.Fatalf("Lower(%q): want *chplan.Filter, got %T", q, lower(t, q))
		}
		return filter.Predicate
	}

	// Odd nesting depth (1, 3): reference always empties, regardless of
	// what's wrapped (single comparison, AND, OR).
	for _, q := range []string{
		`{ !(kind = server) }`,
		`{ !(!(!(kind = server))) }`,
		`{ !(kind = server && span.foo = "x") }`,
		`{ !(kind = server || span.foo = "x") }`,
	} {
		pred := predicateOf(t, q)
		lit, ok := pred.(*chplan.LitBool)
		if !ok || lit.V {
			t.Errorf("Lower(%q): predicate = %#v; want LitBool{V:false}", q, pred)
		}
	}

	// Even nesting depth (2, 4): cancels, matching the positive form
	// byte-for-byte — compared via the deterministic chplan IR printer so
	// the whole plan (not just the predicate) must line up.
	wantPlan := spec.PrintChplan(lower(t, `{ kind = server }`))
	for _, q := range []string{
		`{ !(!(kind = server)) }`,
		`{ !(!(!(!(kind = server)))) }`,
	} {
		if _, isConst := predicateOf(t, q).(*chplan.LitBool); isConst {
			t.Errorf("Lower(%q): predicate folded to a constant; want the positive form's predicate", q)
		}
		gotPlan := spec.PrintChplan(lower(t, q))
		if gotPlan != wantPlan {
			t.Errorf("Lower(%q) plan =\n%s\nwant (same as the positive form):\n%s", q, gotPlan, wantPlan)
		}
	}

	// AND/OR operand wrapping a single comparison: reference drops the
	// NOT operand as the combinator's identity element, so the predicate
	// collapses to exactly the other operand's plan — never a
	// constant-false fold.
	otherOperandPlan := spec.PrintChplan(lower(t, `{ kind = client }`))
	for _, q := range []string{
		`{ kind = client && !(kind = server) }`,
		`{ !(kind = server) && kind = client }`,
		`{ kind = client || !(kind = server) }`,
		`{ !(kind = server) || kind = client }`,
	} {
		if _, isConst := predicateOf(t, q).(*chplan.LitBool); isConst {
			t.Errorf("Lower(%q): predicate folded to a constant; want the other operand's plan", q)
		}
		gotPlan := spec.PrintChplan(lower(t, q))
		if gotPlan != otherOperandPlan {
			t.Errorf("Lower(%q) plan =\n%s\nwant (same as `{ kind = client }` alone):\n%s", q, gotPlan, otherOperandPlan)
		}
	}

	// AND/OR operand wrapping a COMPOUND (AND/OR) sub-expression: no
	// identity-drop — reference's behaviour there isn't a clean rule (see
	// lowerUnaryNot's doc comment), so cerberus computes the
	// logically-correct answer: a real Binary{And/Or} predicate whose NOT
	// arm is an explicit FuncCall{"not"}, never folded to a constant.
	for _, tc := range []struct {
		q        string
		wantOp   chplan.BinaryOp
		wantNotL bool // NOT arm is the left operand (else right)
	}{
		{`{ kind = client && !(kind = server && span.foo = "x") }`, chplan.OpAnd, false},
		{`{ !(kind = server && span.foo = "x") && kind = client }`, chplan.OpAnd, true},
		{`{ kind = client || !(kind = server || span.foo = "x") }`, chplan.OpOr, false},
		{`{ !(kind = server || span.foo = "x") || kind = client }`, chplan.OpOr, true},
	} {
		pred := predicateOf(t, tc.q)
		bin, ok := pred.(*chplan.Binary)
		if !ok {
			t.Errorf("Lower(%q): predicate = %T, want *chplan.Binary", tc.q, pred)
			continue
		}
		if bin.Op != tc.wantOp {
			t.Errorf("Lower(%q): predicate op = %v, want %v", tc.q, bin.Op, tc.wantOp)
		}
		notArm := bin.Right
		if tc.wantNotL {
			notArm = bin.Left
		}
		call, ok := notArm.(*chplan.FuncCall)
		if !ok || call.Name != "not" {
			t.Errorf("Lower(%q): NOT arm = %#v, want *chplan.FuncCall{Name: \"not\"}", tc.q, notArm)
		}
	}
}

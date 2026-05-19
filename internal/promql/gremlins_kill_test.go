// Tests in this file pin behaviour that the gremlins mutation suite had
// reported as LIVED on the phase4-promql job — each one constructs an
// input that observably differentiates the original code from the
// mutated branch, so the test fails when the mutant is applied and the
// mutant is reported KILLED.
//
// See `.gremlins.yaml` for the mutation operators in play; the mutant
// IDs in each test's doc comment refer to gremlins's `file:line:col`
// notation as printed in the workflow logs.
//
// Conventions:
//   - one Test... per source-file cluster of related mutants
//   - assertions name the original behaviour explicitly, so a `<` ↔ `<=`
//     boundary flip or an `&&` ↔ `||` logical inversion on the named
//     operator falls out of scope and gets killed.
package promql

import (
	"math"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestFoldComparisonScalar_LessThanIsStrict kills the `<` → `<=`
// boundary flip at scalar.go:136 inside foldComparisonScalar's LSS
// case. PromQL's `<` is strict — `5 < 5` is false (0.0). A
// CONDITIONALS_BOUNDARY mutant flipping `<` to `<=` would return 1.0
// for equal operands, breaking Prom's scalar-scalar comparison
// semantics.
//
// Driven via TryFoldScalar so the kill ties to the public surface:
// `5 < bool 5` parses with ReturnBool set, lands on
// foldComparisonScalar(parser.LSS, 5, 5), and must yield 0.0.
func TestFoldComparisonScalar_LessThanIsStrict(t *testing.T) {
	t.Parallel()

	expr := mustParse(t, `5 < bool 5`)
	got, ok := TryFoldScalar(expr)
	if !ok {
		t.Fatalf("TryFoldScalar(%q) returned ok=false; expected scalar fold to succeed", `5 < bool 5`)
	}
	if got != 0 {
		t.Fatalf("foldComparisonScalar(LSS, 5, 5) = %v; want 0 (mutant `<` → `<=` would yield 1)", got)
	}
}

// TestFoldComparisonScalar_GreaterThanIsStrict kills the `>` → `>=`
// boundary flip at scalar.go:140 inside foldComparisonScalar's GTR
// case. PromQL's `>` is strict — `5 > 5` is false (0.0). A
// CONDITIONALS_BOUNDARY mutant flipping `>` to `>=` would return 1.0
// for equal operands.
func TestFoldComparisonScalar_GreaterThanIsStrict(t *testing.T) {
	t.Parallel()

	expr := mustParse(t, `5 > bool 5`)
	got, ok := TryFoldScalar(expr)
	if !ok {
		t.Fatalf("TryFoldScalar(%q) returned ok=false; expected scalar fold to succeed", `5 > bool 5`)
	}
	if got != 0 {
		t.Fatalf("foldComparisonScalar(GTR, 5, 5) = %v; want 0 (mutant `>` → `>=` would yield 1)", got)
	}
}

// TestFoldComparisonScalar_LessOrEqualIncludesEquality complements the
// LSS kill above: `<=` includes equality, so `5 <= bool 5` must yield
// 1.0. This pins the LTE boundary at scalar.go:138 — flipping `<=` to
// `<` (a hypothetical sibling mutant in the same family) would return
// 0 for the equality case. The test is also a regression backstop for
// the LSS kill in case a future refactor merges the cases.
func TestFoldComparisonScalar_LessOrEqualIncludesEquality(t *testing.T) {
	t.Parallel()

	expr := mustParse(t, `5 <= bool 5`)
	got, ok := TryFoldScalar(expr)
	if !ok {
		t.Fatalf("TryFoldScalar(%q) returned ok=false; expected scalar fold to succeed", `5 <= bool 5`)
	}
	if got != 1 {
		t.Fatalf("foldComparisonScalar(LTE, 5, 5) = %v; want 1 (equality holds for <=)", got)
	}
}

// mustParseExperimental parses a PromQL query with experimental
// functions (e.g. double_exponential_smoothing) enabled. The Prom
// parser refuses such names by default; the boundary-guard tests for
// lowerHoltWinters need them through to exercise the in-range checks.
func mustParseExperimental(t *testing.T, q string) parser.Expr {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	if expr == nil {
		t.Fatalf("ParseExpr(%q) returned nil", q)
	}
	return expr
}

// TestLowerHoltWinters_SmoothingFactorZeroRejected kills the `<=` → `<`
// boundary flip at range_fns.go:91 in the smoothing-factor guard. The
// guard `if sf <= 0 || sf >= 1` rejects the boundary value sf=0; a
// CONDITIONALS_BOUNDARY mutant relaxing `<=` to `<` would let sf=0
// through into the lowering, where the recurrence is undefined.
func TestLowerHoltWinters_SmoothingFactorZeroRejected(t *testing.T) {
	t.Parallel()

	expr := mustParseExperimental(t, `double_exponential_smoothing(up[5m], 0, 0.5)`)
	s := schema.DefaultOTelMetrics()
	_, err := lower(expr, s, lowerCtx{})
	if err == nil {
		t.Fatalf("expected holt_winters(sf=0, ...) to error; got nil (mutant `<=` → `<` at range_fns.go:91 would pass sf=0 through the (0,1) check)")
	}
}

// TestLowerHoltWinters_SmoothingFactorOneRejected kills the `>=` → `>`
// boundary flip at range_fns.go:91 in the smoothing-factor upper
// guard. Same shape as the lower-bound test: sf=1 sits exactly on the
// `>= 1` boundary; flipping `>=` to `>` would let sf=1 through.
func TestLowerHoltWinters_SmoothingFactorOneRejected(t *testing.T) {
	t.Parallel()

	expr := mustParseExperimental(t, `double_exponential_smoothing(up[5m], 1, 0.5)`)
	s := schema.DefaultOTelMetrics()
	_, err := lower(expr, s, lowerCtx{})
	if err == nil {
		t.Fatalf("expected holt_winters(sf=1, ...) to error; got nil (mutant `>=` → `>` at range_fns.go:91 would pass sf=1 through the (0,1) check)")
	}
}

// TestLowerHoltWinters_TrendFactorZeroRejected kills the `<=` → `<`
// boundary flip at range_fns.go:94 in the trend-factor guard. The
// guard `if tf <= 0 || tf >= 1` rejects the boundary value tf=0; a
// CONDITIONALS_BOUNDARY mutant relaxing `<=` to `<` would let tf=0
// through.
func TestLowerHoltWinters_TrendFactorZeroRejected(t *testing.T) {
	t.Parallel()

	expr := mustParseExperimental(t, `double_exponential_smoothing(up[5m], 0.5, 0)`)
	s := schema.DefaultOTelMetrics()
	_, err := lower(expr, s, lowerCtx{})
	if err == nil {
		t.Fatalf("expected holt_winters(tf=0, ...) to error; got nil (mutant `<=` → `<` at range_fns.go:94 would pass tf=0 through the (0,1) check)")
	}
}

// TestLowerHoltWinters_TrendFactorOneRejected kills the `>=` → `>`
// boundary flip at range_fns.go:94 in the trend-factor upper guard.
func TestLowerHoltWinters_TrendFactorOneRejected(t *testing.T) {
	t.Parallel()

	expr := mustParseExperimental(t, `double_exponential_smoothing(up[5m], 0.5, 1)`)
	s := schema.DefaultOTelMetrics()
	_, err := lower(expr, s, lowerCtx{})
	if err == nil {
		t.Fatalf("expected holt_winters(tf=1, ...) to error; got nil (mutant `>=` → `>` at range_fns.go:94 would pass tf=1 through the (0,1) check)")
	}
}

// TestRewriteAnchorToTimeUnix_QualifierGuardsName kills the
// INVERT_LOGICAL mutant at binary.go:396, where the guard
//
//	if v.Name == "anchor_ts" && v.Qualifier == ""
//
// must combine the two conditions with AND. Flipping `&&` to `||`
// would let a `ColumnRef{Name: "anchor_ts", Qualifier: "leg"}` slip
// through and get rewritten to the TimestampColumn — but Qualifier
// non-empty means the column belongs to a specific subquery leg, and
// the rewrite must NOT touch it. The test feeds in the qualified
// variant and asserts the ColumnRef returns unchanged.
func TestRewriteAnchorToTimeUnix_QualifierGuardsName(t *testing.T) {
	t.Parallel()

	original := &chplan.ColumnRef{Name: "anchor_ts", Qualifier: "leg"}
	s := schema.DefaultOTelMetrics()
	got := rewriteAnchorToTimeUnix(original, s)
	cr, ok := got.(*chplan.ColumnRef)
	if !ok {
		t.Fatalf("expected *ColumnRef, got %T", got)
	}
	if cr.Name != "anchor_ts" || cr.Qualifier != "leg" {
		t.Fatalf("expected qualified anchor_ts to pass through unchanged; got %#v (mutant `&&` → `||` at binary.go:396 would rewrite it to %q)",
			cr, s.TimestampColumn)
	}
}

// TestRewriteAnchorToTimeUnix_BareAnchorTsIsRewritten complements the
// qualifier-guard test above. A `ColumnRef{Name: "anchor_ts",
// Qualifier: ""}` is the canonical synthetic-leg shape and must be
// rewritten to the TimestampColumn — preventing the `&&` → `||` mutant
// from being killed by also rejecting the bare form. This test pins
// the "rewrite when both halves match" half of the conjunction.
func TestRewriteAnchorToTimeUnix_BareAnchorTsIsRewritten(t *testing.T) {
	t.Parallel()

	original := &chplan.ColumnRef{Name: "anchor_ts"}
	s := schema.DefaultOTelMetrics()
	got := rewriteAnchorToTimeUnix(original, s)
	cr, ok := got.(*chplan.ColumnRef)
	if !ok {
		t.Fatalf("expected *ColumnRef, got %T", got)
	}
	if cr.Name != s.TimestampColumn {
		t.Fatalf("expected bare anchor_ts rewritten to %q; got %q", s.TimestampColumn, cr.Name)
	}
}

// TestIsDefaultMatching_AllFourConjunctsRequired kills the conjunctive
// guard at binary.go:236-238 which combines four independent
// constraints with `&&`:
//
//	vm.Card == parser.CardOneToOne &&
//	    len(vm.MatchingLabels) == 0 &&
//	    len(vm.Include) == 0 &&
//	    !vm.On
//
// Each conjunct guards a single non-default knob; flipping any `==`
// to `!=` (CONDITIONALS_NEGATION) or any `&&` to `||` (INVERT_LOGICAL)
// must reverse the boolean for at least one of the cases below.
//
// Strategy: pin the canonical default (all four conjuncts satisfied →
// true) and four "one-non-default-knob" variants — only that knob
// changes from the default, so each variant uniquely exercises one
// conjunct.
func TestIsDefaultMatching_AllFourConjunctsRequired(t *testing.T) {
	t.Parallel()

	if !isDefaultMatching(nil) {
		t.Fatalf("nil VectorMatching must report default; got false")
	}

	defaultVM := &parser.VectorMatching{Card: parser.CardOneToOne}
	if !isDefaultMatching(defaultVM) {
		t.Fatalf("zero-value OneToOne VectorMatching must report default; got false")
	}

	// One-to-many cardinality.
	if isDefaultMatching(&parser.VectorMatching{Card: parser.CardOneToMany}) {
		t.Fatalf("CardOneToMany must not report default (kills `==` → `!=` on Card)")
	}

	// Non-empty MatchingLabels.
	if isDefaultMatching(&parser.VectorMatching{Card: parser.CardOneToOne, MatchingLabels: []string{"job"}}) {
		t.Fatalf("non-empty MatchingLabels must not report default (kills `== 0` → `!= 0` on MatchingLabels)")
	}

	// Non-empty Include.
	if isDefaultMatching(&parser.VectorMatching{Card: parser.CardOneToOne, Include: []string{"env"}}) {
		t.Fatalf("non-empty Include must not report default (kills `== 0` → `!= 0` on Include)")
	}

	// On=true (ignoring → on).
	if isDefaultMatching(&parser.VectorMatching{Card: parser.CardOneToOne, On: true}) {
		t.Fatalf("On=true must not report default (kills `!` flip on On)")
	}
}

// TestLowerClamp_NonLiteralBoundRejected kills the `||` → `&&` flip at
// instant_fns.go:128 in the clamp argument guard. The guard
//
//	if !okMin || !okMax { return ..., err }
//
// rejects clamp when EITHER bound fails to fold to a scalar literal.
// Flipping `||` to `&&` (gremlins INVERT_LOGICAL) would only reject
// when BOTH fail, letting one-side-non-literal calls through into the
// lowering with a misleading bound (the default zero from
// tryScalarLiteral) — silently producing an emitter that clamps to
// `[minLit, 0]` regardless of the actual upper bound.
//
// Input: `clamp(up, 0, time())` — the upper bound is `time()`, a
// scalar function call, not a literal; `tryScalarLiteral` returns
// `(0, false)`. Original returns the "clamp requires scalar-literal
// bounds" error. Mutant passes with maxB=0 — `0 < 0 = false` skips
// the degenerate-bounds filter, and the lowering emits a misleading
// clamp expression instead of erroring.
func TestLowerClamp_NonLiteralBoundRejected(t *testing.T) {
	t.Parallel()

	expr := mustParse(t, `clamp(up, 0, time())`)
	s := schema.DefaultOTelMetrics()
	_, err := lower(expr, s, lowerCtx{})
	if err == nil {
		t.Fatalf("expected clamp with non-literal upper bound to error; got nil (mutant `||` → `&&` at instant_fns.go:128 would only fail when BOTH bounds are non-literal)")
	}
}

// TestFoldBinaryScalar_DivByZeroNegativeBranches pins the `<` boundary
// at scalar.go:89 inside foldBinaryScalar's DIV case. The branch
//
//	if lhs < 0 { return math.Inf(-1), true }
//
// returns -Inf for any strictly-negative LHS divided by zero. The
// sibling `lhs == 0` branch (line 86) already handles the 0/0 case
// (NaN), and the fall-through returns +Inf. A boundary mutant would
// either misclassify the lhs=0 case (already caught earlier in the
// switch) or shift the negative/positive split — pinning two opposite
// signs catches both.
//
// Driven via TryFoldScalar on `(-1) / 0` (parses as
// BinaryExpr{UnaryExpr{NumberLiteral{1}}, DIV, NumberLiteral{0}}) so
// the kill ties to the public surface.
func TestFoldBinaryScalar_DivByZeroNegativeBranches(t *testing.T) {
	t.Parallel()

	negExpr := mustParse(t, `(-1) / 0`)
	gotNeg, ok := TryFoldScalar(negExpr)
	if !ok {
		t.Fatalf("TryFoldScalar(%q) returned ok=false", `(-1) / 0`)
	}
	if !math.IsInf(gotNeg, -1) {
		t.Fatalf("(-1)/0 = %v; want -Inf", gotNeg)
	}

	posExpr := mustParse(t, `1 / 0`)
	gotPos, ok := TryFoldScalar(posExpr)
	if !ok {
		t.Fatalf("TryFoldScalar(%q) returned ok=false", `1 / 0`)
	}
	if !math.IsInf(gotPos, 1) {
		t.Fatalf("1/0 = %v; want +Inf", gotPos)
	}

	zeroExpr := mustParse(t, `0 / 0`)
	gotZero, ok := TryFoldScalar(zeroExpr)
	if !ok {
		t.Fatalf("TryFoldScalar(%q) returned ok=false", `0 / 0`)
	}
	if !math.IsNaN(gotZero) {
		t.Fatalf("0/0 = %v; want NaN", gotZero)
	}
}

// Tests in this file pin behaviour that the gremlins mutation suite had
// reported as LIVED on a phase4-promql-* job (mutation.yml, split into
// sibling legs by cerberus issue #2636) — each one constructs an
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
	"strings"
	"testing"

	"github.com/prometheus/prometheus/model/labels"
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

// mustParseHoltWintersCall parses a `double_exponential_smoothing(...)`
// query with experimental functions enabled and returns the underlying
// *parser.Call. The Prom parser refuses the experimental name by default;
// the boundary-guard tests for lowerHoltWinters need the call through to
// exercise the in-range checks. These mutation tests drive lowerHoltWinters
// directly (rather than via the lowering dispatch) so the (0,1) smoothing/
// trend-factor boundary mutants are pinned at the guard itself, independent
// of how the dispatch routes the call.
func mustParseHoltWintersCall(t *testing.T, q string) *parser.Call {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	call, ok := expr.(*parser.Call)
	if !ok {
		t.Fatalf("ParseExpr(%q) = %T, want *parser.Call", q, expr)
	}
	return call
}

// TestLowerHoltWinters_SmoothingFactorZeroRejected kills the `<=` → `<`
// boundary flip at range_fns.go:91 in the smoothing-factor guard. The
// guard `if sf <= 0 || sf >= 1` rejects the boundary value sf=0; a
// CONDITIONALS_BOUNDARY mutant relaxing `<=` to `<` would let sf=0
// through into the lowering, where the recurrence is undefined.
func TestLowerHoltWinters_SmoothingFactorZeroRejected(t *testing.T) {
	t.Parallel()

	call := mustParseHoltWintersCall(t, `double_exponential_smoothing(up[5m], 0, 0.5)`)
	s := schema.DefaultOTelMetrics()
	_, err := lowerHoltWinters(call, s, lowerCtx{})
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

	call := mustParseHoltWintersCall(t, `double_exponential_smoothing(up[5m], 1, 0.5)`)
	s := schema.DefaultOTelMetrics()
	_, err := lowerHoltWinters(call, s, lowerCtx{})
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

	call := mustParseHoltWintersCall(t, `double_exponential_smoothing(up[5m], 0.5, 0)`)
	s := schema.DefaultOTelMetrics()
	_, err := lowerHoltWinters(call, s, lowerCtx{})
	if err == nil {
		t.Fatalf("expected holt_winters(tf=0, ...) to error; got nil (mutant `<=` → `<` at range_fns.go:94 would pass tf=0 through the (0,1) check)")
	}
}

// TestLowerHoltWinters_TrendFactorOneRejected kills the `>=` → `>`
// boundary flip at range_fns.go:94 in the trend-factor upper guard.
func TestLowerHoltWinters_TrendFactorOneRejected(t *testing.T) {
	t.Parallel()

	call := mustParseHoltWintersCall(t, `double_exponential_smoothing(up[5m], 0.5, 1)`)
	s := schema.DefaultOTelMetrics()
	_, err := lowerHoltWinters(call, s, lowerCtx{})
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

// TestLowerClamp_MixedBoundsTakeComputedPath kills the `&&` → `||`
// flip in lowerClamp's literal fast-path gate (`if okMin && okMax`).
// With the flip, `clamp(up, 0, time())` — literal min, computed max —
// would enter the literal branch carrying tryScalarLiteral's zero
// default for the max bound: `maxB(0) < minB(0)` is false, so the
// degenerate fold never fires and the lowering silently clamps every
// sample into `[0, 0]` instead of using the computed bound.
//
// The pin asserts the mixed shape routes to the COMPUTED path: a
// Project whose input is the runtime degenerate-bounds Filter
// (`not(max < min)` FuncCall predicate), not the literal branch's
// bare lowered selector.
func TestLowerClamp_MixedBoundsTakeComputedPath(t *testing.T) {
	t.Parallel()

	expr := mustParse(t, `clamp(up, 0, time())`)
	s := schema.DefaultOTelMetrics()
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower(clamp(up, 0, time())): %v", err)
	}
	pj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.Project", plan)
	}
	f, ok := pj.Input.(*chplan.Filter)
	if !ok {
		t.Fatalf("Project input = %T, want the runtime degenerate-bounds *chplan.Filter", pj.Input)
	}
	fc, ok := f.Predicate.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnNot {
		t.Fatalf("Filter predicate = %#v, want not(max < min) FuncCall", f.Predicate)
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

// labelRewriteProject descends to the Project that carries the
// label_replace / label_join rewrite expression.
//
// The lowering wraps that Project in the duplicate-labelset guard (#1839)
// — Project(Aggregate(rewrite Project)) — so the rewrite expression these
// mutation tests read sits two layers below the root. The walk asserts
// each layer rather than searching for the first Project it can find, so a
// future shape change fails loudly here instead of silently reading the
// wrong node and turning a mutation-kill assertion inert.
func labelRewriteProject(t *testing.T, plan chplan.Node) *chplan.Project {
	t.Helper()
	outer, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("expected *chplan.Project at the root, got %T", plan)
	}
	agg, ok := outer.Input.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("expected the duplicate-labelset guard *chplan.Aggregate under the root Project, got %T", outer.Input)
	}
	rewrite, ok := agg.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("expected the rewrite *chplan.Project under the guard Aggregate, got %T", agg.Input)
	}
	return rewrite
}

// TestLowerLabelJoin_SrcsSliceCapacityIsTight kills the two adjacent
// arithmetic mutants at label_fns.go:81:39 inside lowerLabelJoin's
// slice-capacity hint:
//
//	srcs := make([]string, 0, len(c.Args)-3)
//
// The `-` is gremlins ARITHMETIC_BASE (`-` → `+`) and the literal `3`
// is INVERT_NEGATIVES (`-3` → `+3`). Both mutants enlarge the initial
// capacity by 6 — `append` silently uses the extra headroom, so the
// resulting LabelJoin holds an identical Srcs slice and every
// semantic-level assertion (Dst / Separator / Srcs values / SQL output)
// passes under both branches.
//
// The only externally observable difference is the slice's cap:
// `make([]T, 0, N)` returns cap == N, and the lowering then appends
// exactly N times (one append per c.Args[3:] entry), so cap stays at N
// under the original arithmetic. With `+` in place of `-`, cap = N+6
// while len = N — and `len(c.Args) = 5` (one vector + dst + sep + 2
// srcs) means original cap = 2, mutant cap = 8.
//
// Calling lowerLabelJoin directly (rather than going through Lower /
// the optimizer) keeps the slice identity intact — no rule in the
// optimizer touches LabelJoin.Srcs, but going direct removes any
// possibility of a future fixup pass cloning the slice and masking
// the cap difference.
func TestLowerLabelJoin_SrcsSliceCapacityIsTight(t *testing.T) {
	t.Parallel()

	expr := mustParse(t, `label_join(up, "id", "-", "instance", "job")`)
	call, ok := expr.(*parser.Call)
	if !ok {
		t.Fatalf("expected *parser.Call, got %T", expr)
	}
	s := schema.DefaultOTelMetrics()

	plan, err := lowerLabelJoin(call, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lowerLabelJoin: %v", err)
	}
	proj := labelRewriteProject(t, plan)
	// Inner is a non-RangeWindow LWR shape (Aggregate over Filter over
	// Scan), so attrs sits at Projections[1] per
	// projectAttributesOverInner.
	if len(proj.Projections) != 4 {
		t.Fatalf("expected 4 projections (non-RangeWindow shape), got %d", len(proj.Projections))
	}
	lj, ok := proj.Projections[1].Expr.(*chplan.LabelJoin)
	if !ok {
		t.Fatalf("expected projections[1].Expr to be *chplan.LabelJoin, got %T", proj.Projections[1].Expr)
	}

	const wantSrcs = 2 // "instance", "job"
	if got := len(lj.Srcs); got != wantSrcs {
		t.Fatalf("len(Srcs) = %d; want %d", got, wantSrcs)
	}
	// Original code: cap == len(c.Args)-3 == 5-3 == 2.
	// `+` mutant: cap == 5+3 == 8.
	// `-3` → `+3` mutant: cap == 5+3 == 8 (same observable as above).
	// Both mutants must yield cap == 8; original yields cap == 2.
	if got := cap(lj.Srcs); got != wantSrcs {
		t.Fatalf("cap(Srcs) = %d; want %d (mutants `-` → `+` and `-3` → `+3` at label_fns.go:81:39 would yield cap=%d)",
			got, wantSrcs, len(call.Args)+3)
	}
}

// TestLowerLabelJoin_SrcsSliceCapacityIsTight_FiveSrcs reinforces the
// cap-assertion above with a larger argument list so the differential
// between original and mutant cap is unambiguous. With 5 srcs, c.Args
// has 8 entries (1 vector + dst + sep + 5 srcs):
//
//   - Original `-3`: cap = 5, len = 5 (exact fit, no headroom).
//   - `+` mutant:  cap = 11, len = 5 (6 cells of headroom).
//   - `-3` → `+3`: cap = 11, len = 5 (same observable).
//
// Two independent fixtures (2 srcs, 5 srcs) make accidental coincidence
// (e.g., a Go runtime growth schedule that lands on the original cap)
// impossible.
func TestLowerLabelJoin_SrcsSliceCapacityIsTight_FiveSrcs(t *testing.T) {
	t.Parallel()

	expr := mustParse(t, `label_join(up, "fqdn", "/", "a", "b", "c", "d", "e")`)
	call, ok := expr.(*parser.Call)
	if !ok {
		t.Fatalf("expected *parser.Call, got %T", expr)
	}
	s := schema.DefaultOTelMetrics()

	plan, err := lowerLabelJoin(call, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lowerLabelJoin: %v", err)
	}
	proj := labelRewriteProject(t, plan)
	lj, ok := proj.Projections[1].Expr.(*chplan.LabelJoin)
	if !ok {
		t.Fatalf("expected projections[1].Expr to be *chplan.LabelJoin, got %T", proj.Projections[1].Expr)
	}

	const wantSrcs = 5
	if got := len(lj.Srcs); got != wantSrcs {
		t.Fatalf("len(Srcs) = %d; want %d", got, wantSrcs)
	}
	if got := cap(lj.Srcs); got != wantSrcs {
		t.Fatalf("cap(Srcs) = %d; want %d (mutants at label_fns.go:81:39 would yield cap=%d)",
			got, wantSrcs, len(call.Args)+3)
	}
}

// TestLowerLabelJoin_NonLiteralSrcErrorIndexesParamName kills the two
// adjacent arithmetic mutants at label_fns.go:83:79 inside the
// per-src error-formatting:
//
//	fmt.Sprintf("src_label_%d", i-2)
//
// The `-` is gremlins ARITHMETIC_BASE (`-` → `+`) and the literal `2`
// is INVERT_NEGATIVES (`-2` → `+2`). Both mutants change the parameter
// name embedded in the error message — surfaced only on the
// non-StringLiteral guard path inside stringArg.
//
// The loop iterates `i := 3 .. len(c.Args)-1`. The intent of `i-2` is
// to print the 1-based source-label index (src_label_1, src_label_2,
// …): at i=3 → "src_label_1", at i=4 → "src_label_2", etc. Mutating
// `-` to `+` shifts every printed index by +4 (i=3 → "src_label_5",
// i=4 → "src_label_6", …); mutating `-2` to `+2` does the same. The
// kill therefore pins the printed index at a chosen position.
//
// Strategy: construct a parser.Call by hand with a non-StringLiteral
// in the second src slot (c.Args[4], i=4). The PromQL parser refuses
// to accept this shape, so we bypass it and feed lowerLabelJoin
// directly. The original code prints "src_label_2"; both mutants
// print "src_label_6".
func TestLowerLabelJoin_NonLiteralSrcErrorIndexesParamName(t *testing.T) {
	t.Parallel()

	// Args: [vector, dst, separator, src1, src2_nonliteral]
	//   index   0      1     2         3     4
	// The non-literal slot is at c.Args[4] → i=4 in the loop → the
	// original "src_label_%d" formats as "src_label_2" (4-2=2). Both
	// mutants format as "src_label_6" (4+2=6).
	innerSelector := &parser.VectorSelector{
		Name: "up",
	}
	call := &parser.Call{
		Func: parser.MustGetFunction("label_join"),
		Args: parser.Expressions{
			innerSelector,
			&parser.StringLiteral{Val: "id"},
			&parser.StringLiteral{Val: "-"},
			&parser.StringLiteral{Val: "instance"},
			// Non-string-literal at src position 2 (i=4 in the loop).
			// A NumberLiteral is convenient — definitely not a
			// *parser.StringLiteral, so stringArg's type-assert fails.
			&parser.NumberLiteral{Val: 1.5},
		},
	}
	s := schema.DefaultOTelMetrics()

	_, err := lowerLabelJoin(call, s, lowerCtx{})
	if err == nil {
		t.Fatalf("expected non-literal src arg to error; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "src_label_2") {
		t.Fatalf("error %q does not mention %q (mutants `-` → `+` and `-2` → `+2` at label_fns.go:83:79 would emit %q)",
			msg, "src_label_2", "src_label_6")
	}
	// Defensive: also ensure the mutant string is NOT present (e.g., if a
	// future refactor printed both indices, the positive assertion above
	// would still pass while the mutant survived).
	if strings.Contains(msg, "src_label_6") {
		t.Fatalf("error %q contains the mutant param name %q", msg, "src_label_6")
	}
}

// TestLowerLabelJoin_NonLiteralSrcErrorIndexesParamName_FirstSlot
// reinforces the kill above by exercising the i=3 boundary — the very
// first src position. With the original `i-2` arithmetic this prints
// "src_label_1"; both mutants print "src_label_5".
//
// Two cases at different i values rule out an accidental coincidence
// where one specific mutant happens to print the original-style index
// (e.g., if a refactor used `i & 0x3` or similar).
func TestLowerLabelJoin_NonLiteralSrcErrorIndexesParamName_FirstSlot(t *testing.T) {
	t.Parallel()

	innerSelector := &parser.VectorSelector{
		Name: "up",
	}
	call := &parser.Call{
		Func: parser.MustGetFunction("label_join"),
		Args: parser.Expressions{
			innerSelector,
			&parser.StringLiteral{Val: "id"},
			&parser.StringLiteral{Val: "-"},
			// Non-string-literal at the first src slot (i=3).
			&parser.NumberLiteral{Val: 2.5},
		},
	}
	s := schema.DefaultOTelMetrics()

	_, err := lowerLabelJoin(call, s, lowerCtx{})
	if err == nil {
		t.Fatalf("expected non-literal src arg to error; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "src_label_1") {
		t.Fatalf("error %q does not mention %q (mutants at label_fns.go:83:79 would emit %q)",
			msg, "src_label_1", "src_label_5")
	}
	if strings.Contains(msg, "src_label_5") {
		t.Fatalf("error %q contains the mutant param name %q", msg, "src_label_5")
	}
}

// TestAbsentAttrsMap_ArgsSliceCapacityIsTight kills the ARITHMETIC_BASE
// mutant at absent.go:absentAttrsMap:`for _, name := range order` inside absentAttrsMap's slice-capacity
// hint:
//
//	args := make([]chplan.Expr, 0, len(pairs)*2)
//
// Each of the len(pairs) iterations below appends exactly 2 elements
// (one LitString per key, one per value), so len(args) after the loop
// is always len(pairs)*2 — matching the pre-allocated capacity exactly
// (no growth). Flipping `*` to `/` shrinks the hint to len(pairs)/2,
// forcing `append` to grow past it, which — per Go's amortized growth
// schedule — lands on a capacity other than len(pairs)*2. Semantic-
// level assertions (the emitted Map() args) pass under both branches;
// only cap() distinguishes them, mirroring
// TestLowerLabelJoin_SrcsSliceCapacityIsTight's own strategy for the
// analogous label_fns.go:81:39 pair.
func TestAbsentAttrsMap_ArgsSliceCapacityIsTight(t *testing.T) {
	t.Parallel()

	matchers := []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, "job", "a"),
		labels.MustNewMatcher(labels.MatchEqual, "instance", "b"),
		labels.MustNewMatcher(labels.MatchEqual, "env", "c"),
	}
	got := absentAttrsMap(matchers)
	fc, ok := got.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnMap {
		t.Fatalf("absentAttrsMap(3 equality matchers) = %#v, want a Map() FuncCall", got)
	}

	const wantPairs = 3
	if got := len(fc.Args); got != wantPairs*2 {
		t.Fatalf("len(Args) = %d; want %d (3 pairs * 2)", got, wantPairs*2)
	}
	// Original: cap == len(pairs)*2 == 6, exact fit.
	// `/` mutant: cap starts at len(pairs)/2 == 1, then grows past it
	// while appending 6 elements — never lands back on 6.
	if got := cap(fc.Args); got != wantPairs*2 {
		t.Fatalf("cap(Args) = %d; want %d (mutant `*` -> `/` at absent.go:absentAttrsMap:`for _, name := range order` would yield a different cap via forced regrowth)",
			got, wantPairs*2)
	}
}

// TestLowerSortByLabel_KeysSliceCapacityIsTight kills the two adjacent
// mutants at sort.go:192:48 inside lowerSortByLabel's slice-capacity
// hint:
//
//	keys := make([]chplan.OrderKey, 0, len(c.Args)-1)
//
// The `-` is ARITHMETIC_BASE (`-` -> `+`) and the literal `1` is the
// INVERT_NEGATIVES sibling (`-1` -> `+1`) — both produce the identical
// observable effect: cap == len(c.Args)+1 instead of len(c.Args)-1. The
// loop appends exactly len(c.Args)-1 keys (one per label argument), so
// the original capacity is an exact fit; both mutants over-allocate by
// 2, and — because 0 <= len(c.Args)-1 <= len(c.Args)+1 — no growth is
// ever triggered to mask the difference the way the absent.go case
// above relies on regrowth to detect. cap() is checked directly.
func TestLowerSortByLabel_KeysSliceCapacityIsTight(t *testing.T) {
	t.Parallel()

	// sort_by_label is experimental in the reference parser, unlike the
	// double_exponential_smoothing call mustParseHoltWintersCall parses —
	// but that helper's own EnableExperimentalFunctions parser covers it
	// identically, so it is reused here instead of a third near-duplicate
	// parser construction.
	call := mustParseHoltWintersCall(t, `sort_by_label(up, "a", "b", "c")`)
	s := schema.DefaultOTelMetrics()

	plan, err := lowerSortByLabel(call, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lowerSortByLabel: %v", err)
	}
	ob, ok := plan.(*chplan.OrderBy)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.OrderBy", plan)
	}

	const wantKeys = 3 // "a", "b", "c"
	if got := len(ob.Keys); got != wantKeys {
		t.Fatalf("len(Keys) = %d; want %d", got, wantKeys)
	}
	// Original: cap == len(c.Args)-1 == 4-1 == 3, exact fit.
	// Both mutants: cap == 4+1 == 5.
	if got := cap(ob.Keys); got != wantKeys {
		t.Fatalf("cap(Keys) = %d; want %d (mutants at sort.go:`keys := make([]chplan.OrderKey, 0, len(c.Args)-1)` would yield cap=%d)",
			got, wantKeys, len(call.Args)+1)
	}
}

// TestLowerClamp_SingleArgHistogramShortcutTakesHistogramPath kills the
// CONDITIONALS_BOUNDARY mutant at instant_fns.go:202:17, which flips
//
//	if len(c.Args) >= 1 {
//
// to `> 1`. Every VALID clamp/clamp_max/clamp_min call the parser
// accepts always carries at least 2 arguments, so a real query can
// never observe the boundary — the only way to reach it with exactly 1
// argument is to hand-build the *parser.Call the way this test does,
// bypassing the parser's own arity check (the same technique
// TestLowerLabelJoin_NonLiteralSrcErrorIndexesParamName uses just
// above).
//
// With exactly 1 histogram-valued argument: the original `>= 1` routes
// through lowerExpHistogramArgAsCanonicalFloat and returns successfully
// (ok=true short-circuits before the switch's own arg-count check ever
// runs). The `> 1` mutant evaluates false for len==1, skips the
// histogram routing entirely, and falls into the switch's
// `len(c.Args) != 2` guard, erroring "clamp_max expects 2 arguments,
// got 1" instead.
func TestLowerClamp_SingleArgHistogramShortcutTakesHistogramPath(t *testing.T) {
	t.Parallel()

	histArg := mustParse(t, `demo_latency_exp_hist`)
	call := &parser.Call{
		Func: parser.MustGetFunction("clamp_max"),
		Args: parser.Expressions{histArg},
	}
	s := schema.DefaultOTelMetrics()

	plan, err := lowerClamp(call, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lowerClamp(clamp_max(<1 histogram arg>)): unexpected error %v "+
			"(mutant `>= 1` -> `> 1` at instant_fns.go:`>= 1` would skip the histogram "+
			"shortcut and fail the switch's arg-count check instead)", err)
	}
	if plan == nil {
		t.Fatalf("lowerClamp returned a nil plan alongside a nil error")
	}
}

// TestLowerClamp_EqualLiteralBoundsTakeComputedPath kills the
// CONDITIONALS_BOUNDARY mutant at instant_fns.go:271:12, which flips
//
//	if maxB < minB {
//
// to `<= `. Prom's funcClamp only short-circuits to an empty vector
// when max is STRICTLY less than min; `clamp(v, 5, 5)` (equal bounds)
// is a normal, non-degenerate clamp that pins every sample to exactly
// 5 via greatest(min, least(max, v)). The `<=` mutant would treat the
// equal-bounds case as degenerate too, returning the zero-row
// `Filter{Predicate: false}` shape instead of the computed
// greatest/least projection.
func TestLowerClamp_EqualLiteralBoundsTakeComputedPath(t *testing.T) {
	t.Parallel()

	expr := mustParse(t, `clamp(up, 5, 5)`)
	s := schema.DefaultOTelMetrics()
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower(clamp(up, 5, 5)): %v", err)
	}

	// The degenerate-bounds shape is a *chplan.Filter with a literal
	// `false` predicate sitting directly at the root (see the maxB <
	// minB branch in instant_fns.go). The non-degenerate path instead
	// returns whatever guardedValueProjection built (a *chplan.Project
	// for a plain single-name selector like `up`), never a bare
	// constant-false Filter.
	if f, ok := plan.(*chplan.Filter); ok {
		if lb, ok := f.Predicate.(*chplan.LitBool); ok && !lb.V {
			t.Fatalf("clamp(up, 5, 5) lowered to the degenerate-bounds empty Filter %#v; "+
				"want the computed greatest/least projection (mutant `<` -> `<=` at "+
				"instant_fns.go:`if maxB < minB` would treat equal bounds as degenerate)", f)
		}
	}
	if _, ok := plan.(*chplan.Project); !ok {
		t.Fatalf("plan = %T, want *chplan.Project (guardedValueProjection's shape for a "+
			"plain single-name selector)", plan)
	}
}

// TestLowerInfo_ArgCountRejectsOutOfRange kills the INVERT_LOGICAL
// mutant at info_fn.go:83:21, which flips
//
//	if len(c.Args) < 1 || len(c.Args) > 2 {
//
// to `&&`. No integer argument count can be simultaneously < 1 AND >
// 2, so the mutant's conjunction is always false — it would accept
// ANY argument count, including 0, silently skipping the validation
// that guards `c.Args[0]` from an out-of-range index a few lines
// below. A 0-argument info() call can only be constructed by hand
// (the parser itself enforces info()'s 1-or-2-argument signature), so
// this test bypasses the parser the same way the other hand-built-AST
// tests in this file do.
func TestLowerInfo_ArgCountRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	call := &parser.Call{
		Func: parser.MustGetFunction("info"),
		Args: parser.Expressions{},
	}
	s := schema.DefaultOTelMetrics()

	_, err := lowerInfo(call, s, lowerCtx{})
	if err == nil {
		t.Fatalf("expected info() with 0 arguments to error; got nil " +
			"(mutant `||` -> `&&` at info_fn.go:`if len(c.Args) < 1 || len(c.Args) > 2` can never be true, so " +
			"validation never fires and c.Args[0] would index out of range)")
	}
	if !strings.Contains(err.Error(), "got 0") {
		t.Fatalf("error %q does not mention the actual arg count", err.Error())
	}
}

// TestScalarGuardPlan_LoweringErrorPropagates kills the
// CONDITIONALS_NEGATION mutant at scalar_guard.go:92:9, which flips
//
//	if err != nil {
//		return nil, err
//	}
//
// to `err == nil`. When the inner lowerScalarArg call fails, the
// original code propagates the error and returns a nil plan. The
// mutant's flipped condition is false on a real error, so control
// falls through to `return syntheticScalarVector(value, nil, s, ctx),
// nil` — building a synthetic vector out of value (nil, since lowering
// failed) and swallowing the error entirely.
//
// `scalar()` called with 0 arguments is a lowerScalarArg error the
// parser itself would normally reject at parse time, so the *parser.Call
// is hand-built the same way the other arity-bypass tests in this file
// are.
func TestScalarGuardPlan_LoweringErrorPropagates(t *testing.T) {
	t.Parallel()

	e := &parser.Call{
		Func: parser.MustGetFunction("scalar"),
		Args: parser.Expressions{},
	}
	s := schema.DefaultOTelMetrics()

	plan, err := scalarGuardPlan(e, s, lowerCtx{})
	if err == nil {
		t.Fatalf("expected scalarGuardPlan to propagate lowerScalarArg's error; got nil err, "+
			"plan=%#v (mutant `!=` -> `==` at scalar_guard.go:scalarGuardPlan:`if err != nil` would swallow the error "+
			"and build a synthetic vector from a nil value instead)", plan)
	}
	if plan != nil {
		t.Fatalf("expected a nil plan alongside the propagated error; got %#v", plan)
	}
}

// TestHoltWintersFactors_MixedLiteralComputedTakesComputedPath kills
// the INVERT_LOGICAL mutant at range_fns.go:246:10, which flips
//
//	if okSf && okTf {
//		return sf, tf, nil, nil, nil
//	}
//
// to `||`. With one literal factor and one computed factor (okSf !=
// okTf), the original code must fall through to the computed path,
// returning `(0, 0, sfExpr, tfExpr, nil)` — both expression slots
// populated, both float slots zeroed, because [chplan.RangeWindow]'s
// positional ScalarExprs pair must stay complete. The `||` mutant's
// condition is true whenever EITHER factor is literal, so it takes the
// early-return literal path instead, returning the literal factor's own
// float value with both expression slots left nil — dropping the
// computed factor's expression entirely.
func TestHoltWintersFactors_MixedLiteralComputedTakesComputedPath(t *testing.T) {
	t.Parallel()

	call := mustParseHoltWintersCall(t, `double_exponential_smoothing(up[5m], 0.5, time())`)
	s := schema.DefaultOTelMetrics()

	sf, tf, sfExpr, tfExpr, err := holtWintersFactors(call.Args[1], call.Args[2], s, lowerCtx{})
	if err != nil {
		t.Fatalf("holtWintersFactors: %v", err)
	}
	if sfExpr == nil || tfExpr == nil {
		t.Fatalf("expected both sfExpr and tfExpr populated on the computed path "+
			"(sfExpr=%v tfExpr=%v sf=%v tf=%v); mutant `&&` -> `||` at range_fns.go:`if okSf && okTf` "+
			"would take the literal-only fast path and leave them nil",
			sfExpr, tfExpr, sf, tf)
	}
	if sf != 0 || tf != 0 {
		t.Fatalf("expected sf=0 tf=0 on the computed path (the real values ride in "+
			"sfExpr/tfExpr instead); got sf=%v tf=%v", sf, tf)
	}
}

// TestSynthLabelsFromMatchers_NameSkipContinuesPastLaterMatchers kills
// the INVERT_LOOPCTRL mutant at absent.go:390:4, where
//
//	if m.Name == model.MetricNameLabel {
//		continue
//	}
//
// flips to `break`. With only ONE matcher in the list, `continue` and
// `break` are indistinguishable (both end the loop). The kill requires
// a `__name__` matcher followed by further equality matchers: the
// original `continue` skips just the `__name__` entry and keeps
// processing the rest, while `break` would abandon the loop the moment
// it sees `__name__`, silently dropping every matcher positioned after
// it.
func TestSynthLabelsFromMatchers_NameSkipContinuesPastLaterMatchers(t *testing.T) {
	t.Parallel()

	matchers := []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, "__name__", "demo_metric"),
		labels.MustNewMatcher(labels.MatchEqual, "job", "a"),
		labels.MustNewMatcher(labels.MatchEqual, "instance", "b"),
	}
	got := synthLabelsFromMatchers(matchers)

	want := map[string]string{"job": "a", "instance": "b"}
	if len(got) != len(want) {
		t.Fatalf("synthLabelsFromMatchers = %#v; want %d labels (mutant `continue` -> "+
			"`break` at absent.go:390:4 would abandon the loop at __name__ and drop "+
			"every matcher after it, yielding 0)", got, len(want))
	}
	for _, sl := range got {
		if wantV, ok := want[sl.Key]; !ok || wantV != sl.Value {
			t.Fatalf("unexpected label %+v in result %#v", sl, got)
		}
	}
}

// TestAbsentAttrsMap_NameSkipContinuesPastLaterMatchers kills the
// INVERT_LOOPCTRL mutant at absent.go:440:4 — absentAttrsMap's own
// `__name__`-skip, the mirror of synthLabelsFromMatchers' loop just
// above (see that test's doc comment for why a single-matcher list
// cannot distinguish `continue` from `break`).
func TestAbsentAttrsMap_NameSkipContinuesPastLaterMatchers(t *testing.T) {
	t.Parallel()

	matchers := []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, "__name__", "demo_metric"),
		labels.MustNewMatcher(labels.MatchEqual, "job", "a"),
		labels.MustNewMatcher(labels.MatchEqual, "instance", "b"),
	}
	got := absentAttrsMap(matchers)
	fc, ok := got.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnMap {
		t.Fatalf("absentAttrsMap = %#v; want a Map() FuncCall over the surviving pairs "+
			"(mutant `continue` -> `break` at absent.go:440:4 would abandon the loop at "+
			"__name__ and drop every matcher after it, yielding the empty-map literal)", got)
	}
	// 2 surviving pairs (job, instance) * 2 args each.
	const wantArgs = 4
	if got := len(fc.Args); got != wantArgs {
		t.Fatalf("len(Args) = %d; want %d", got, wantArgs)
	}
}

// mixedDiscriminatorMarkerProject is a *chplan.Project whose single
// projection publishes chplan.MixedDiscriminatorColumn — the one shape
// [chplan.RowShapeOf] recognises as chplan.MixedRowShape (see that
// function's own doc comment). Used below purely to make
// guardLabelRewriteCollision's `mixed` local report true without
// depending on a real mixed-`or` lowering.
func mixedDiscriminatorMarkerProject(input chplan.Node) *chplan.Project {
	return &chplan.Project{
		Input: input,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: chplan.MixedDiscriminatorColumn}, Alias: chplan.MixedDiscriminatorColumn},
		},
	}
}

// TestGuardLabelRewriteCollision_MixedPayloadSkipContinuesLoop kills
// the INVERT_LOOPCTRL mutant at duplicate_labelset_guard.go:391:5,
// inside guardLabelRewriteCollision's per-projection switch:
//
//	default:
//		if mixed && mixedPayload[name] {
//			continue
//		}
//
// flips to `break`. A single qualifying projection cannot distinguish
// `continue` from `break` (the loop would end either way), so the
// fixture needs a mixed-payload column FOLLOWED by another projection
// that must still be reachable. This test forces `keyOnStep = true`
// (via an Aggregate input whose GroupByAliases already names the
// timestamp column, see guardKeysOnTimestamp) so the trailing
// projection lands in the group key, then asserts its alias survived.
func TestGuardLabelRewriteCollision_MixedPayloadSkipContinuesLoop(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	stepKeyedAgg := &chplan.Aggregate{GroupByAliases: []string{s.TimestampColumn}}
	mixedMarker := mixedDiscriminatorMarkerProject(stepKeyedAgg)

	const extraStepCol = "extra_step_col"
	rewritten := &chplan.Project{
		Input: mixedMarker,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: chplan.HistogramCountColumn}, Alias: chplan.HistogramCountColumn},
			{Expr: &chplan.ColumnRef{Name: extraStepCol}, Alias: extraStepCol},
		},
	}

	plan := guardLabelRewriteCollision(rewritten, s)
	agg, ok := plan.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.Aggregate (rewritten publishes none of the four "+
			"canonical columns, so guardLabelRewriteCollision must return the raw guard)", plan)
	}

	found := false
	for _, alias := range agg.GroupByAliases {
		if alias == extraStepCol {
			found = true
		}
	}
	if !found {
		t.Fatalf("GroupByAliases = %#v; want %q present (mutant `continue` -> `break` at "+
			"duplicate_labelset_guard.go:391:5 would abandon the loop at the mixed-payload "+
			"histogram column and never reach the projection after it)", agg.GroupByAliases, extraStepCol)
	}
}

// TestGuardLabelRewriteCollision_KeyOnStepSkipContinuesLoop kills the
// INVERT_LOOPCTRL mutant at duplicate_labelset_guard.go:400:5:
//
//	if keyOnStep {
//		groupBy = append(groupBy, &chplan.ColumnRef{Name: name})
//		aliases = append(aliases, name)
//		continue
//	}
//
// flips to `break`. Two non-payload, non-canonical projections in a
// row.Input needs `keyOnStep = true` (this test's Aggregate input
// already names the timestamp column) so BOTH hit this branch; the
// second is reachable only if the first's `continue` doesn't end the
// loop.
func TestGuardLabelRewriteCollision_KeyOnStepSkipContinuesLoop(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	stepKeyedAgg := &chplan.Aggregate{GroupByAliases: []string{s.TimestampColumn}}

	const colA, colB = "col_a", "col_b"
	rewritten := &chplan.Project{
		Input: stepKeyedAgg,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: colA}, Alias: colA},
			{Expr: &chplan.ColumnRef{Name: colB}, Alias: colB},
		},
	}

	plan := guardLabelRewriteCollision(rewritten, s)
	agg, ok := plan.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.Aggregate", plan)
	}

	wantAliases := map[string]bool{colA: true, colB: true}
	gotAliases := map[string]bool{}
	for _, alias := range agg.GroupByAliases {
		gotAliases[alias] = true
	}
	for name := range wantAliases {
		if !gotAliases[name] {
			t.Fatalf("GroupByAliases = %#v; want both %q and %q present (mutant `continue` "+
				"-> `break` at duplicate_labelset_guard.go:400:5 would abandon the loop after "+
				"the first key-on-step projection and never reach the second)",
				agg.GroupByAliases, colA, colB)
		}
	}
}

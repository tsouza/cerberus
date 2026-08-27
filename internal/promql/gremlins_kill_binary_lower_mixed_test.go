// Tests in this file kill the LIVED gremlins mutants assigned to the
// binary.go / lower_strategy.go / histogram_native_mixed_or_{math_fn,
// aggregate_count_values,aggregate_presence,datefn}.go cluster from a
// phase4-promql-* mutation run (mutation.yml). Each test constructs an
// input that observably differentiates the original code from the
// mutated branch. See gremlins_kill_test.go's header for the shared
// conventions (mutant IDs in each test's doc comment are gremlins's
// `file:line:col`).
package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// mustParseExperimental parses q with experimental PromQL functions
// enabled (double_exponential_smoothing and friends aren't needed here,
// but the mixed-or fixtures in this file mirror the
// EnableExperimentalFunctions setting the sibling
// histogram_native_mixed_or_*_test.go files already use).
func mustParseExperimental(t *testing.T, q string) parser.Expr {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	return expr
}

// lowerMixedOrAt lowers q at a fixed instant via the public LowerAt entry
// point against the default OTel schema — the same shape the sibling
// histogram_native_mixed_or_*_test.go files use.
func lowerMixedOrAt(t *testing.T, q string) chplan.Node {
	t.Helper()
	expr := mustParseExperimental(t, q)
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan, err := LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt(%q): unexpected error: %v", q, err)
	}
	return plan
}

// clampFamilyNewValue lowers a clamp-family call directly wrapping a
// mixed float/histogram `or` and returns the Value projection's
// expression — the last of the four canonical projections
// [projectCanonicalFloatValue] builds.
func clampFamilyNewValue(t *testing.T, q string) chplan.Expr {
	t.Helper()
	plan := lowerMixedOrAt(t, q)
	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("lower(%q): plan = %T, want *chplan.Project", q, plan)
	}
	if len(proj.Projections) == 0 {
		t.Fatalf("lower(%q): Project has no projections", q)
	}
	return proj.Projections[len(proj.Projections)-1].Expr
}

// ---------------------------------------------------------------------
// binary.go
// ---------------------------------------------------------------------

// TestIsVectorTypedSyntheticOperand_ConjunctsAndGuard kills two mutants
// on the same line, binary.go:289:
//
//	return ok && call.Func != nil && call.Func.Name == "vector"
//
// INVERT_LOGICAL at col 14 flips the first `&&` to `||`. Go's `&&`/`||`
// precedence is equal-left-to-right for this expression's grouping
// ((ok && x) && y vs ok || (x && y)), so the mutant becomes
// `ok || (call.Func != nil && call.Func.Name == "vector")`. When e is
// NOT a *parser.Call (the `ok=false` default-case branch), `call` is a
// nil *parser.Call; evaluating `call.Func` under the mutant's OR
// (which does not short-circuit on ok=false the way the original AND
// does) panics on the nil pointer dereference. The original code never
// evaluates call.Func in that case and safely returns false.
//
// CONDITIONALS_NEGATION at col 27 flips `call.Func != nil` to
// `call.Func == nil`, which makes the whole expression always false for
// any real *parser.Call (since Func is always set on a parsed call) —
// killed by asserting `vector(1)` reports true.
func TestIsVectorTypedSyntheticOperand_ConjunctsAndGuard(t *testing.T) {
	t.Parallel()

	// Not a *parser.Call at all: hits the `ok=false` default branch.
	// Must return false WITHOUT panicking.
	numLit := mustParse(t, `5`)
	if isVectorTypedSyntheticOperand(numLit) {
		t.Fatalf("isVectorTypedSyntheticOperand(NumberLiteral) = true, want false")
	}

	// A *parser.Call to a function OTHER than "vector" — Func is always
	// non-nil for any real call, so this must not panic and must report
	// false via the Name comparison, not the nil-guard.
	timeCall := mustParse(t, `time()`)
	if isVectorTypedSyntheticOperand(timeCall) {
		t.Fatalf("isVectorTypedSyntheticOperand(time()) = true, want false")
	}

	// The genuine positive case: vector(1) must report true. A
	// CONDITIONALS_NEGATION mutant flipping `!=` to `==` at col 27 would
	// make this false unconditionally.
	vectorCall := mustParse(t, `vector(1)`)
	if !isVectorTypedSyntheticOperand(vectorCall) {
		t.Fatalf("isVectorTypedSyntheticOperand(vector(1)) = false, want true (mutant `!=`→`==` at binary.go:289:27 would always return false)")
	}
}

// TestFoldSyntheticVectorBinary_ReturnBoolOnlyWrapsComparisons kills the
// INVERT_LOGICAL mutant at binary.go:423:22:
//
//	if isComparison(op) && returnBool {
//	    newValue = &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: ...}
//	}
//
// Every REAL caller reaches this line only after the early return on
// line 418 (`isComparison(op) && !returnBool` already handled) combined
// with PromQL's grammar-level invariant that `bool` only parses after a
// comparison operator (returnBool=true implies isComparison(op)=true).
// Together those two facts mean the only states reachable via normal
// parsing are (isComparison=true, returnBool=true) and
// (isComparison=false, returnBool=false) — for both, `&&` and `||`
// agree. Killing the mutant requires calling foldSyntheticVectorBinary
// directly with the combination the real call sites can never produce:
// an arithmetic op with returnBool=true.
func TestFoldSyntheticVectorBinary_ReturnBoolOnlyWrapsComparisons(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	ctx := lowerCtx{}
	synth, err := lowerVectorVectorOperand(mustParse(t, `time()`), s, ctx)
	if err != nil {
		t.Fatalf("lowerVectorVectorOperand(time()): %v", err)
	}
	vec, err := lowerVectorVectorOperand(mustParse(t, `up`), s, ctx)
	if err != nil {
		t.Fatalf("lowerVectorVectorOperand(up): %v", err)
	}
	vecExpr := mustParse(t, `up`)

	// op=OpAdd (arithmetic) with returnBool=true never occurs via
	// lowerVectorVector's real call path. Original:
	// isComparison(false) && returnBool(true) = false, so newValue stays
	// the raw Binary. Mutant `&&`→`||` at binary.go:423:22 would wrap it
	// in toFloat64 regardless.
	plan := foldSyntheticVectorBinary(synth, vec, vecExpr, chplan.OpAdd, true, true, s)
	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("foldSyntheticVectorBinary result = %T, want *chplan.Project", plan)
	}
	if len(proj.Projections) == 0 {
		t.Fatalf("foldSyntheticVectorBinary result Project has no projections")
	}
	valueExpr := proj.Projections[len(proj.Projections)-1].Expr
	if _, wrapped := valueExpr.(*chplan.FuncCall); wrapped {
		t.Fatalf("Value = %#v, want *chplan.Binary (mutant `&&`→`||` at binary.go:423:22 wraps arithmetic ops in toFloat64 too when returnBool=true)", valueExpr)
	}
	if _, ok := valueExpr.(*chplan.Binary); !ok {
		t.Fatalf("Value = %T, want *chplan.Binary", valueExpr)
	}
}

// TestLowerVectorScalar_ReturnBoolOnlyWrapsComparisons is the
// vector-scalar sibling of the synthetic-vector-binary test above,
// killing the INVERT_LOGICAL mutant at binary.go:856:22. The same
// reachability argument applies: line 842's early return plus PromQL's
// grammar invariant means `isComparison(op) && returnBool` only ever
// observes (true,true) or (false,false) via real parsing, so the kill
// calls lowerVectorScalar directly with the otherwise-unreachable
// (arithmetic op, returnBool=true) combination.
func TestLowerVectorScalar_ReturnBoolOnlyWrapsComparisons(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	plan, err := lowerVectorScalar(mustParse(t, `up`), s, chplan.OpAdd, 5, true, true, lowerCtx{})
	if err != nil {
		t.Fatalf("lowerVectorScalar: %v", err)
	}
	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("lowerVectorScalar result = %T, want *chplan.Project", plan)
	}
	if len(proj.Projections) == 0 {
		t.Fatalf("lowerVectorScalar result Project has no projections")
	}
	valueExpr := proj.Projections[len(proj.Projections)-1].Expr
	if _, wrapped := valueExpr.(*chplan.FuncCall); wrapped {
		t.Fatalf("Value = %#v, want *chplan.Binary (mutant `&&`→`||` at binary.go:856:22 wraps arithmetic ops in toFloat64 too when returnBool=true)", valueExpr)
	}
	if _, ok := valueExpr.(*chplan.Binary); !ok {
		t.Fatalf("Value = %T, want *chplan.Binary", valueExpr)
	}
}

// TestLowerVectorSetOp_MixedOr_StepAlignedTracksStep kills the
// CONDITIONALS_BOUNDARY (`>`→`>=`) and CONDITIONALS_NEGATION (`>`→`<=`)
// mutants at binary.go:593:32:
//
//	StepAligned: ctx.step > 0,
//
// This line sits inside lowerVectorSetOp's `case leftMixed ||
// rightMixed` arm (binary.go:578), reached only when at least one
// operand of the `or` is ITSELF already a Mixed VectorSetOp (cerberus
// issue #2555) — a bare `<histogram> or <float>` lands in the sibling
// `leftHistogram != rightHistogram` case instead. The query below
// nests a mixed-or inside another `or` so the left operand is already
// Mixed when the outer lowerVectorSetOp call runs.
//
// At step=0 (instant mode) the original is false; both mutants (`>=`
// making 0>=0 true, `<=` making 0<=0 true) would set it true. At
// step>0 (range mode) the original is true; the negation mutant (`<=`
// making step<=0 false) would set it false. The instant-mode assertion
// alone already kills both mutants; the range-mode assertion pins the
// positive direction too.
func TestLowerVectorSetOp_MixedOr_StepAlignedTracksStep(t *testing.T) {
	t.Parallel()

	query := `(latency_exp_hist or histogram_quantile(0.5, latency_exp_hist)) or up`
	expr := mustParseExperimental(t, query)
	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	instantPlan, err := LowerAt(context.Background(), expr, s, start, start)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	instantSetOp, ok := instantPlan.(*chplan.VectorSetOp)
	if !ok {
		t.Fatalf("lower(%q) instant plan = %T, want *chplan.VectorSetOp", query, instantPlan)
	}
	if instantSetOp.StepAligned {
		t.Fatalf("instant-mode (step=0) StepAligned = true, want false (mutants at binary.go:593:32 would set true)")
	}

	end := start.Add(5 * time.Minute)
	rangePlan, err := LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): %v", query, err)
	}
	rangeSetOp, ok := rangePlan.(*chplan.VectorSetOp)
	if !ok {
		t.Fatalf("lower(%q) range plan = %T, want *chplan.VectorSetOp", query, rangePlan)
	}
	if !rangeSetOp.StepAligned {
		t.Fatalf("range-mode (step=1m) StepAligned = false, want true (CONDITIONALS_NEGATION mutant `>`→`<=` at binary.go:593:32 would set false)")
	}
}

// ---------------------------------------------------------------------
// lower_strategy.go
// ---------------------------------------------------------------------

// TestNativeTemporalityFilter_ProjectionsCapacityIsTight kills the two
// adjacent mutants at lower_strategy.go:349:81 inside
// nativeTemporalityFilter's slice-capacity hint:
//
//	projectCopy.Projections = make([]chplan.Projection, 0, len(project.Projections)-1)
//
// ARITHMETIC_BASE (`-`→`+`) and INVERT_NEGATIVES (`-1`→`+1`) both
// enlarge the capacity by 2 relative to the tight fit (removing exactly
// one column — the temporality column itself — always leaves
// len(Projections)-1 survivors). append silently uses the extra
// headroom, so the resulting slice's CONTENTS are identical under both
// branches; only cap() differs, mirroring
// TestLowerLabelJoin_SrcsSliceCapacityIsTight's approach in
// gremlins_kill_test.go.
func TestNativeTemporalityFilter_ProjectionsCapacityIsTight(t *testing.T) {
	t.Parallel()

	input := &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "a"}, Alias: "a"},
			{Expr: &chplan.ColumnRef{Name: "temporality"}, Alias: "temporality"},
			{Expr: &chplan.ColumnRef{Name: "b"}, Alias: "b"},
		},
	}
	got := nativeTemporalityFilter(input, "temporality")
	proj, ok := got.(*chplan.Project)
	if !ok {
		t.Fatalf("nativeTemporalityFilter result = %T, want *chplan.Project", got)
	}

	const wantLen = 2 // "a", "b" survive; "temporality" is dropped.
	if len(proj.Projections) != wantLen {
		t.Fatalf("len(Projections) = %d, want %d", len(proj.Projections), wantLen)
	}
	// Original: cap == len(project.Projections)-1 == 3-1 == 2.
	// Both mutants: cap == 3+1 == 4.
	if got := cap(proj.Projections); got != wantLen {
		t.Fatalf("cap(Projections) = %d, want %d (mutants `-`→`+` and `-1`→`+1` at lower_strategy.go:349:81 would yield cap=4)", got, wantLen)
	}
}

// TestNativePredictLinearHorizonEligible_GuardIsDisjunctive kills the
// INVERT_LOGICAL mutant at lower_strategy.go:581:30:
//
//	if len(rw.ScalarExprs) != 0 || len(rw.Scalars) != 1 {
//	    return false
//	}
//
// Each case below satisfies exactly one disjunct while leaving the
// other false, so `||` (short-circuit: reject) and `&&` (mutant: only
// reject when BOTH conditions hold) disagree.
func TestNativePredictLinearHorizonEligible_GuardIsDisjunctive(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rw   *chplan.RangeWindow
		want bool
	}{
		{
			name: "ScalarExprs set disqualifies even with exactly one Scalars entry",
			rw:   &chplan.RangeWindow{ScalarExprs: []chplan.Expr{&chplan.LitFloat{V: 1}}, Scalars: []float64{5}},
			want: false,
		},
		{
			name: "two Scalars entries disqualify even with no ScalarExprs",
			rw:   &chplan.RangeWindow{Scalars: []float64{5, 6}},
			want: false,
		},
		{
			name: "eligible: no ScalarExprs, exactly one non-negative whole Scalars entry",
			rw:   &chplan.RangeWindow{Scalars: []float64{5}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := nativePredictLinearHorizonEligible(tc.rw); got != tc.want {
				t.Fatalf("nativePredictLinearHorizonEligible(ScalarExprs=%d, Scalars=%v) = %v, want %v (mutant `||`→`&&` at lower_strategy.go:581:30 would flip the first two cases)",
					len(tc.rw.ScalarExprs), tc.rw.Scalars, got, tc.want)
			}
		})
	}
}

// TestNativePredictLinearHorizonEligible_ZeroHorizonIsEligible kills the
// CONDITIONALS_BOUNDARY mutant at lower_strategy.go:585:11:
//
//	return t >= 0 && t == math.Trunc(t)
//
// t=0 sits exactly on the boundary: `>= 0` is true (0 is a valid
// non-negative horizon — `predict_linear(m[5m], 0)` is legal PromQL),
// but a `>=`→`>` mutant would reject it.
func TestNativePredictLinearHorizonEligible_ZeroHorizonIsEligible(t *testing.T) {
	t.Parallel()

	rw := &chplan.RangeWindow{Scalars: []float64{0}}
	if !nativePredictLinearHorizonEligible(rw) {
		t.Fatalf("nativePredictLinearHorizonEligible(t=0) = false, want true (mutant `>=`→`>` at lower_strategy.go:585:11 would reject t=0)")
	}
}

// ---------------------------------------------------------------------
// histogram_native_mixed_or_math_fn.go
// ---------------------------------------------------------------------

// TestLowerClampOverMixedExpHistogramSetOp_ClampMinMaxPickCorrectFn kills
// the CONDITIONALS_NEGATION mutant at
// histogram_native_mixed_or_math_fn.go:270:21:
//
//	fnName := chplan.FnLeast
//	if call.Func.Name == "clamp_min" {
//	    fnName = chplan.FnGreatest
//	}
//
// clamp_min must pick FnGreatest; a `==`→`!=` mutant would leave fnName
// at its FnLeast default for clamp_min (and would wrongly flip it to
// FnGreatest for clamp_max, covered by the second assertion below).
func TestLowerClampOverMixedExpHistogramSetOp_ClampMinMaxPickCorrectFn(t *testing.T) {
	t.Parallel()

	minValue := clampFamilyNewValue(t, `clamp_min(latency_exp_hist or histogram_quantile(0.5, latency_exp_hist), 5)`)
	fc, ok := minValue.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnGreatest {
		t.Fatalf("clamp_min newValue = %#v, want FuncCall{Fn: FnGreatest} (mutant `==`→`!=` at histogram_native_mixed_or_math_fn.go:270:21 would pick FnLeast)", minValue)
	}

	maxValue := clampFamilyNewValue(t, `clamp_max(latency_exp_hist or histogram_quantile(0.5, latency_exp_hist), 5)`)
	fc, ok = maxValue.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnLeast {
		t.Fatalf("clamp_max newValue = %#v, want FuncCall{Fn: FnLeast} (mutant `==`→`!=` at histogram_native_mixed_or_math_fn.go:270:21 would pick FnGreatest)", maxValue)
	}
}

// TestLowerClampOverMixedExpHistogramSetOp_MixedBoundsTakeRuntimePath
// kills the INVERT_LOGICAL mutant at
// histogram_native_mixed_or_math_fn.go:293:12:
//
//	if okMin && okMax {
//
// With one literal bound (minB=5) and one computed bound
// (scalar(sum(up))), okMin=true and okMax=false. The original AND
// routes to the runtime FnIf-guarded path (checked below via the
// FuncCall{Fn: FnIf} shape). A `&&`→`||` mutant would instead enter the
// literal-only branch with maxB defaulting to its zero value (okMax was
// false), and 0 < 5 trips the degenerate empty-clamp fold.
func TestLowerClampOverMixedExpHistogramSetOp_MixedBoundsTakeRuntimePath(t *testing.T) {
	t.Parallel()

	newValue := clampFamilyNewValue(t, `clamp(latency_exp_hist or histogram_quantile(0.5, latency_exp_hist), 5, scalar(sum(up)))`)
	fc, ok := newValue.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnIf {
		t.Fatalf("mixed literal/computed clamp newValue = %#v, want FuncCall{Fn: FnIf} (runtime-bounds path; mutant `&&`→`||` at histogram_native_mixed_or_math_fn.go:293:12 would take the literal degenerate-fold path instead)", newValue)
	}
}

// TestLowerClampOverMixedExpHistogramSetOp_EqualLiteralBoundsNonDegenerate
// kills both mutants at histogram_native_mixed_or_math_fn.go:298:12:
//
//	if maxB < minB {
//
// minB == maxB == 5: equality is NOT less-than, so the original takes
// the non-degenerate literal path (FuncCall{Fn: FnGreatest}).
// CONDITIONALS_BOUNDARY (`<`→`<=`) and CONDITIONALS_NEGATION (`<`→`>=`)
// both evaluate 5<=5 / 5>=5 as true, wrongly taking the degenerate
// empty-clamp fold (a bare ColumnRef, no FuncCall).
func TestLowerClampOverMixedExpHistogramSetOp_EqualLiteralBoundsNonDegenerate(t *testing.T) {
	t.Parallel()

	newValue := clampFamilyNewValue(t, `clamp(latency_exp_hist or histogram_quantile(0.5, latency_exp_hist), 5, 5)`)
	fc, ok := newValue.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnGreatest {
		t.Fatalf("equal-bounds clamp newValue = %#v, want FuncCall{Fn: FnGreatest} (mutants `<`→`<=` and `<`→`>=` at histogram_native_mixed_or_math_fn.go:298:12 would take the degenerate empty-clamp fold)", newValue)
	}
}

// TestLowerClampOverMixedExpHistogramSetOp_DegenerateLiteralBoundsAreEmpty
// complements the equal-bounds test above with the genuinely degenerate
// direction: minB=10 > maxB=5. Prom's clamp() short-circuits to an empty
// vector rather than clamping to minB, so newValue is the bare
// passed-through Value column, not a FuncCall.
func TestLowerClampOverMixedExpHistogramSetOp_DegenerateLiteralBoundsAreEmpty(t *testing.T) {
	t.Parallel()

	newValue := clampFamilyNewValue(t, `clamp(latency_exp_hist or histogram_quantile(0.5, latency_exp_hist), 10, 5)`)
	if _, ok := newValue.(*chplan.FuncCall); ok {
		t.Fatalf("degenerate clamp newValue = %#v, want a bare *chplan.ColumnRef (empty-clamp fold), not a FuncCall", newValue)
	}
	ref, ok := newValue.(*chplan.ColumnRef)
	if !ok || ref.Name != schema.DefaultOTelMetrics().ValueColumn {
		t.Fatalf("degenerate clamp newValue = %#v, want *chplan.ColumnRef{Name: %q}", newValue, schema.DefaultOTelMetrics().ValueColumn)
	}
}

// ---------------------------------------------------------------------
// histogram_native_mixed_or_aggregate_count_values.go
// ---------------------------------------------------------------------

// TestCountValuesOverMixedExpHistogramSetOp_RecognizesOnlyCountValues
// kills the CONDITIONALS_NEGATION mutant at
// histogram_native_mixed_or_aggregate_count_values.go:76:19:
//
//	if !ok || agg.Op != parser.COUNT_VALUES {
//	    return nil, nil, false
//	}
//
// A `!=`→`==` mutant inverts the recognizer entirely: it would reject
// genuine count_values() calls and accept every OTHER aggregate wrapping
// a mixed `or` (here, sum()).
func TestCountValuesOverMixedExpHistogramSetOp_RecognizesOnlyCountValues(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	mixedOrQuery := `latency_exp_hist or histogram_quantile(0.5, latency_exp_hist)`

	cvExpr := mustParseExperimental(t, `count_values("v", `+mixedOrQuery+`)`)
	if _, _, ok := countValuesOverMixedExpHistogramSetOp(cvExpr, s, lowerCtx{}); !ok {
		t.Fatalf("count_values(...) over mixed-or not recognized (mutant `!=`→`==` at histogram_native_mixed_or_aggregate_count_values.go:76:19 would reject it)")
	}

	sumExpr := mustParseExperimental(t, `sum(`+mixedOrQuery+`)`)
	if _, _, ok := countValuesOverMixedExpHistogramSetOp(sumExpr, s, lowerCtx{}); ok {
		t.Fatalf("sum(...) over mixed-or wrongly recognized as count_values (mutant `!=`→`==` at histogram_native_mixed_or_aggregate_count_values.go:76:19 would accept any non-count_values aggregate)")
	}
}

// ---------------------------------------------------------------------
// histogram_native_mixed_or_aggregate_presence.go
// ---------------------------------------------------------------------

// TestCountOrGroupOverMixedExpHistogramSetOp_RecognizesCountAndGroup
// kills the three CONDITIONALS_NEGATION mutants on
// histogram_native_mixed_or_aggregate_presence.go:66:
//
//	if !ok || (agg.Op != parser.COUNT && agg.Op != parser.GROUP) || agg.Param != nil {
//	    return nil, nil, false
//	}
//
// col 20 (`agg.Op != parser.COUNT` → `==`): with Op=COUNT, the mutated
// first conjunct becomes true, and (true && (COUNT != GROUP = true)) =
// true — the whole OR chain trips and count(...) is wrongly rejected.
// Killed by the count(...) assertion.
//
// col 46 (`agg.Op != parser.GROUP` → `==`): symmetric — with Op=GROUP,
// ((GROUP != COUNT = true) && true) = true trips the OR chain and
// group(...) is wrongly rejected. Killed by the group(...) assertion.
//
// col 76 (`agg.Param != nil` → `==`): with Param genuinely nil (both
// count(...) and group(...) never carry a Param), the mutated third
// disjunct becomes true unconditionally, tripping the OR chain and
// rejecting BOTH normal calls. Killed by either assertion.
func TestCountOrGroupOverMixedExpHistogramSetOp_RecognizesCountAndGroup(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	mixedOrQuery := `latency_exp_hist or histogram_quantile(0.5, latency_exp_hist)`

	countExpr := mustParseExperimental(t, `count(`+mixedOrQuery+`)`)
	if _, _, ok := countOrGroupOverMixedExpHistogramSetOp(countExpr, s, lowerCtx{}); !ok {
		t.Fatalf("count(...) over mixed-or not recognized (mutants at histogram_native_mixed_or_aggregate_presence.go:66:20 / :66:76 would reject it)")
	}

	groupExpr := mustParseExperimental(t, `group(`+mixedOrQuery+`)`)
	if _, _, ok := countOrGroupOverMixedExpHistogramSetOp(groupExpr, s, lowerCtx{}); !ok {
		t.Fatalf("group(...) over mixed-or not recognized (mutants at histogram_native_mixed_or_aggregate_presence.go:66:46 / :66:76 would reject it)")
	}

	// Negative control: an aggregate op that is neither COUNT nor GROUP
	// must still be rejected.
	sumExpr := mustParseExperimental(t, `sum(`+mixedOrQuery+`)`)
	if _, _, ok := countOrGroupOverMixedExpHistogramSetOp(sumExpr, s, lowerCtx{}); ok {
		t.Fatalf("sum(...) over mixed-or wrongly recognized by the count/group recognizer")
	}
}

// ---------------------------------------------------------------------
// histogram_native_mixed_or_datefn.go
// ---------------------------------------------------------------------

// TestDateFnOverMixedExpHistogramSetOp_ArgCountAndTimestampExclusion
// kills the three mutants on histogram_native_mixed_or_datefn.go:58:
//
//	if len(c.Args) != 1 || c.Func.Name == "timestamp" {
//	    return nil, false
//	}
//
// col 17 (`!=`→`==`): with exactly 1 arg (the normal, only-valid shape
// for these date fns), the mutated first disjunct becomes true and
// wrongly rejects. Killed by the year(...) assertion.
//
// col 37 (`==`→`!=`): with Func.Name="year" (not "timestamp"), the
// mutated second disjunct ("year" != "timestamp") becomes true and
// wrongly rejects EVERY normal date-fn call. Also killed by the
// year(...) assertion.
//
// col 22 (`||`→`&&`): with Func.Name="timestamp" and exactly 1 arg, the
// original OR trips on the second disjunct alone and correctly rejects
// (timestamp is explicitly excluded by name). The mutated AND requires
// BOTH disjuncts, and the first (len!=1) is false here, so the mutant
// wrongly accepts "timestamp". Killed by the timestamp(...) assertion.
//
// timestamp() isn't reachable through this recognizer via the normal
// lowerCall dispatch (which excludes it by name before ever calling
// here per this file's own doc comment), so the kill constructs the
// *parser.Call by hand and calls the recognizer directly — same pattern
// gremlins_kill_test.go's label_join tests use for otherwise-unreachable
// argument shapes.
func TestDateFnOverMixedExpHistogramSetOp_ArgCountAndTimestampExclusion(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	mixedOr := mustParseExperimental(t, `latency_exp_hist or histogram_quantile(0.5, latency_exp_hist)`)
	binExpr, ok := mixedOr.(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("parsed mixed-or query = %T, want *parser.BinaryExpr", mixedOr)
	}

	yearCall := &parser.Call{Func: parser.MustGetFunction("year"), Args: parser.Expressions{binExpr}}
	if _, ok := dateFnOverMixedExpHistogramSetOp(yearCall, s, lowerCtx{}); !ok {
		t.Fatalf("year(<mixed or>) not recognized (mutants `!=`→`==` at histogram_native_mixed_or_datefn.go:58:17 or `==`→`!=` at :58:37 would reject it)")
	}

	tsCall := &parser.Call{Func: parser.MustGetFunction("timestamp"), Args: parser.Expressions{binExpr}}
	if _, ok := dateFnOverMixedExpHistogramSetOp(tsCall, s, lowerCtx{}); ok {
		t.Fatalf("timestamp(<mixed or>) wrongly recognized (mutant `||`→`&&` at histogram_native_mixed_or_datefn.go:58:22, or `==`→`!=` at :58:37, would accept it)")
	}
}

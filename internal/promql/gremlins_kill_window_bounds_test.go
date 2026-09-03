// Tests in this file kill LIVED gremlins mutants reported on the
// phase4-promql-b / -d / -f / -i / -other / -quantile legs (cerberus issue
// #2949): the exp-histogram window recognizers' zero-range guards and the
// value-producing call's arity guard. See gremlins_kill_test.go for the
// shared file-header convention this file follows.
package promql

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// zeroRangeExpHistogramCall builds `<fn>(latency_exp_hist[0])` by hand.
//
// The PromQL grammar refuses a literal `[0s]` outright ("duration must be
// greater than 0"), so a zero-range matrix selector cannot be reached through
// the parser and the guard is defensive. It is still each recognizer's
// contract — a window of no width has no samples to reduce — and building the
// selector directly is the convention this package already uses for the same
// class (see TestCountPresentOverExpHistogram_ZeroRangeRejected and
// TestRangeFnOverExpHistogram_ZeroRangeRejected in
// histogram_native_range_family_gremlins_test.go).
func zeroRangeExpHistogramCall(t *testing.T, fn string) *parser.Call {
	t.Helper()
	return &parser.Call{
		Func: parser.MustGetFunction(fn),
		Args: parser.Expressions{
			&parser.MatrixSelector{Range: 0, VectorSelector: mustParseVectorSelector(t, "latency_exp_hist")},
		},
	}
}

// TestLastFirstOverExpHistogram_ZeroRangeRejected kills the
// CONDITIONALS_BOUNDARY mutant at
//
//	histogram_native_last_first_over_time.go:lastFirstOverExpHistogram:`ms.Range <= 0`
//
// rewritten to `ms.Range < 0` inside [lastFirstOverExpHistogram]. A range is
// never negative, so `< 0` can only ever be false and the zero-width window
// is admitted.
func TestLastFirstOverExpHistogram_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	if _, _, _, ok := lastFirstOverExpHistogram(zeroRangeExpHistogramCall(t, "last_over_time"), s, lowerCtx{}); ok {
		t.Fatal("lastFirstOverExpHistogram accepted a zero-range matrix selector; a window of no " +
			"width has no samples to reduce (mutant `<=`->`<` at " +
			"histogram_native_last_first_over_time.go:lastFirstOverExpHistogram:`ms.Range <= 0` admits it)")
	}
}

// TestOverTimeOverExpHistogram_ZeroRangeRejected kills the
// CONDITIONALS_BOUNDARY mutant on
//
//	histogram_native_over_time.go:overTimeOverExpHistogram:`ms.Range <= 0`
//
// the same rewrite to `ms.Range < 0` inside [overTimeOverExpHistogram].
func TestOverTimeOverExpHistogram_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	if _, ok := overTimeOverExpHistogram(zeroRangeExpHistogramCall(t, "sum_over_time"), s, lowerCtx{}); ok {
		t.Fatal("overTimeOverExpHistogram accepted a zero-range matrix selector; a window of no " +
			"width has no samples to reduce (mutant `<=`->`<` at " +
			"histogram_native_over_time.go:overTimeOverExpHistogram:`ms.Range <= 0` admits it)")
	}
}

// TestResetsOrChangesOverExpHistogram_ZeroRangeRejected kills the
// CONDITIONALS_BOUNDARY mutant on
//
//	histogram_native_resets.go:resetsOrChangesOverExpHistogram:`ms.Range <= 0`
//
// the same rewrite inside [resetsOrChangesOverExpHistogram].
func TestResetsOrChangesOverExpHistogram_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	if _, ok := resetsOrChangesOverExpHistogram(zeroRangeExpHistogramCall(t, "resets"), s, lowerCtx{}); ok {
		t.Fatal("resetsOrChangesOverExpHistogram accepted a zero-range matrix selector; a window " +
			"of no width has no samples to reduce (mutant `<=`->`<` at " +
			"histogram_native_resets.go:resetsOrChangesOverExpHistogram:`ms.Range <= 0` admits it)")
	}
}

// TestTsOfFirstLastOverExpHistogram_ZeroRangeRejected kills the
// CONDITIONALS_BOUNDARY mutant at
//
//	histogram_native_ts_of_first_last_over_time.go:tsOfFirstLastOverExpHistogram:`ms.Range <= 0`
//
// the same rewrite inside [tsOfFirstLastOverExpHistogram].
func TestTsOfFirstLastOverExpHistogram_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	call := zeroRangeExpHistogramCall(t, tsOfLastOverTimeExpHistFn)
	if _, _, _, ok := tsOfFirstLastOverExpHistogram(call, s, lowerCtx{}); ok {
		t.Fatal("tsOfFirstLastOverExpHistogram accepted a zero-range matrix selector; a window of " +
			"no width has no samples to reduce (mutant `<=`->`<` at " +
			"histogram_native_ts_of_first_last_over_time.go:tsOfFirstLastOverExpHistogram:`ms.Range <= 0` admits it)")
	}
}

// TestHistogramValuedProducerCall_InfoTakesAtMostTwoArguments kills the
// INVERT_LOGICAL mutant on the `||` of
//
//	histogram_native_value_producing_call.go:`len(call.Args) < 1 || len(call.Args) > 2`
//
// inside [histogramValuedProducerCall].
//
// The guard is `info`'s arity contract stated as two independent bounds: too
// few arguments and too many are each disqualifying on their own. Under `&&`
// the rejection needs BOTH at once, which is unsatisfiable — no argument
// count is simultaneously below one and above two — so the guard never fires
// and a three-argument `info` over a histogram-valued base is reported as a
// histogram-valued producer call.
func TestHistogramValuedProducerCall_InfoTakesAtMostTwoArguments(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	base := mustParseVectorSelector(t, "latency_exp_hist")
	dataLabels := mustParse(t, `{__name__="target_info"}`)

	twoArg := &parser.Call{
		Func: parser.MustGetFunction("info"),
		Args: parser.Expressions{base, dataLabels},
	}
	if _, ok := histogramValuedProducerCall(twoArg, s, lowerCtx{}); !ok {
		t.Fatal("positive control: a two-argument `info` over a histogram-valued base is not " +
			"recognised; the negative assertion below would then hold for the wrong reason")
	}

	threeArg := &parser.Call{
		Func: parser.MustGetFunction("info"),
		Args: parser.Expressions{base, dataLabels, dataLabels},
	}
	if _, ok := histogramValuedProducerCall(threeArg, s, lowerCtx{}); ok {
		t.Fatal("histogramValuedProducerCall accepted a three-argument `info`; the arity bounds " +
			"are independent and either one disqualifies (mutant `||`->`&&` at " +
			"histogram_native_value_producing_call.go:`len(call.Args) < 1 || len(call.Args) > 2` " +
			"makes the pair unsatisfiable)")
	}
}

// NOT KILLABLE — documented, not defended by a test.
//
// The remaining survivors on cerberus issue #2949's phase4-promql-* legs fall
// into five equivalence classes. Each mutant is named by the construct it
// rewrites — a line number cannot be machine-verified and rots on every
// insertion above it (#2953) — scoped to its enclosing function wherever the
// construct repeats within the file.
//
// 1. CAPACITY HINTS — ARITHMETIC_BASE inside a `make(T, 0, <cap>)` argument.
//
//	schema_lookup.go:`make([]chplan.Expr, 0, len(pairs)*2)`
//	resource_attributes.go:`make(map[string]struct{}, len(dedicatedResourceKeys)*2)`
//
// This class USED to carry six more citations, and the reason it gave for all
// eight — "a slice's capacity is not observable behaviour: `append` grows past
// a short one, and every element, ordering and identity is unchanged" — was
// true in its premise and wrong in its conclusion. Observability is not
// confined to the emitted SQL or to the elements: `cap` is readable on any
// slice a test can reach, and six of the eight sized a slice that becomes an
// exported field of a value its builder RETURNS. Those six are killed in
// capacity_hint_mutation_test.go, one test each, and their citations moved
// there with them (cerberus issue #2984). docs/test-strategy.md's "When a
// capacity mutant is equivalent" states the rule the eight were adjudicated
// against without.
//
// The two that remain fail that rule on DIFFERENT halves of it, which is why
// neither inherits the other's argument:
//
//   - `make(map[string]struct{}, len(dedicatedResourceKeys)*2)` sizes a MAP.
//     Go publishes no accessor for a map's capacity — `len` counts the entries
//     the loop inserted, `cap` is not defined for maps at all, and the runtime
//     hint reaches no exported or unexported surface. No test in any package
//     can read the rewritten arithmetic back, so the escape question never
//     arises: this one is unobservable whatever the map does next.
//
//   - `make([]chplan.Expr, 0, len(pairs)*2)` DOES escape — the slice becomes
//     the `map(...)` FuncCall's exported `Args` inside the expression
//     augmentAttributesForTopLevelExpr returns — and it is still unkillable,
//     because the finished capacity does not distinguish the mutant gremlins
//     writes. That mutant is `*` -> `/` (ARITHMETIC_BASE rewrites each
//     arithmetic token to exactly one other, so this hint carries one mutant,
//     not four). `pairs` is promqlTopLevelKeys' answer over
//     resource_attributes.go:`var dedicatedResourceKeys = []dedicatedResourceKey{`,
//     which holds one entry, and the function returns before the `make` when
//     that answer is empty — so `pairs` is 1 at every reachable call. The
//     unmutated hint is 2 and the mutated one is 0, and the single
//     `append(args, key, value)` that follows grows a zero-capacity slice
//     straight to capacity 2: `append` asks for length 2, which exceeds twice
//     the old capacity, so the runtime allocates exactly the two elements
//     asked for. Both builds finish at len 2 / cap 2, so a `cap` assertion
//     reads the SAME number either way.
//
//     This equivalence is a property of the registry's current size, exactly
//     as class 3's is: at two dedicated keys the true hint is 4 and the mutant
//     hint is 1, whose growth finishes at capacity 4 as well — but at three
//     the mutant finishes at 8 against a true 6, and the mutant becomes
//     killable and should be killed rather than re-adjudicated.
//
// 2. A BOUND THE ENCLOSING CODE HAS ALREADY NORMALISED.
//
//	histogram_native_range_fn.go:subqueryHasEvalAnchor:`step < 0`
//	histogram_native_range_fn.go:lowerExpHistogramRangeFnOverSubquery:`step < 0`
//	histogram_native_subquery_select.go:lowerSelectFnOverExpHistogramSubquery:`step < 0`
//	scalar.go:`if lhs < 0 {`
//
// `<` -> `<=` differs only at zero, and zero cannot reach these guards. Each
// `step` is assigned `defaultSubqueryStep` (a positive constant) by an
// immediately preceding `if step == 0`. The `lhs` guard sits inside
// `if rhs == 0 { if lhs == 0 { return NaN } ... }`, so `lhs` is non-zero — and
// a negative zero is caught by that `lhs == 0` too, since IEEE equality holds
// for it.
//
// 3. A LOOP OVER A REGISTRY THAT HOLDS ONE ENTRY.
//
//	schema_lookup.go:promqlTopLevelKeys:`if col == "" {`
//	resource_attributes.go:excludedResourceKeys:`if d.column(s) == "" {`
//
// Each citation names the guard whose body is the mutated statement, because
// that statement is a bare `continue` and no substring of it singles one out.
// Both loops range over
// resource_attributes.go:`var dedicatedResourceKeys = []dedicatedResourceKey{`,
// which holds exactly one element (service.name). Over a one-element range
// `continue` and `break` both leave the loop after the same single iteration,
// so the rewrite cannot change what either function returns. This equivalence
// is a property of the registry's current size, and it dissolves the moment a
// second dedicated key is added — at which point both mutants become killable
// and should be killed rather than re-adjudicated.
//
// 4. A REWRITE THE LANGUAGE MAKES A NO-OP.
//
//	histogram_native_mixed_or_vector_plain_comparison.go:`len(b.VectorMatching.Include) > 0`
//	histogram_native_mixed_or_vector_comparison.go:`len(b.VectorMatching.Include) > 0`
//
// Both guard `include = append([]string(nil), b.VectorMatching.Include...)`.
// `>= 0` makes the guard always true, so the append runs even for an empty
// Include — and appending ZERO elements to a nil slice returns that same nil
// slice, so `include` is nil either way. Confirmed by evaluating
// `append([]string(nil), src...) == nil` for both a nil and an empty-non-nil
// src.
//
// 5. AN INTERNAL-INVARIANT ERROR PATH.
//
//	histogram_native_range_fn.go:`!matched || chplan.RowShapeOf(input) != chplan.HistogramRowShape`
//	histogram_native_subquery_select.go:`!matched || chplan.RowShapeOf(input) != chplan.HistogramRowShape`
//
// `||` -> `&&` narrows a check that reports "internal invariant violated".
// The two readings differ only when exactly one disjunct holds, and neither
// half is constructible: `lowerExpHistogramValuedShape` returns a nil node
// whenever `matched` is false, and `chplan.RowShapeOf(nil)` is the sample row
// shape, so `!matched` always arrives together with a non-histogram shape and
// both readings error. The complementary case — matched with a non-histogram
// shape — is the invariant the line exists to report, and producing it would
// require lowerExpHistogramValuedShape to break its own contract.
//
// 7. A `continue` WHOSE `break` BINDS TO A SWITCH, NOT TO THE LOOP.
//
//	duplicate_labelset_guard.go:`if mixed && mixedPayload[name] {`
//	duplicate_labelset_guard.go:`if keyOnStep`
//
// Each citation names the guard whose body holds the mutated statement,
// because that statement is a bare `continue` and no substring of it singles
// one out. Both sit in guardLabelRewriteCollision's per-projection walk,
// whose loop body is a single `switch` with nothing after it:
//
//	for _, proj := range rewritten.Projections {
//		switch name := chplan.ProjectionOutputName(proj); name {
//		...
//		}
//	}
//
// INVERT_LOOPCTRL rewrites each `continue` to `break`, and inside a `switch`
// a bare `break` leaves the switch rather than the loop. Because the switch
// IS the whole loop body, leaving it and continuing the loop reach the same
// place: the next iteration, with the remainder of the arm skipped either
// way. Both readings therefore visit every projection and build the identical
// groupBy / aliases / aggs triple.
//
// Confirmed by applying each rewrite by hand and running the package's two
// walk-order tests (TestGuardLabelRewriteCollision_MixedPayloadSkipContinuesLoop
// and TestGuardLabelRewriteCollision_KeyOnStepSkipContinuesLoop, above in
// gremlins_kill_test.go): both pass under both mutants, matching the
// phase4-promql-e leg, which reports both as LIVED. Those two tests pin the
// walk's OUTPUT, which is real behaviour worth holding; they do not pin the
// keyword, and no test can.
//
// The equivalence is a property of the switch being the last statement in
// the loop body. It dissolves the moment a statement is added after the
// switch, at which point both mutants become killable and should be killed
// rather than re-adjudicated.

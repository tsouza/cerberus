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
// histogram_native_last_first_over_time.go:62:21 — `ms.Range <= 0` ->
// `ms.Range < 0` inside [lastFirstOverExpHistogram]. A range is never
// negative, so `< 0` can only ever be false and the zero-width window is
// admitted.
func TestLastFirstOverExpHistogram_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	if _, _, _, ok := lastFirstOverExpHistogram(zeroRangeExpHistogramCall(t, "last_over_time"), s, lowerCtx{}); ok {
		t.Fatal("lastFirstOverExpHistogram accepted a zero-range matrix selector; a window of no " +
			"width has no samples to reduce (mutant `<=`->`<` at " +
			"histogram_native_last_first_over_time.go:62:21 admits it)")
	}
}

// TestOverTimeOverExpHistogram_ZeroRangeRejected kills the
// CONDITIONALS_BOUNDARY mutant at histogram_native_over_time.go:36:21 —
// the same `ms.Range <= 0` -> `ms.Range < 0` inside
// [overTimeOverExpHistogram].
func TestOverTimeOverExpHistogram_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	if _, ok := overTimeOverExpHistogram(zeroRangeExpHistogramCall(t, "sum_over_time"), s, lowerCtx{}); ok {
		t.Fatal("overTimeOverExpHistogram accepted a zero-range matrix selector; a window of no " +
			"width has no samples to reduce (mutant `<=`->`<` at " +
			"histogram_native_over_time.go:36:21 admits it)")
	}
}

// TestResetsOrChangesOverExpHistogram_ZeroRangeRejected kills the
// CONDITIONALS_BOUNDARY mutant at histogram_native_resets.go:133:21 — the
// same `ms.Range <= 0` -> `ms.Range < 0` inside
// [resetsOrChangesOverExpHistogram].
func TestResetsOrChangesOverExpHistogram_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	if _, ok := resetsOrChangesOverExpHistogram(zeroRangeExpHistogramCall(t, "resets"), s, lowerCtx{}); ok {
		t.Fatal("resetsOrChangesOverExpHistogram accepted a zero-range matrix selector; a window " +
			"of no width has no samples to reduce (mutant `<=`->`<` at " +
			"histogram_native_resets.go:133:21 admits it)")
	}
}

// TestTsOfFirstLastOverExpHistogram_ZeroRangeRejected kills the
// CONDITIONALS_BOUNDARY mutant at
// histogram_native_ts_of_first_last_over_time.go:89:21 — the same
// `ms.Range <= 0` -> `ms.Range < 0` inside [tsOfFirstLastOverExpHistogram].
func TestTsOfFirstLastOverExpHistogram_ZeroRangeRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	call := zeroRangeExpHistogramCall(t, tsOfLastOverTimeExpHistFn)
	if _, _, _, ok := tsOfFirstLastOverExpHistogram(call, s, lowerCtx{}); ok {
		t.Fatal("tsOfFirstLastOverExpHistogram accepted a zero-range matrix selector; a window of " +
			"no width has no samples to reduce (mutant `<=`->`<` at " +
			"histogram_native_ts_of_first_last_over_time.go:89:21 admits it)")
	}
}

// TestHistogramValuedProducerCall_InfoTakesAtMostTwoArguments kills the
// INVERT_LOGICAL mutant at histogram_native_value_producing_call.go:41:25 —
// the `||` of
//
//	if len(call.Args) < 1 || len(call.Args) > 2 {
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
			"histogram_native_value_producing_call.go:41:25 makes the pair unsatisfiable)")
	}
}

// NOT KILLABLE — documented, not defended by a test.
//
// The remaining survivors on cerberus issue #2949's phase4-promql-* legs fall
// into five equivalence classes. Each mutant is named by `file:line:col` AND
// by the construct it rewrites, because bare line numbers drift.
//
// 1. CAPACITY HINTS — ARITHMETIC_BASE inside a `make(T, 0, <cap>)` argument.
//
//	schema_lookup.go:133:43                  `len(pairs)*2`
//	resource_attributes.go:73:61             `len(dedicatedResourceKeys)*2`
//	histogram_quantile_native_window.go:299:65 and :299:83
//	                                         `len(keyAliases)+len(aggs)+len(extraAliases)+1`
//	histogram_quantile_native_window.go:380:55 `len(keyAliases)+len(scalars)+7`
//	histogram_native_resets.go:298:55        `len(keyAliases)+1`
//	histogram_native_scalar_binop.go:345:60 and :345:62
//	                                         `len(passthroughCols)+1+5`
//	histogram_quantile.go:1537:63            `len(passthrough)+2`
//	histogram_quantile.go:1874:47            `len(labels)*2`
//
// A slice's capacity is not observable behaviour: `append` grows past a short
// one, and every element, ordering and identity is unchanged. The one way a
// capacity rewrite CAN be observed is a negative argument, which makes `make`
// panic — so each site was checked for that rather than waved through:
//
//   - The `*2` and `/2` forms cannot go negative from a length.
//   - `len(passthroughCols)+1+5` (scalar_binop.go:345) reads a slice literal
//     built two lines above with exactly six elements, so the two rewrites
//     compute 10 and 2. Neither is negative.
//   - `len(passthrough)+2` (histogram_quantile.go:1537) sits in
//     classicBucketShaping.reshape's `sh.fold == nil` branch. The only
//     shaping constructed with a nil fold is histogram_quantile_range.go:280's
//     bare-selector shaping, and both of its reshape call sites
//     (histogram_quantile_range.go:348 and :395) pass a TWO-element
//     passthrough, so the rewrite computes 0. Instrumenting the branch across
//     the whole package suite observed `foldNil: true` only ever with
//     `passthrough: 2`; the one-element call site
//     (histogram_quantile.go:1301) never reaches the nil-fold branch.
//   - `len(keyAliases)+1` and the `+len(...)+N` forms are reached only with
//     the aggregation's own alias set, which the emitting code has already
//     populated.
//
// 2. A BOUND THE ENCLOSING CODE HAS ALREADY NORMALISED.
//
//	histogram_native_range_fn.go:249:10       `step < 0` in subqueryHasEvalAnchor
//	histogram_native_range_fn.go:269:10       `step < 0` in lowerExpHistogramRangeFnOverSubquery
//	histogram_native_subquery_select.go:170:10 `step < 0` in lowerSelectFnOverExpHistogramSubquery
//	scalar.go:98:11                            `lhs < 0` in the DIV-by-zero arm
//
// `<` -> `<=` differs only at zero, and zero cannot reach these lines. Each
// `step` is assigned `defaultSubqueryStep` (a positive constant) by an
// immediately preceding `if step == 0`. `scalar.go:98` sits inside
// `if rhs == 0 { if lhs == 0 { return NaN } ... }`, so `lhs` is non-zero — and
// a negative zero is caught by that `lhs == 0` too, since IEEE equality holds
// for it.
//
// 3. A GUARD WHOSE CONDITION IS INVARIANTLY TRUE.
//
//	histogram_quantile.go:1257:23  `shape.windowRange > 0` in lowerHistogramQuantileAgg
//	histogram_quantile.go:2207:23  the same guard in the exp-histogram sibling
//
// `> 0` -> `>= 0` widens a test that already always holds. histogramAggShape's
// windowRange is set at exactly two places in the shape's construction
// (histogram_quantile.go:1149 and :1191): `instantLookback`, which is
// qlcommon.InstantLookback = 5m, or `ms.Range`, which the PromQL grammar
// refuses to parse as anything but strictly positive ("duration must be
// greater than 0"). Instrumenting both lines across the package suite
// observed no zero. The surrounding comment describing a "shape.windowRange
// == 0" bare-selector case predates that construction and no longer describes
// a reachable state; cerberus issue #2961 tracks the stale comment and the
// guard it describes.
//
// 4. A LOOP OVER A REGISTRY THAT HOLDS ONE ENTRY.
//
//	schema_lookup.go:67:4         `continue` in promqlTopLevelKeys
//	resource_attributes.go:76:4   `continue` in excludedResourceKeys
//
// Both loops range over `dedicatedResourceKeys`, which is declared in
// resource_attributes.go:61 with exactly one element (service.name). Over a
// one-element range `continue` and `break` both leave the loop after the same
// single iteration, so the rewrite cannot change what either function
// returns. This equivalence is a property of the registry's current size, and
// it dissolves the moment a second dedicated key is added — at which point
// both mutants become killable and should be killed rather than re-adjudicated.
//
// 5. A REWRITE THE LANGUAGE MAKES A NO-OP.
//
//	histogram_native_mixed_or_vector_plain_comparison.go:99:36
//	                              `len(b.VectorMatching.Include) > 0`
//	histogram_native_mixed_or_vector_comparison.go:163:36
//	                              the same guard in the vector-vector sibling
//
// Both guard `include = append([]string(nil), b.VectorMatching.Include...)`.
// `>= 0` makes the guard always true, so the append runs even for an empty
// Include — and appending ZERO elements to a nil slice returns that same nil
// slice, so `include` is nil either way. Confirmed by evaluating
// `append([]string(nil), src...) == nil` for both a nil and an empty-non-nil
// src.
//
// 6. AN INTERNAL-INVARIANT ERROR PATH.
//
//	histogram_native_range_fn.go:284:14        `!matched || chplan.RowShapeOf(input) != chplan.HistogramRowShape`
//	histogram_native_subquery_select.go:185:14 the same guard in the select sibling
//
// `||` -> `&&` narrows a check that reports "internal invariant violated".
// The two readings differ only when exactly one disjunct holds, and neither
// half is constructible: `lowerExpHistogramValuedShape` returns a nil node
// whenever `matched` is false, and `chplan.RowShapeOf(nil)` is the sample row
// shape, so `!matched` always arrives together with a non-histogram shape and
// both readings error. The complementary case — matched with a non-histogram
// shape — is the invariant the line exists to report, and producing it would
// require lowerExpHistogramValuedShape to break its own contract.

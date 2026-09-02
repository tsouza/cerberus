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

package promql

import (
	"testing"
	"time"
)

// baseEligibleInput returns a classicHistogramWindowInput that
// nativeClassicHistogramEligible accepts: a `rate` window, a materialised
// (Step > 0, bounds pinned) grid, not step-aligned (not a subquery inner),
// a positive lookback, and every duration a whole number of seconds — see
// [nativeClassicHistogramEligible]'s own doc for why each clause is
// required. Callers mutate one field at a time to probe each individual
// guard clause.
func baseEligibleInput() classicHistogramWindowInput {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return classicHistogramWindowInput{
		shape: histogramAggShape{windowFn: rateWindowFn},
		win:   histogramWindow{lookback: 5 * time.Minute, offset: 0},
		ctx: lowerCtx{
			start: at,
			end:   at.Add(time.Hour),
			step:  30 * time.Second,
		},
	}
}

// TestNativeClassicHistogramEligible_HappyPath pins that a fully
// shape-eligible window (see [baseEligibleInput]) is accepted.
func TestNativeClassicHistogramEligible_HappyPath(t *testing.T) {
	t.Parallel()
	if !nativeClassicHistogramEligible(baseEligibleInput()) {
		t.Fatalf("nativeClassicHistogramEligible(base) = false, want true")
	}
}

// TestNativeClassicHistogramEligible_WindowFnMustBeRate pins that only
// `rate` is native-eligible — `increase`/`sum_over_time`/etc. stay on the
// fan-out fallback.
func TestNativeClassicHistogramEligible_WindowFnMustBeRate(t *testing.T) {
	t.Parallel()
	in := baseEligibleInput()
	in.shape.windowFn = "increase"
	if nativeClassicHistogramEligible(in) {
		t.Fatalf("nativeClassicHistogramEligible(windowFn=increase) = true, want false")
	}
}

// TestNativeClassicHistogramEligible_StepBoundary pins the `in.ctx.step <=
// 0 || in.ctx.start.IsZero() || in.ctx.end.IsZero()` rejection at its exact
// boundary: Step == 0 (instant mode, no materialised grid) must be
// rejected — the aggregate takes (start, end, step) as constant
// parameters, so there is nothing to hand it in instant mode. This single
// case distinguishes:
//   - CONDITIONALS_BOUNDARY on `step <= 0` (`<= 0` -> `< 0`): a mutant
//     would read Step == 0 as NOT `< 0`, letting it through.
//   - INVERT_LOGICAL on EITHER `||` in that same line (`&&` in place of
//     either): with start/end both non-zero, the `step <= 0` clause is the
//     ONLY one true, so replacing either `||` with `&&` swallows it and
//     the mutant no longer rejects Step == 0 either.
func TestNativeClassicHistogramEligible_StepBoundary(t *testing.T) {
	t.Parallel()
	in := baseEligibleInput()
	in.ctx.step = 0
	if nativeClassicHistogramEligible(in) {
		t.Fatalf("nativeClassicHistogramEligible(step=0) = true, want false (instant mode has no materialised grid)")
	}
}

// TestNativeClassicHistogramEligible_StartEndZero pins that a zero
// start/end bound (the OTHER two disjuncts on the same line as the Step
// boundary) also rejects, keeping the `||` chain honest at each of its
// three terms rather than only the first.
func TestNativeClassicHistogramEligible_StartEndZero(t *testing.T) {
	t.Parallel()

	t.Run("start_zero", func(t *testing.T) {
		t.Parallel()
		in := baseEligibleInput()
		in.ctx.start = time.Time{}
		if nativeClassicHistogramEligible(in) {
			t.Fatalf("nativeClassicHistogramEligible(start=zero) = true, want false")
		}
	})
	t.Run("end_zero", func(t *testing.T) {
		t.Parallel()
		in := baseEligibleInput()
		in.ctx.end = time.Time{}
		if nativeClassicHistogramEligible(in) {
			t.Fatalf("nativeClassicHistogramEligible(end=zero) = true, want false")
		}
	})
}

// TestNativeClassicHistogramEligible_StepAlignedRejects pins that a
// subquery inner's epoch-aligned grid (ctx.stepAligned) stays on the
// fan-out: chplan.RangeBucketGridNative carries no StepAlign field.
func TestNativeClassicHistogramEligible_StepAlignedRejects(t *testing.T) {
	t.Parallel()
	in := baseEligibleInput()
	in.ctx.stepAligned = true
	if nativeClassicHistogramEligible(in) {
		t.Fatalf("nativeClassicHistogramEligible(stepAligned=true) = true, want false")
	}
}

// TestNativeClassicHistogramEligible_LookbackBoundary pins the
// `in.win.lookback <= 0` rejection at its exact boundary — lookback == 0
// must be rejected (CONDITIONALS_BOUNDARY would read `<= 0` as `< 0`,
// letting a zero lookback slip through as eligible).
func TestNativeClassicHistogramEligible_LookbackBoundary(t *testing.T) {
	t.Parallel()
	in := baseEligibleInput()
	in.win.lookback = 0
	if nativeClassicHistogramEligible(in) {
		t.Fatalf("nativeClassicHistogramEligible(lookback=0) = true, want false")
	}
}

// TestNativeClassicHistogramEligible_WholeSecondsConjunction pins the
// final `wholeSeconds(step) && wholeSeconds(lookback) && wholeSeconds(offset)`
// return as a genuine three-way AND: a non-whole-second value on ANY ONE
// of the three durations, with the other two whole-second, must reject —
// covering both `&&` positions against an INVERT_LOGICAL flip to `||`. A
// sub-second window would otherwise silently truncate to 0 in the native
// aggregate's whole-second UInt32 parameters (see
// [nativeClassicHistogramEligible]'s own doc).
func TestNativeClassicHistogramEligible_WholeSecondsConjunction(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut  func(*classicHistogramWindowInput)
	}{
		{"step_sub_second", func(in *classicHistogramWindowInput) { in.ctx.step = 30*time.Second + 500*time.Millisecond }},
		{"lookback_sub_second", func(in *classicHistogramWindowInput) { in.win.lookback = 90*time.Second + 500*time.Millisecond }},
		{"offset_sub_second", func(in *classicHistogramWindowInput) { in.win.offset = 500 * time.Millisecond }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := baseEligibleInput()
			tc.mut(&in)
			if nativeClassicHistogramEligible(in) {
				t.Fatalf("nativeClassicHistogramEligible(%s) = true, want false (a sub-second duration must reject)", tc.name)
			}
		})
	}
}

// TestWholeSeconds pins the exact-multiple-of-a-second predicate the
// eligibility gate's final return composes three times over.
func TestWholeSeconds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		d    time.Duration
		want bool
	}{
		{0, true},
		{time.Second, true},
		{30 * time.Second, true},
		{5 * time.Minute, true},
		{500 * time.Millisecond, false},
		{30*time.Second + 500*time.Millisecond, false},
	}
	for _, tc := range cases {
		if got := wholeSeconds(tc.d); got != tc.want {
			t.Errorf("wholeSeconds(%v) = %v, want %v", tc.d, got, tc.want)
		}
	}
}

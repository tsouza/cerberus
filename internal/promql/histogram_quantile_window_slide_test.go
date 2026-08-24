package promql

import (
	"testing"
	"time"
)

// windowSlideEligibleBaseCase returns a classicHistogramWindowInput that
// clears every windowSlideShapeEligible guard — the control every negative
// case below flips exactly ONE field away from. If this base case itself is
// not shape-eligible, every negative test below is vacuous (it would pass
// whether or not its own guard actually fires), so
// TestWindowSlideShapeEligible_BaseCaseIsEligible pins it directly.
//
// All of the tests in this file exercise windowSlideShapeEligible directly,
// NOT windowSlideEligible — windowSlideEligible additionally gates on
// windowSlideDisabledPending2511 (currently true, see that constant's own
// doc for why), which would make every guard-clause assertion below
// vacuously true regardless of whether its own guard fires.
// TestWindowSlideEligible_DisabledPending2511 covers that outer gate
// separately.
func windowSlideEligibleBaseCase() classicHistogramWindowInput {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return classicHistogramWindowInput{
		shape: histogramAggShape{windowFn: sumOverTimeWindowFn},
		win: histogramWindow{
			lookback:   10 * time.Minute, // ratio = 10m/1m = 10, meets the threshold exactly
			offset:     0,
			minSamples: stalenessMinSamples,
		},
		ctx: lowerCtx{
			start: start,
			end:   start.Add(time.Hour),
			step:  time.Minute,
		},
	}
}

func TestWindowSlideShapeEligible_BaseCaseIsEligible(t *testing.T) {
	if !windowSlideShapeEligible(windowSlideEligibleBaseCase()) {
		t.Fatal("base case (every guard satisfied) is not eligible — every negative case below " +
			"is vacuous until this passes")
	}
}

// TestWindowSlideShapeEligible_Refusals covers one refusal per guard. Each case
// starts from windowSlideEligibleBaseCase() and flips exactly the ONE field
// the guard it names reads, so a passing test here is evidence THAT guard —
// not some other one — is what makes windowSlideEligible answer false.
func TestWindowSlideShapeEligible_Refusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(in *classicHistogramWindowInput)
	}{
		{
			name: "windowFn is not sum_over_time (rate has its own native ladder)",
			mutate: func(in *classicHistogramWindowInput) {
				in.shape.windowFn = rateWindowFn
			},
		},
		{
			name: "windowFn is not sum_over_time (increase)",
			mutate: func(in *classicHistogramWindowInput) {
				in.shape.windowFn = increaseWindowFn
			},
		},
		{
			name: "instant mode (Step <= 0)",
			mutate: func(in *classicHistogramWindowInput) {
				in.ctx.step = 0
			},
		},
		{
			name: "unpinned grid (Start zero)",
			mutate: func(in *classicHistogramWindowInput) {
				in.ctx.start = time.Time{}
			},
		},
		{
			name: "unpinned grid (End zero)",
			mutate: func(in *classicHistogramWindowInput) {
				in.ctx.end = time.Time{}
			},
		},
		{
			name: "subquery-inner epoch-aligned grid",
			mutate: func(in *classicHistogramWindowInput) {
				in.ctx.stepAligned = true
			},
		},
		{
			name: "zero lookback",
			mutate: func(in *classicHistogramWindowInput) {
				in.win.lookback = 0
			},
		},
		{
			name: "non-whole-millisecond step",
			mutate: func(in *classicHistogramWindowInput) {
				in.ctx.step = time.Millisecond + 500*time.Microsecond // 1.5ms, not whole
				in.win.lookback = 10 * in.ctx.step                    // keep the ratio itself at the threshold
			},
		},
		{
			name: "non-whole-millisecond lookback",
			mutate: func(in *classicHistogramWindowInput) {
				in.win.lookback = 10*time.Millisecond + 500*time.Microsecond // 10.5ms, not whole
			},
		},
		{
			name: "non-whole-millisecond offset",
			mutate: func(in *classicHistogramWindowInput) {
				in.win.offset = 500 * time.Microsecond // 0.5ms, not whole
			},
		},
		{
			name: "lookback past the ms-encoding headroom (windowSlideMaxLookbackMs)",
			mutate: func(in *classicHistogramWindowInput) {
				in.win.lookback = (windowSlideMaxLookbackMs + 1) * time.Millisecond
				in.ctx.step = in.win.lookback / windowSlideMinLookbackStepRatio // keep the ratio at the threshold
			},
		},
		{
			name: "Lookback/Step ratio below windowSlideMinLookbackStepRatio",
			mutate: func(in *classicHistogramWindowInput) {
				in.win.lookback = (windowSlideMinLookbackStepRatio - 1) * in.ctx.step
			},
		},
		// NOTE: windowSlideEligible also checks
		// `in.shape.minSamples() != stalenessMinSamples` (a defence against
		// a future histogramWindowMinSamples change desyncing chsql's
		// hard-coded windowSlideMinSamples — see that check's own doc).
		// There is deliberately no row for it here: shape.minSamples() is
		// derived ENTIRELY from shape.windowFn (histogramWindowMinSamples's
		// own switch), so under the CURRENT implementation it can only
		// diverge from stalenessMinSamples by also flipping windowFn away
		// from sum_over_time — which the "windowFn is not sum_over_time"
		// cases above already cover. A row that flips in.win.minSamples
		// instead (a DIFFERENT field the guard does not read) would pass
		// vacuously without exercising the guard at all.
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := windowSlideEligibleBaseCase()
			tc.mutate(&in)
			if windowSlideShapeEligible(in) {
				t.Errorf("windowSlideShapeEligible(%+v) = true, want false — this guard is not enforced", in)
			}
		})
	}
}

// TestWindowSlideShapeEligible_RatioBoundary pins the exact threshold RELATIVE
// TO ITS OWN CURRENT VALUE: a ratio one below windowSlideMinLookbackStepRatio
// refuses, the ratio AT the threshold accepts. This catches the threshold
// moving in EITHER direction relative to itself — but NOT every constant
// change: at windowSlideMinLookbackStepRatio == 1, "one below" degenerates
// to a ZERO lookback, which the separate `in.win.lookback <= 0` guard also
// refuses — so a change from 10 straight to 1 does not reliably flip the
// "below" assertion here (confirmed by deliberately setting the constant to
// 1 while developing this test: every case still passed). See
// TestWindowSlideShapeEligible_ModalPanelShapeStaysOnFanout for a check that is
// NOT derived from the constant's own current value and so is not subject
// to that degeneracy.
func TestWindowSlideShapeEligible_RatioBoundary(t *testing.T) {
	base := windowSlideEligibleBaseCase()

	below := base
	below.win.lookback = (windowSlideMinLookbackStepRatio - 1) * base.ctx.step
	if windowSlideShapeEligible(below) {
		t.Errorf("ratio %d (one below the threshold) is eligible; want refused", windowSlideMinLookbackStepRatio-1)
	}

	at := base
	at.win.lookback = windowSlideMinLookbackStepRatio * base.ctx.step
	if !windowSlideShapeEligible(at) {
		t.Errorf("ratio %d (exactly the threshold) is refused; want eligible", windowSlideMinLookbackStepRatio)
	}
}

// TestWindowSlideShapeEligible_ModalPanelShapeStaysOnFanout pins a FIXED,
// non-degenerate ratio (5 — the plan's own cited "modal Grafana panel
// shape", 5-minute lookback at 1-minute step) as ineligible, independent of
// windowSlideMinLookbackStepRatio's own current value. Deliberately NOT
// derived from the constant: this is what actually catches the constant
// being weakened to something at or below 5 (confirmed: setting it to 1
// while developing this test flips this assertion, where
// TestWindowSlideShapeEligible_RatioBoundary's "below" case does not — see that
// test's own doc). The business requirement this defends is stated in the
// plan itself: the modal panel shape's real speedup was measured at only
// 1.12x, explicitly not worth the design's own correctness surface and
// maintenance cost, so this ratio must stay on the fan-out.
func TestWindowSlideShapeEligible_ModalPanelShapeStaysOnFanout(t *testing.T) {
	in := windowSlideEligibleBaseCase()
	in.win.lookback = 5 * in.ctx.step // ratio 5, e.g. 5m lookback / 1m step
	if windowSlideShapeEligible(in) {
		t.Error("the modal 5:1 Lookback/Step panel shape is eligible for window-slide; " +
			"the plan's own measured 1.12x speedup there does not justify it")
	}
}

// TestWindowSlideEligible_DisabledPending2511 pins the outer gate
// windowSlideEligible adds on top of windowSlideShapeEligible: even a
// shape-eligible input (windowSlideEligibleBaseCase's own base case) must
// be refused by windowSlideEligible while windowSlideDisabledPending2511 is
// true — see that constant's own doc for the real OOM regression
// (issue #2511) this gate exists to stop. This test asserts
// windowSlideDisabledPending2511 is true directly, so it fails loudly
// (rather than silently going vacuous) the moment someone flips the
// constant back without also updating this test — expected, as part of
// re-enabling the mechanism once #2511 is root-caused and re-verified
// against a real benchmark.
func TestWindowSlideEligible_DisabledPending2511(t *testing.T) {
	if !windowSlideDisabledPending2511 {
		t.Fatal("windowSlideDisabledPending2511 is false — issue #2511's mitigation has been " +
			"reverted; update or remove this test as part of that change")
	}
	base := windowSlideEligibleBaseCase()
	if !windowSlideShapeEligible(base) {
		t.Fatal("base case is not even shape-eligible — this test cannot distinguish the " +
			"outer gate from a shape refusal until TestWindowSlideShapeEligible_BaseCaseIsEligible passes")
	}
	if windowSlideEligible(base) {
		t.Error("windowSlideEligible(base case) = true while windowSlideDisabledPending2511 is true; " +
			"the outer gate is not being enforced")
	}
}

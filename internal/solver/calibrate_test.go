package solver

import (
	"reflect"
	"testing"
)

// calibDefaults returns a conservative shipped Config in auto mode for the
// calibrator tests. These are GENERIC shipped values — never a deployment's
// learned constants. The tests assert Calibrate only ever moves them in the
// safer direction and never bakes any deployment-specific number in.
func calibDefaults() Config {
	c := DefaultConfig()
	c.Mode = ModeAuto
	c.MinFanout = 16
	c.MinAnchorPairs = 4000
	c.MinAnchorsPerSlice = 16
	c.MaxK = 64
	return c
}

// noSignalCorpus models a no-signal corpus: every dispatch is route A,
// below-threshold never fired (the thresholds never gated a plan), and there
// are zero OOM/cost-danger outcomes. Calibrate MUST treat this as no-signal and
// return the defaults verbatim — the load-bearing no-op-on-no-signal proof.
func noSignalCorpus() []CorpusSample {
	out := make([]CorpusSample, 0, 8)
	for i := 0; i < 8; i++ {
		out = append(out, CorpusSample{
			NAnchors:    100 + i,
			Fanout:      4 + i,
			CumulativeD: 300,
			Route:       "A",
			// Not below-threshold: these were genuinely small (ineligible /
			// instant), never held back by the cost thresholds.
			BelowThreshold: false,
			OOM:            false,
			Count:          50, // 8 * 50 = 400 ≥ minCalibrationSamples
		})
	}
	return out
}

func TestCalibrate_NoOpOnNoSignalCorpus(t *testing.T) {
	defaults := calibDefaults()
	got, report := Calibrate(noSignalCorpus(), defaults)

	if !reflect.DeepEqual(got, defaults) {
		t.Fatalf("no-signal corpus must return defaults verbatim, got %+v want %+v", got, defaults)
	}
	if !report.NoOp {
		t.Fatalf("expected NoOp report on no-signal corpus, got %+v", report)
	}
	if len(report.Changes) != 0 {
		t.Fatalf("no-op must report zero changes, got %+v", report.Changes)
	}
}

func TestCalibrate_NoOpOnThinCorpus(t *testing.T) {
	defaults := calibDefaults()
	// Far below minCalibrationSamples even though it carries danger signal:
	// thin corpus must fail open regardless of what little it shows.
	thin := []CorpusSample{
		{NAnchors: 100, Fanout: 8, CumulativeD: 300, Route: "A", BelowThreshold: true, OOM: true, Count: 3},
	}
	got, report := Calibrate(thin, defaults)
	if !reflect.DeepEqual(got, defaults) {
		t.Fatalf("thin corpus must return defaults verbatim, got %+v", got)
	}
	if !report.NoOp || report.NoOpReason == "" {
		t.Fatalf("expected NoOp with reason on thin corpus, got %+v", report)
	}
}

func TestCalibrate_NoOpWhenNoBelowThreshold(t *testing.T) {
	defaults := calibDefaults()
	// Plenty of samples and even danger, but NO below-threshold decision: the
	// thresholds never gated a plan, so there is nothing to tighten toward.
	var s []CorpusSample
	for i := 0; i < 20; i++ {
		s = append(s, CorpusSample{
			NAnchors: 200, Fanout: 30, CumulativeD: 300, Route: "B",
			BelowThreshold: false, OOM: true, Count: 20,
		})
	}
	got, report := Calibrate(s, defaults)
	if !reflect.DeepEqual(got, defaults) {
		t.Fatalf("no below-threshold → defaults verbatim, got %+v", got)
	}
	if !report.NoOp {
		t.Fatalf("expected NoOp when no below-threshold decisions, got %+v", report)
	}
}

func TestCalibrate_NoOpWhenNoDangerSignal(t *testing.T) {
	defaults := calibDefaults()
	// Below-threshold decisions exist, but ZERO danger outcomes: no evidence
	// route A near these thresholds is risky, so tightening would be
	// speculative → defaults verbatim.
	var s []CorpusSample
	for i := 0; i < 20; i++ {
		s = append(s, CorpusSample{
			NAnchors: 100, Fanout: 8, CumulativeD: 300, Route: "A",
			BelowThreshold: true, OOM: false, Count: 30,
		})
	}
	got, report := Calibrate(s, defaults)
	if !reflect.DeepEqual(got, defaults) {
		t.Fatalf("no danger signal → defaults verbatim, got %+v", got)
	}
	if !report.NoOp {
		t.Fatalf("expected NoOp when no danger signal, got %+v", report)
	}
}

// clearFrontierCorpus models a deployment whose own corpus shows a clear
// OOM/cost frontier well BELOW the shipped defaults: queries at fanout ~10 and
// anchor-pairs ~1000 are already OOMing, yet the defaults (MinFanout 16,
// MinAnchorPairs 4000) keep them on route A (below-threshold). Calibrate must
// tighten MinFanout / MinAnchorPairs DOWN so those queries route B before the
// frontier — but never below the safety floor.
func clearFrontierCorpus() []CorpusSample {
	var s []CorpusSample
	// The danger frontier: fanout 10, N=100 → anchor-pairs 1000, OOMing.
	for i := 0; i < 10; i++ {
		s = append(s, CorpusSample{
			NAnchors: 100, Fanout: 10, CumulativeD: 300, Route: "A",
			BelowThreshold: true,
			OOM:            true,
			Count:          30,
		})
	}
	// Some safe small queries below the frontier, also below-threshold.
	for i := 0; i < 5; i++ {
		s = append(s, CorpusSample{
			NAnchors: 50, Fanout: 3, CumulativeD: 300, Route: "A",
			BelowThreshold: true, OOM: false, Count: 40,
		})
	}
	return s
}

func TestCalibrate_ClearFrontierTightensTowardRouteB(t *testing.T) {
	defaults := calibDefaults()
	got, report := Calibrate(clearFrontierCorpus(), defaults)

	if report.NoOp {
		t.Fatalf("clear frontier must NOT be a no-op, got %+v", report)
	}
	// Tightened in the safer direction: lower thresholds → shard more readily.
	if got.MinFanout >= defaults.MinFanout {
		t.Fatalf("MinFanout must tighten DOWN: got %d, default %d", got.MinFanout, defaults.MinFanout)
	}
	if got.MinAnchorPairs >= defaults.MinAnchorPairs {
		t.Fatalf("MinAnchorPairs must tighten DOWN: got %d, default %d", got.MinAnchorPairs, defaults.MinAnchorPairs)
	}
	// Never past the shipped safety floor.
	if got.MinFanout < minCalibratedFanout {
		t.Fatalf("MinFanout below safety floor: got %d floor %d", got.MinFanout, minCalibratedFanout)
	}
	if got.MinAnchorPairs < minCalibratedAnchorPairs {
		t.Fatalf("MinAnchorPairs below safety floor: got %d floor %d", got.MinAnchorPairs, minCalibratedAnchorPairs)
	}
	// Calibrated threshold must sit strictly below the observed frontier so
	// route B fires BEFORE the danger zone (the safety margin).
	if got.MinFanout >= report.FrontierFanout {
		t.Fatalf("MinFanout %d must be below the frontier fanout %d", got.MinFanout, report.FrontierFanout)
	}
	if len(report.Changes) == 0 {
		t.Fatalf("expected reported changes on a real tightening, got none")
	}
}

// TestCalibrate_NeverLoosensTightenOnlyInvariant fuzzes a range of corpora and
// asserts the calibrated thresholds NEVER exceed the defaults (the tighten-only
// rail) and NEVER drop below the safety floor.
func TestCalibrate_NeverLoosensTightenOnlyInvariant(t *testing.T) {
	defaults := calibDefaults()

	corpora := map[string][]CorpusSample{
		"no-signal":      noSignalCorpus(),
		"clear-frontier": clearFrontierCorpus(),
		"no-below":       mustManySamples(false, true),
		"no-danger":      mustManySamples(true, false),
		// A corpus whose frontier sits ABOVE the defaults: danger only at huge
		// fanout. Must not loosen the defaults upward toward that frontier.
		"high-frontier": highFrontierCorpus(),
	}

	for name, c := range corpora {
		got, _ := Calibrate(c, defaults)
		if got.MinFanout > defaults.MinFanout {
			t.Errorf("%s: MinFanout LOOSENED %d > default %d", name, got.MinFanout, defaults.MinFanout)
		}
		if got.MinAnchorPairs > defaults.MinAnchorPairs {
			t.Errorf("%s: MinAnchorPairs LOOSENED %d > default %d", name, got.MinAnchorPairs, defaults.MinAnchorPairs)
		}
		if got.MinFanout < minCalibratedFanout {
			t.Errorf("%s: MinFanout below floor %d", name, got.MinFanout)
		}
		if got.MinAnchorPairs < minCalibratedAnchorPairs {
			t.Errorf("%s: MinAnchorPairs below floor %d", name, got.MinAnchorPairs)
		}
		// Geometric knobs must always pass through untouched.
		if got.MaxK != defaults.MaxK || got.MinAnchorsPerSlice != defaults.MinAnchorsPerSlice {
			t.Errorf("%s: geometric knobs must pass through: got MaxK=%d MAPS=%d", name, got.MaxK, got.MinAnchorsPerSlice)
		}
	}
}

// highFrontierCorpus: danger only at very high fanout (way above defaults), so
// the frontier confirms the defaults already route safely → no loosening.
func highFrontierCorpus() []CorpusSample {
	var s []CorpusSample
	for i := 0; i < 10; i++ {
		s = append(s, CorpusSample{
			NAnchors: 5000, Fanout: 2000, CumulativeD: 300, Route: "B",
			BelowThreshold: true, OOM: true, Count: 30,
		})
	}
	return s
}

func mustManySamples(belowThreshold, oom bool) []CorpusSample {
	var s []CorpusSample
	for i := 0; i < 20; i++ {
		s = append(s, CorpusSample{
			NAnchors: 100, Fanout: 10, CumulativeD: 300, Route: "A",
			BelowThreshold: belowThreshold, OOM: oom, Count: 30,
		})
	}
	return s
}

// TestCalibrate_DeterministicAndPure asserts repeated calls with the same input
// (in different slice orders) yield identical output — no clock / RNG / order
// dependence.
func TestCalibrate_DeterministicAndPure(t *testing.T) {
	defaults := calibDefaults()
	base := clearFrontierCorpus()

	first, r1 := Calibrate(base, defaults)
	for iter := 0; iter < 5; iter++ {
		again, r2 := Calibrate(base, defaults)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("non-deterministic Config: %+v vs %+v", first, again)
		}
		if !reflect.DeepEqual(r1, r2) {
			t.Fatalf("non-deterministic report: %+v vs %+v", r1, r2)
		}
	}

	// Order independence: reversing the sample slice must not change the result.
	rev := make([]CorpusSample, len(base))
	for i := range base {
		rev[len(base)-1-i] = base[i]
	}
	revGot, _ := Calibrate(rev, defaults)
	if !reflect.DeepEqual(first, revGot) {
		t.Fatalf("order-dependent result: forward %+v reversed %+v", first, revGot)
	}

	// Purity: Calibrate must not mutate its input slice.
	snapshot := make([]CorpusSample, len(base))
	copy(snapshot, base)
	_, _ = Calibrate(base, defaults)
	if !reflect.DeepEqual(base, snapshot) {
		t.Fatalf("Calibrate mutated its input slice")
	}
}

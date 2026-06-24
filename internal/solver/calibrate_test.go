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

// noBelowThresholdReason is the exact NoOpReason for the no-below-threshold
// rail. Pinning the literal kills a mutant that returns the wrong rail's reason.
const noBelowThresholdReason = "no below-threshold decisions: thresholds never gated a plan"

func TestCalibrate_NoOpWhenNoBelowThreshold(t *testing.T) {
	defaults := calibDefaults()
	// Rail isolation (KILL-MUTANT): plenty of samples (rail 1 silent) AND a
	// real danger frontier — 20*20 = 400 OOM danger samples ≥ floor, valid
	// frontier (rail 3 silent) — but NO below-threshold decision, so ONLY the
	// below-threshold rail can fire. Inverting that rail must change the result,
	// which it cannot if another rail also no-ops this corpus.
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
	if report.NoOpReason != noBelowThresholdReason {
		t.Fatalf("wrong NoOpReason: got %q want %q", report.NoOpReason, noBelowThresholdReason)
	}
}

// noDangerReason is the exact NoOpReason for the no-danger-signal rail.
const noDangerReason = "no OOM/cost-danger frontier: corpus shows no danger signal"

func TestCalibrate_NoOpWhenNoDangerSignal(t *testing.T) {
	defaults := calibDefaults()
	// Rail isolation (KILL-MUTANT): plenty of samples (rail 1 silent) AND
	// below-threshold decisions exist (rail 2 silent), but ZERO danger
	// outcomes, so ONLY the no-danger rail can fire. 20*30 = 600 below-threshold
	// route-A samples, none OOM.
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
	if report.NoOpReason != noDangerReason {
		t.Fatalf("wrong NoOpReason: got %q want %q", report.NoOpReason, noDangerReason)
	}
}

// thinReason is the exact NoOpReason for the thin-corpus rail.
const thinReason = "thin corpus: below min-sample floor"

// TestCalibrate_ThinCorpusRailIsolated (KILL-MUTANT) builds a corpus that
// carries a REAL danger frontier (below-threshold + ≥ minFrontierDangerSamples
// OOM samples at a valid coordinate) but stays just under the min-sample floor.
// Only the thin-corpus rail may fire: the other two rails would PASS this
// corpus through to tightening, so deleting / weakening the min-sample floor
// flips the result from defaults to a real calibration. The existing
// TestCalibrate_NoOpOnThinCorpus uses a single count=3 sample (dangerCount=3 <
// floor), so the no-danger rail ALSO no-ops it — setting the min-sample floor
// to 1 there leaves the test green. This case closes that hole.
func TestCalibrate_ThinCorpusRailIsolated(t *testing.T) {
	defaults := calibDefaults()
	var s []CorpusSample
	// 6 OOM danger samples (≥ minFrontierDangerSamples) at a clear sub-default
	// frontier, all below-threshold — but Count=1 each so the total is far
	// under minCalibrationSamples.
	for i := 0; i < 6; i++ {
		s = append(s, CorpusSample{
			NAnchors: 100, Fanout: 10, CumulativeD: 300, Route: "A",
			BelowThreshold: true, OOM: true, Count: 1,
		})
	}
	total := 0
	for _, x := range s {
		total += x.Count
	}
	if total >= minCalibrationSamples {
		t.Fatalf("test setup: corpus not thin (total=%d ≥ floor %d)", total, minCalibrationSamples)
	}
	got, report := Calibrate(s, defaults)
	if !reflect.DeepEqual(got, defaults) {
		t.Fatalf("thin corpus must return defaults verbatim, got %+v", got)
	}
	if !report.NoOp || report.NoOpReason != thinReason {
		t.Fatalf("expected thin-corpus rail, got NoOp=%v reason=%q", report.NoOp, report.NoOpReason)
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

// dangerBoundaryCorpus builds a corpus with exactly `dangerCount` OOM danger
// samples (Count=1 each) at a clear sub-default frontier, padded with enough
// safe below-threshold dispatches that the total clears minCalibrationSamples.
// Below-threshold is always present so only the danger-sample floor decides.
func dangerBoundaryCorpus(dangerCount int) []CorpusSample {
	var s []CorpusSample
	for i := 0; i < dangerCount; i++ {
		s = append(s, CorpusSample{
			NAnchors: 100, Fanout: 10, CumulativeD: 300, Route: "A",
			BelowThreshold: true, OOM: true, Count: 1,
		})
	}
	// Padding: safe, below-threshold, non-OOM — total well over the sample floor
	// without adding danger or a smaller frontier.
	s = append(s, CorpusSample{
		NAnchors: 10, Fanout: 1, CumulativeD: 300, Route: "A",
		BelowThreshold: true, OOM: false, Count: minCalibrationSamples + 10,
	})
	return s
}

// TestCalibrate_DangerSampleFloorBoundary (KILL-MUTANT) pins the
// minFrontierDangerSamples floor at its exact value: dangerCount = floor-1 must
// no-op (the no-danger rail), dangerCount = floor must tighten. A mutant that
// lowers the floor to 1 (or removes the comparison) lets the floor-1 case
// tighten, flipping this test red.
func TestCalibrate_DangerSampleFloorBoundary(t *testing.T) {
	defaults := calibDefaults()
	// Pin the literal floor value so a mutant that moves minFrontierDangerSamples
	// (e.g. down to 1) cannot also slide the boundary these corpora probe.
	const wantDangerFloor = 5
	if minFrontierDangerSamples != wantDangerFloor {
		t.Fatalf("danger-floor moved to %d; update this test's literal corpora deliberately",
			minFrontierDangerSamples)
	}

	below, reportBelow := Calibrate(dangerBoundaryCorpus(wantDangerFloor-1), defaults)
	if !reflect.DeepEqual(below, defaults) {
		t.Fatalf("dangerCount=floor-1 must no-op, got %+v", below)
	}
	if !reportBelow.NoOp || reportBelow.NoOpReason != noDangerReason {
		t.Fatalf("dangerCount=floor-1: expected no-danger rail, got NoOp=%v reason=%q",
			reportBelow.NoOp, reportBelow.NoOpReason)
	}

	at, reportAt := Calibrate(dangerBoundaryCorpus(wantDangerFloor), defaults)
	if reportAt.NoOp {
		t.Fatalf("dangerCount=floor must tighten, got no-op %+v", reportAt)
	}
	if at.MinFanout >= defaults.MinFanout || at.MinAnchorPairs >= defaults.MinAnchorPairs {
		t.Fatalf("dangerCount=floor must lower thresholds, got MinFanout=%d MinAnchorPairs=%d",
			at.MinFanout, at.MinAnchorPairs)
	}
}

// TestCalibrate_AnchorPairsFloorPinsExactly (KILL-MUTANT) drives the
// anchor-pairs frontier into the band [floor+1, floor/0.75] so the margin-
// reduced target falls BELOW the floor while the frontier itself stays above
// it. The floor must pin the result at EXACTLY minCalibratedAnchorPairs and set
// FloorClampedAnchorPairs. clearFrontierCorpus never exercises this (its pairs
// frontier is 1000 → margin 750, well above the 500 floor), so the floor clamp
// was deletable-green before this test. The fanout axis is kept clean (frontier
// 10 → margin 7, above its floor of 2) to isolate the pairs clamp.
func TestCalibrate_AnchorPairsFloorPinsExactly(t *testing.T) {
	defaults := calibDefaults()
	// frontier pairs = 60*10 = 600 ∈ [501,666]: margin 450 < floor 500, but
	// frontier 600 > floor → the floor PINS (not the sub-floor cap).
	var s []CorpusSample
	for i := 0; i < minFrontierDangerSamples+1; i++ {
		s = append(s, CorpusSample{
			NAnchors: 60, Fanout: 10, CumulativeD: 300, Route: "A",
			BelowThreshold: true, OOM: true, Count: 10,
		})
	}
	s = append(s, CorpusSample{
		NAnchors: 10, Fanout: 1, CumulativeD: 300, Route: "A",
		BelowThreshold: true, OOM: false, Count: minCalibrationSamples,
	})

	got, report := Calibrate(s, defaults)
	if report.NoOp {
		t.Fatalf("expected a real calibration, got no-op %+v", report)
	}
	if got.MinAnchorPairs != minCalibratedAnchorPairs {
		t.Fatalf("anchor-pairs must pin EXACTLY at floor %d, got %d",
			minCalibratedAnchorPairs, got.MinAnchorPairs)
	}
	if !report.FloorClampedAnchorPairs {
		t.Fatalf("expected FloorClampedAnchorPairs=true when the floor swallowed the margin")
	}
	// The floor sits below the frontier — route B still fires before danger.
	if got.MinAnchorPairs >= report.FrontierAnchorPairs {
		t.Fatalf("floor-pinned threshold %d must stay below frontier %d",
			got.MinAnchorPairs, report.FrontierAnchorPairs)
	}
	// Fanout axis stays clean (no clamp) so the pairs clamp is isolated.
	if report.FloorClampedFanout {
		t.Fatalf("fanout axis should not be floor-clamped in this corpus")
	}
}

// TestCalibrate_FanoutSubFloorCapsBelowFrontier (KILL-MUTANT) drives the fanout
// frontier to 2 — at/below minCalibratedFanout. Pinning to the floor (2) would
// install a gate AT the OOM coordinate, leaving an OOMing query on route A. The
// sub-floor branch must instead cap STRICTLY BELOW the frontier (frontier-1=1)
// and flag FloorClampedFanout. A mutant that pins to the floor here yields
// MinFanout=2 == frontier, failing the strict-below assertion. The pairs axis
// is kept clean (frontier 800 → margin 600, above floor 500) to isolate fanout.
func TestCalibrate_FanoutSubFloorCapsBelowFrontier(t *testing.T) {
	defaults := calibDefaults()
	// frontier fanout = 2 (≤ floor 2); pairs = 400*2 = 800 (margin 600 ≥ floor).
	var s []CorpusSample
	for i := 0; i < minFrontierDangerSamples+1; i++ {
		s = append(s, CorpusSample{
			NAnchors: 400, Fanout: 2, CumulativeD: 300, Route: "A",
			BelowThreshold: true, OOM: true, Count: 10,
		})
	}
	s = append(s, CorpusSample{
		NAnchors: 10, Fanout: 1, CumulativeD: 300, Route: "A",
		BelowThreshold: true, OOM: false, Count: minCalibrationSamples,
	})

	got, report := Calibrate(s, defaults)
	if report.NoOp {
		t.Fatalf("expected a real calibration, got no-op %+v", report)
	}
	if report.FrontierFanout != minCalibratedFanout {
		t.Fatalf("test setup: expected frontier fanout %d, got %d",
			minCalibratedFanout, report.FrontierFanout)
	}
	if !report.FloorClampedFanout {
		t.Fatalf("expected FloorClampedFanout=true on a sub-floor OOM")
	}
	// Strictly below the frontier — a floor-pin (==2) would fail here.
	if got.MinFanout >= report.FrontierFanout {
		t.Fatalf("sub-floor cap must stay STRICTLY below frontier %d, got MinFanout=%d",
			report.FrontierFanout, got.MinFanout)
	}
	if got.MinFanout != report.FrontierFanout-1 {
		t.Fatalf("sub-floor cap must be frontier-1=%d, got %d",
			report.FrontierFanout-1, got.MinFanout)
	}
	// Pairs axis clean (no clamp) so the fanout clamp is isolated.
	if report.FloorClampedAnchorPairs {
		t.Fatalf("anchor-pairs axis should not be floor-clamped in this corpus")
	}
}

// TestCalibrate_CoupledFrontierIgnoresFanoutGatedOOM (KILL-MUTANT for Fix 4)
// proves the frontier coordinate is COUPLED to a single OOM sample. The corpus
// has two disjoint OOM samples:
//
//   - a fanout-gated OOM at small fanout but LARGE anchor-pairs (F=3, N=400 →
//     pairs=1200), and
//   - the real binding corner at larger fanout but SMALLER pairs (F=12, N=50 →
//     pairs=600).
//
// Independent per-axis minimization (the old bug) would take frontierFanout=3
// from the first sample and frontierAnchorPairs=600 from the second — a
// synthetic corner at no real query. The coupled scan must report BOTH axes
// from the smallest-pairs OOM sample: frontierFanout=12, frontierAnchorPairs=600.
func TestCalibrate_CoupledFrontierIgnoresFanoutGatedOOM(t *testing.T) {
	defaults := calibDefaults()
	var s []CorpusSample
	for i := 0; i < minFrontierDangerSamples; i++ {
		// Fanout-gated OOM: small fanout, large pairs.
		s = append(s, CorpusSample{
			NAnchors: 400, Fanout: 3, CumulativeD: 300, Route: "A",
			BelowThreshold: true, OOM: true, Count: 10,
		})
		// Binding corner: larger fanout, smaller pairs.
		s = append(s, CorpusSample{
			NAnchors: 50, Fanout: 12, CumulativeD: 300, Route: "A",
			BelowThreshold: true, OOM: true, Count: 10,
		})
	}
	s = append(s, CorpusSample{
		NAnchors: 10, Fanout: 1, CumulativeD: 300, Route: "A",
		BelowThreshold: true, OOM: false, Count: minCalibrationSamples,
	})

	_, report := Calibrate(s, defaults)
	const wantFrontierF = 12
	const wantFrontierPairs = 600 // 50 * 12
	if report.FrontierFanout != wantFrontierF {
		t.Fatalf("coupled frontier fanout: got %d want %d (must come from the smallest-pairs OOM, not the fanout-gated one)",
			report.FrontierFanout, wantFrontierF)
	}
	if report.FrontierAnchorPairs != wantFrontierPairs {
		t.Fatalf("coupled frontier pairs: got %d want %d", report.FrontierAnchorPairs, wantFrontierPairs)
	}
}

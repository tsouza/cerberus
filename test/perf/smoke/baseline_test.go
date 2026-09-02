package smoke

import "testing"

// preClampCeilingBytes records, per sentinel, the ceiling
// perf-smoke-baseline.json carried before issue #2906 clamped the calibration
// path — the numbers produced by an unclamped
// max-of-N * sentinelBaselineHeadroom.
//
// Both are ABOVE sentinelCapCeilingBytes, which is precisely the defect: a
// PRONG (b) ceiling above PRONG (a)'s can never fire, because PRONG (a)
// rejects any measurement that would have reached it. These two sentinels ran
// with one of their two guards silently dead for the whole life of the corpus,
// on the lane that gates every PR through the required strict-scan job.
//
// They are kept here, rather than deleted with the baseline they came from,
// because they are what makes
// TestPerfSmokeBaseline_ProngBFiresWhereItPreviouslyCouldNot a real
// before/after: the probe it rejects is a value the OLD ceiling demonstrably
// accepted, not an arbitrary large number.
var preClampCeilingBytes = map[string]uint64{
	"spill_high_cardinality_groupby": 977_277_601,
	"compare_memory_bound":           1_023_624_673,
}

// TestCommittedCeilingBytes_ClampsToTheAbsoluteCeiling pins the clamp itself.
// Without it the calibration path is free to write a PRONG (b) ceiling that
// PRONG (a) makes unreachable, which is issue #2906 exactly.
func TestCommittedCeilingBytes_ClampsToTheAbsoluteCeiling(t *testing.T) {
	// A measurement whose headroom multiple still fits under the absolute
	// ceiling keeps the full multiple; one whose multiple overshoots is
	// clamped to the absolute ceiling. The boundary case (a measurement whose
	// multiple lands exactly on the ceiling) must NOT be clamped away from the
	// value the unclamped arithmetic produces.
	// A var, not a const: 651,518,401 * 1.5 is not an integer, so the
	// unclamped cross-check below has to go through runtime float64 arithmetic
	// rather than Go's exact-rational constant folding.
	var overshoot uint64 = 651_518_401 // spill_high_cardinality_groupby's calibration measurement
	const boundary = uint64(float64(sentinelCapCeilingBytes) / sentinelBaselineHeadroom)

	cases := []struct {
		name     string
		maxBytes uint64
		want     uint64
	}{
		{"well under the ceiling", 100_000_000, 150_000_000},
		{"exactly on the ceiling", boundary, sentinelCapCeilingBytes},
		{"overshoots the ceiling", overshoot, sentinelCapCeilingBytes},
		{"already over the ceiling", sentinelCapCeilingBytes + 1, sentinelCapCeilingBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := committedCeilingBytes(tc.maxBytes); got != tc.want {
				t.Fatalf("committedCeilingBytes(%d) = %d, want %d", tc.maxBytes, got, tc.want)
			}
		})
	}

	// The overshoot case is only meaningful if the UNCLAMPED arithmetic really
	// would have produced a ceiling above the absolute one — otherwise the
	// assertion above would hold with the clamp deleted.
	unclamped := uint64(float64(overshoot) * sentinelBaselineHeadroom)
	if unclamped <= sentinelCapCeilingBytes {
		t.Fatalf("unclamped ceiling for %d is %d, which does not exceed the absolute ceiling %d — "+
			"the clamp case above proves nothing; pick a larger measurement",
			overshoot, unclamped, sentinelCapCeilingBytes)
	}
}

// TestPerfSmokeBaseline_ProngBIsNeverLooserThanProngA is the gate issue #2906
// exists for. It asserts, over the COMMITTED baseline rather than over
// arithmetic alone, that every sentinel's PRONG (b) ceiling is one PRONG (a)
// can actually let a measurement reach — and that the committed number is
// exactly what committedCeilingBytes derives from the committed measurement,
// so a hand-edited or half-regenerated baseline (invariant 9) fails here
// rather than silently gating less than it claims.
func TestPerfSmokeBaseline_ProngBIsNeverLooserThanProngA(t *testing.T) {
	baseline := mustReadBaseline(t)
	for _, sentinel := range Sentinels {
		bound, ok := baselineFor(baseline, sentinel.Name)
		if !ok {
			t.Errorf("%s: no committed bound in %s — the sentinel runs with PRONG (b) missing entirely",
				sentinel.Name, perfSmokeBaselinePath)
			continue
		}
		if bound.CeilingBytes > sentinelCapCeilingBytes {
			t.Errorf("%s: committed ceiling %d exceeds the absolute cap-relative ceiling %d — "+
				"PRONG (a) rejects first, so PRONG (b) can never fire and this sentinel gates "+
				"with one of its two guards dead (#2906)",
				sentinel.Name, bound.CeilingBytes, sentinelCapCeilingBytes)
		}
		if want := committedCeilingBytes(bound.MaxOfNBytes); bound.CeilingBytes != want {
			t.Errorf("%s: committed ceiling %d is not committedCeilingBytes(%d) = %d — regenerate with "+
				"`just update-perf-smoke-baseline` rather than editing the file",
				sentinel.Name, bound.CeilingBytes, bound.MaxOfNBytes, want)
		}
	}
}

// TestPerfSmokeBaseline_CalibrationMeasurementPassesBothProngs is the other
// direction: tightening PRONG (b) is only correct if a legitimate measurement
// still passes. The calibration-time max-of-N committed alongside each ceiling
// is the most legitimate measurement there is, so it must clear both prongs
// with the documented headroom still intact.
func TestPerfSmokeBaseline_CalibrationMeasurementPassesBothProngs(t *testing.T) {
	baseline := mustReadBaseline(t)
	for _, bound := range baseline.Sentinels {
		if exceedsCapCeiling(bound.MaxOfNBytes) {
			t.Errorf("%s: calibration measurement %d already exceeds the absolute ceiling %d — "+
				"the committed baseline records a PRONG (a) failure",
				bound.Name, bound.MaxOfNBytes, sentinelCapCeilingBytes)
		}
		if exceedsCommittedCeiling(bound.MaxOfNBytes, bound) {
			t.Errorf("%s: calibration measurement %d already exceeds its own committed ceiling %d — "+
				"the gate is red on the very run that produced it",
				bound.Name, bound.MaxOfNBytes, bound.CeilingBytes)
		}
	}
}

// TestPerfSmokeBaseline_ProngBFiresWhereItPreviouslyCouldNot is the
// red-before-green half of the #2906 fix, expressed as a standing assertion:
// for each sentinel whose ceiling was above the absolute one, a peak-memory
// value the OLD ceiling accepted must now be rejected by the committed one.
//
// The probe is derived, not picked: the midpoint of the interval
// (sentinelCapCeilingBytes, preClampCeilingBytes) — every value in which the
// pre-clamp ceiling let through and the clamped ceiling must not.
func TestPerfSmokeBaseline_ProngBFiresWhereItPreviouslyCouldNot(t *testing.T) {
	baseline := mustReadBaseline(t)
	for name, preClamp := range preClampCeilingBytes {
		bound, ok := baselineFor(baseline, name)
		if !ok {
			t.Errorf("%s: no committed bound in %s", name, perfSmokeBaselinePath)
			continue
		}
		if preClamp <= sentinelCapCeilingBytes {
			t.Errorf("%s: recorded pre-clamp ceiling %d does not exceed the absolute ceiling %d — "+
				"this sentinel was never one of the inert ones and does not belong in "+
				"preClampCeilingBytes", name, preClamp, sentinelCapCeilingBytes)
			continue
		}
		probe := sentinelCapCeilingBytes + (preClamp-sentinelCapCeilingBytes)/2
		if probe >= preClamp {
			t.Fatalf("%s: probe %d is not strictly below the pre-clamp ceiling %d", name, probe, preClamp)
		}

		// The probe is a peak the PRE-CLAMP ceiling accepted. Stated against
		// the real prong predicate, not a restatement of it: this is the exact
		// call the integration harness makes.
		if exceedsCommittedCeiling(probe, sentinelBound{Name: name, CeilingBytes: preClamp}) {
			t.Fatalf("%s: probe %d already tripped the pre-clamp ceiling %d — it is not in the "+
				"interval the clamp is supposed to have reclaimed", name, probe, preClamp)
		}
		if !exceedsCommittedCeiling(probe, bound) {
			t.Errorf("%s: PRONG (b) accepts a peak memory of %d bytes against the committed ceiling %d — "+
				"the pre-clamp ceiling %d accepted it too, so PRONG (b) has not actually been "+
				"restored for this sentinel (#2906)",
				name, probe, bound.CeilingBytes, preClamp)
		}

		// The other direction, on the same predicate: the calibration
		// measurement this ceiling was derived from must still pass both
		// prongs. A clamp that fired hard enough to reject a legitimate
		// measurement would be a different bug, not a fix.
		if exceedsCommittedCeiling(bound.MaxOfNBytes, bound) || exceedsCapCeiling(bound.MaxOfNBytes) {
			t.Errorf("%s: the calibration measurement %d no longer passes both prongs against "+
				"committed ceiling %d / absolute ceiling %d",
				name, bound.MaxOfNBytes, bound.CeilingBytes, sentinelCapCeilingBytes)
		}
	}
}

func mustReadBaseline(t *testing.T) perfSmokeBaseline {
	t.Helper()
	baseline, err := readPerfSmokeBaseline()
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(baseline.Sentinels) == 0 {
		t.Fatalf("%s carries no sentinel bounds", perfSmokeBaselinePath)
	}
	return baseline
}

package nightly

import "testing"

// TestCommittedCeilingBytes_ClampsToTheAbsoluteCeiling pins the clamp itself.
// Without it the calibration path is free to write a PRONG (b) ceiling that
// PRONG (a) makes unreachable — a per-sentinel bound that gates nothing, which
// is what test/perf/smoke shipped with until issue #2906.
func TestCommittedCeilingBytes_ClampsToTheAbsoluteCeiling(t *testing.T) {
	// pod_status_reason_gauge's own calibration measurement: the sentinel whose
	// existence forced the clamp here in the first place, since 1.5x of it
	// overshoots the 1 GiB cap outright.
	var overshoot uint64 = 751_191_629
	const headroom = 1.5

	cases := []struct {
		name     string
		maxBytes uint64
		want     uint64
	}{
		{"well under the ceiling", 100_000_000, 150_000_000},
		{"overshoots the ceiling", overshoot, nightlyCapCeilingBytes},
		{"already over the ceiling", nightlyCapCeilingBytes + 1, nightlyCapCeilingBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := committedCeilingBytes(tc.maxBytes, headroom); got != tc.want {
				t.Fatalf("committedCeilingBytes(%d, %v) = %d, want %d", tc.maxBytes, headroom, got, tc.want)
			}
		})
	}

	// The overshoot case is only meaningful if the UNCLAMPED arithmetic really
	// would have produced a ceiling above the absolute one — otherwise the
	// assertion above would hold with the clamp deleted.
	unclamped := uint64(float64(overshoot) * headroom)
	if unclamped <= nightlyCapCeilingBytes {
		t.Fatalf("unclamped ceiling for %d is %d, which does not exceed the absolute ceiling %d — "+
			"the clamp case above proves nothing; pick a larger measurement",
			overshoot, unclamped, nightlyCapCeilingBytes)
	}
}

// TestNightlyBaseline_ProngBIsNeverLooserThanProngA asserts, over the COMMITTED
// baseline rather than over arithmetic alone, that every sentinel's PRONG (b)
// ceiling is one PRONG (a) can actually let a measurement reach — and that the
// committed number is exactly what committedCeilingBytes derives from the
// committed measurement and the sentinel's own headroom, so a hand-edited or
// half-regenerated baseline (invariant 9) fails here rather than silently
// gating less than it claims.
//
// test/perf/smoke's baseline_test.go carries the same assertion over its own
// corpus. Both lanes clamp, so both are held to it: an invariant enforced in
// one of two sibling corpora is one the other is free to drift away from.
func TestNightlyBaseline_ProngBIsNeverLooserThanProngA(t *testing.T) {
	baseline := mustReadBaseline(t)
	for _, sentinel := range Sentinels {
		bound, ok := baselineFor(baseline, sentinel.Name)
		if !ok {
			t.Errorf("%s: no committed bound in %s — the sentinel runs with PRONG (b) missing entirely",
				sentinel.Name, nightlyBaselinePath)
			continue
		}
		if bound.CeilingBytes > nightlyCapCeilingBytes {
			t.Errorf("%s: committed ceiling %d exceeds the absolute cap-relative ceiling %d — "+
				"PRONG (a) rejects first, so PRONG (b) can never fire and this sentinel gates "+
				"with one of its two guards dead",
				sentinel.Name, bound.CeilingBytes, nightlyCapCeilingBytes)
		}
		if want := committedCeilingBytes(bound.MaxOfNBytes, sentinel.BaselineHeadroom); bound.CeilingBytes != want {
			t.Errorf("%s: committed ceiling %d is not committedCeilingBytes(%d, %v) = %d — regenerate with "+
				"`just update-nightly-perf-baseline` rather than editing the file",
				sentinel.Name, bound.CeilingBytes, bound.MaxOfNBytes, sentinel.BaselineHeadroom, want)
		}
	}
}

// TestNightlyBaseline_CalibrationMeasurementPassesBothProngs is the other
// direction: a clamped PRONG (b) is only correct if a legitimate measurement
// still passes. The calibration-time max-of-N committed alongside each ceiling
// is the most legitimate measurement there is, so it must clear both prongs.
func TestNightlyBaseline_CalibrationMeasurementPassesBothProngs(t *testing.T) {
	baseline := mustReadBaseline(t)
	for _, bound := range baseline.Sentinels {
		if bound.MaxOfNBytes > nightlyCapCeilingBytes {
			t.Errorf("%s: calibration measurement %d already exceeds the absolute ceiling %d — "+
				"the committed baseline records a PRONG (a) failure",
				bound.Name, bound.MaxOfNBytes, nightlyCapCeilingBytes)
		}
		if bound.MaxOfNBytes > bound.CeilingBytes {
			t.Errorf("%s: calibration measurement %d already exceeds its own committed ceiling %d — "+
				"the gate is red on the very run that produced it",
				bound.Name, bound.MaxOfNBytes, bound.CeilingBytes)
		}
	}
}

func mustReadBaseline(t *testing.T) nightlyBaseline {
	t.Helper()
	baseline, err := readNightlyBaseline()
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(baseline.Sentinels) == 0 {
		t.Fatalf("%s carries no sentinel bounds", nightlyBaselinePath)
	}
	return baseline
}

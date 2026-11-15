package nightly

import (
	"net/http"
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/chopt"
)

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

// The 26.9 label_values_matcher_stock_key_renamed peaks the cases below are
// built from: max-of-5 readings of one sentinel, unchanged code, one server.
const (
	// The two calibration runs, each of which committed its own max-of-N.
	labelValuesRenamedCalibrationLow  uint64 = 24_632_333
	labelValuesRenamedCalibrationHigh uint64 = 26_550_752
	// The two gating runs that went red against the first calibration's
	// ceiling, and the one that went red against the second's.
	labelValuesRenamedGatingRedFirst  uint64 = 30_693_646
	labelValuesRenamedGatingRedSecond uint64 = 30_301_384
	labelValuesRenamedGatingRedThird  uint64 = 32_643_773
)

// TestCalibratedBound_LowTailRoundCannotSetTheCeiling is the single-run
// calibration failure: one run's max-of-N landed in the low tail of the
// run-to-run distribution, and the ceiling derived from it alone sat below
// what ordinary gating runs measure. calibratedBound must anchor the ceiling
// on the highest of the rounds, wherever the low-tail round falls among them,
// so the gating runs held out of the calibration clear it.
func TestCalibratedBound_LowTailRoundCannotSetTheCeiling(t *testing.T) {
	const headroom = labelValuesBaselineHeadroom
	// The low-tail calibration run, the second calibration, and three gating
	// runs that passed.
	rounds := []uint64{labelValuesRenamedCalibrationLow, labelValuesRenamedCalibrationHigh, 29_349_973, 29_312_309, 29_414_657}
	const highest uint64 = 29_414_657
	heldOut := []uint64{labelValuesRenamedGatingRedFirst, labelValuesRenamedGatingRedSecond, labelValuesRenamedGatingRedThird}

	// The case is only meaningful if a ceiling derived from the low-tail round
	// alone really is one the held-out gating runs cross.
	singleRun := nightlyBound{MaxOfNBytes: labelValuesRenamedCalibrationLow, CeilingBytes: committedCeilingBytes(labelValuesRenamedCalibrationLow, headroom)}
	for _, peak := range heldOut[:2] {
		if !exceedsCommittedCeiling(peak, singleRun) {
			t.Fatalf("gating peak %d clears the single-run ceiling %d — the low-tail case proves nothing", peak, singleRun.CeilingBytes)
		}
	}

	for shift := range rounds {
		rotated := append(slices.Clone(rounds[shift:]), rounds[:shift]...)
		bound, err := calibratedBound("label_values_matcher_stock_key_renamed", http.StatusOK, rotated, headroom)
		if err != nil {
			t.Fatalf("rounds %v: %v", rotated, err)
		}
		if bound.Name != "label_values_matcher_stock_key_renamed" || bound.ExpectedStatus != http.StatusOK {
			t.Fatalf("rounds %v: bound names %q expecting %d", rotated, bound.Name, bound.ExpectedStatus)
		}
		if bound.MaxOfNBytes != highest {
			t.Fatalf("rounds %v: committed max-of-N %d, want the highest round %d", rotated, bound.MaxOfNBytes, highest)
		}
		if want := committedCeilingBytes(highest, headroom); bound.CeilingBytes != want {
			t.Fatalf("rounds %v: committed ceiling %d, want committedCeilingBytes(%d, %v) = %d",
				rotated, bound.CeilingBytes, highest, headroom, want)
		}
		for _, peak := range append(slices.Clone(rounds), heldOut...) {
			if exceedsCommittedCeiling(peak, bound) {
				t.Fatalf("rounds %v: ordinary peak %d exceeds the calibrated ceiling %d", rotated, peak, bound.CeilingBytes)
			}
		}
	}
}

// TestCalibratedBound_RefusesNoiseWiderThanTheMargin pins the spread check:
// a calibration whose rounds span more than the margin its ceiling leaves
// above the highest round is refused, and one whose margin covers the span is
// not.
func TestCalibratedBound_RefusesNoiseWiderThanTheMargin(t *testing.T) {
	// Five gating runs' max-of-5 on 26.9, against the label-values headroom:
	// 25.9M to 32.6M is a 6.75M span, and 1.20x of 32.6M leaves 6.53M.
	noisy := []uint64{25_890_318, 26_900_754, 29_349_973, 30_800_189, labelValuesRenamedGatingRedThird}
	if _, err := calibratedBound("label_values_matcher_stock_key_renamed", http.StatusOK, noisy, labelValuesBaselineHeadroom); err == nil {
		t.Fatalf("rounds %v under a %.2fx headroom were accepted", noisy, labelValuesBaselineHeadroom)
	}
	// The same rounds under a headroom whose margin covers the span: the
	// refusal above is the span's doing, not the rounds'.
	const coveringHeadroom = 1.25
	if _, err := calibratedBound("label_values_matcher_stock_key_renamed", http.StatusOK, noisy, coveringHeadroom); err != nil {
		t.Fatalf("rounds %v under a %.2fx headroom: %v", noisy, coveringHeadroom, err)
	}

	// The boundary: a margin exactly as wide as the span is accepted, one byte
	// narrower is refused. 1.5x of 100 is exactly 150, a 50-byte margin.
	const headroom = 1.5
	if _, err := calibratedBound("s", http.StatusOK, []uint64{50, 60, 70, 80, 100}, headroom); err != nil {
		t.Fatalf("a 50-byte span under a 50-byte margin: %v", err)
	}
	if _, err := calibratedBound("s", http.StatusOK, []uint64{49, 60, 70, 80, 100}, headroom); err == nil {
		t.Fatal("a 51-byte span under a 50-byte margin was accepted")
	}
}

// TestCalibratedBound_MarginIsTheClampedCeilings: the margin the span has to
// fit in is the one the COMMITTED ceiling leaves, after committedCeilingBytes
// clamps it to the absolute ceiling — not the one the nominal headroom would.
func TestCalibratedBound_MarginIsTheClampedCeilings(t *testing.T) {
	const headroom = 1.5
	// Rounds at pod_status_reason_gauge's scale, whose 1.5x is clamped: a
	// 161M margin over a 31.6M span. A non-200 status checks that the
	// expected status rides through to the bound as given.
	settled := []uint64{719_589_995, 722_049_382, 726_244_515, 740_000_000, 751_191_629}
	bound, err := calibratedBound("pod_status_reason_gauge", http.StatusUnprocessableEntity, settled, headroom)
	if err != nil {
		t.Fatalf("rounds %v: %v", settled, err)
	}
	if bound.CeilingBytes != nightlyCapCeilingBytes || bound.ExpectedStatus != http.StatusUnprocessableEntity {
		t.Fatalf("rounds %v: ceiling %d expecting %d, want the absolute ceiling %d expecting %d",
			settled, bound.CeilingBytes, bound.ExpectedStatus, nightlyCapCeilingBytes, http.StatusUnprocessableEntity)
	}

	// A 50M span: inside the 450M margin 1.5x of 900M would leave, outside the
	// 12.7M margin the clamped ceiling leaves.
	clamped := []uint64{850_000_000, 860_000_000, 870_000_000, 880_000_000, 900_000_000}
	if _, err := calibratedBound("s", http.StatusOK, clamped, headroom); err == nil {
		t.Fatalf("rounds %v were accepted against a clamped ceiling %d", clamped, nightlyCapCeilingBytes)
	}
}

// TestCalibratedBound_RefusesRoundsItCannotDeriveFrom: a single max-of-N
// round — the shape of a single-run calibration — has no range to measure, a
// round that measured nothing has no peak, and a highest round over the
// absolute ceiling already fails PRONG (a). The last two are built so the
// spread check alone would accept them.
func TestCalibratedBound_RefusesRoundsItCannotDeriveFrom(t *testing.T) {
	const doubling = 2.0 // 2x of 100 leaves a 100-byte margin over a 100-byte span
	overCap := slices.Repeat([]uint64{nightlyCapCeilingBytes + 1}, nightlyCalibrationRounds)
	for _, tc := range []struct {
		rounds   []uint64
		headroom float64
	}{
		{[]uint64{labelValuesRenamedCalibrationLow}, labelValuesBaselineHeadroom},
		{slices.Repeat([]uint64{labelValuesRenamedCalibrationLow}, nightlyCalibrationRounds-1), labelValuesBaselineHeadroom},
		{[]uint64{0, 100, 100, 100, 100}, doubling},
		{overCap, labelValuesBaselineHeadroom},
	} {
		if bound, err := calibratedBound("s", http.StatusOK, tc.rounds, tc.headroom); err == nil {
			t.Errorf("rounds %v under %.2fx were accepted as %+v", tc.rounds, tc.headroom, bound)
		}
	}
	if _, err := calibratedBound("s", http.StatusOK, slices.Repeat([]uint64{labelValuesRenamedCalibrationLow}, nightlyCalibrationRounds), labelValuesBaselineHeadroom); err != nil {
		t.Errorf("%d identical rounds: %v", nightlyCalibrationRounds, err)
	}
}

// TestNightlyBaseline_ProngBIsNeverLooserThanProngA asserts, over the COMMITTED
// baseline rather than over arithmetic alone, that every sentinel's PRONG (b)
// ceiling on every calibrated server is one PRONG (a) can actually let a
// measurement reach — and that the committed number is exactly what
// committedCeilingBytes derives from the committed measurement and the
// sentinel's own headroom, so a hand-edited or half-regenerated baseline
// (invariant 9) fails here rather than silently gating less than it claims.
//
// test/perf/smoke's baseline_test.go carries the same assertion over its own
// corpus. Both lanes clamp, so both are held to it: an invariant enforced in
// one of two sibling corpora is one the other is free to drift away from.
func TestNightlyBaseline_ProngBIsNeverLooserThanProngA(t *testing.T) {
	for _, version := range mustReadBaseline(t).Versions {
		set := SentinelsFor(version.ClickHouse)
		for _, sentinel := range set {
			bound, ok := baselineFor(version, sentinel.Name)
			if !ok {
				t.Errorf("%s: no committed bound for ClickHouse %s in %s — the sentinel runs there with PRONG (b) "+
					"missing entirely", sentinel.Name, version.ClickHouse, nightlyBaselinePath)
				continue
			}
			if bound.CeilingBytes > nightlyCapCeilingBytes {
				t.Errorf("%s on %s: committed ceiling %d exceeds the absolute cap-relative ceiling %d — "+
					"PRONG (a) rejects first, so PRONG (b) can never fire and this sentinel gates "+
					"with one of its two guards dead",
					sentinel.Name, version.ClickHouse, bound.CeilingBytes, nightlyCapCeilingBytes)
			}
			if want := committedCeilingBytes(bound.MaxOfNBytes, sentinel.BaselineHeadroom); bound.CeilingBytes != want {
				t.Errorf("%s on %s: committed ceiling %d is not committedCeilingBytes(%d, %v) = %d — regenerate with "+
					"`just update-nightly-perf-baseline` rather than editing the file",
					sentinel.Name, version.ClickHouse, bound.CeilingBytes, bound.MaxOfNBytes, sentinel.BaselineHeadroom, want)
			}
			if bound.ExpectedStatus != sentinel.ExpectedStatus {
				t.Errorf("%s on %s: committed bound expects status %d but the sentinel expects %d",
					sentinel.Name, version.ClickHouse, bound.ExpectedStatus, sentinel.ExpectedStatus)
			}
		}
		for _, bound := range version.Sentinels {
			if !slices.ContainsFunc(set, func(s Sentinel) bool { return s.Name == bound.Name }) {
				t.Errorf("ClickHouse %s: committed bound %q names no sentinel of that server's set", version.ClickHouse, bound.Name)
			}
		}
	}
}

// TestNightlyBaseline_CalibrationMeasurementPassesBothProngs is the other
// direction: a clamped PRONG (b) is only correct if a legitimate measurement
// still passes. The calibration-time max-of-N committed alongside each ceiling
// is the most legitimate measurement there is, so it must clear both prongs.
func TestNightlyBaseline_CalibrationMeasurementPassesBothProngs(t *testing.T) {
	for _, version := range mustReadBaseline(t).Versions {
		for _, bound := range version.Sentinels {
			if exceedsCapCeiling(bound.MaxOfNBytes) {
				t.Errorf("%s on %s: calibration measurement %d already exceeds the absolute ceiling %d — "+
					"the committed baseline records a PRONG (a) failure",
					bound.Name, version.ClickHouse, bound.MaxOfNBytes, nightlyCapCeilingBytes)
			}
			if exceedsCommittedCeiling(bound.MaxOfNBytes, bound) {
				t.Errorf("%s on %s: calibration measurement %d already exceeds its own committed ceiling %d — "+
					"the gate is red on the very run that produced it",
					bound.Name, version.ClickHouse, bound.MaxOfNBytes, bound.CeilingBytes)
			}
		}
	}
}

// TestNightlyBaseline_VersionsAreReleaseLinesInOrder pins the shape the
// calibration writer produces: one bound set per "major.minor" release line —
// the key a run looks its server up under (releaseLine) — each line once, in
// ascending order.
func TestNightlyBaseline_VersionsAreReleaseLinesInOrder(t *testing.T) {
	var prev chopt.Version
	for i, version := range mustReadBaseline(t).Versions {
		v, ok := chopt.ParseVersion(version.ClickHouse)
		if !ok || releaseLine(v) != version.ClickHouse {
			t.Fatalf("bound set %d is filed under %q, not a major.minor release line", i, version.ClickHouse)
		}
		if i > 0 && !prev.Less(v) {
			t.Fatalf("bound set %s follows %s: the sets must be in strictly ascending server order", v, prev)
		}
		prev = v
	}
}

// TestNightlyBaseline_WithVersionReplacesOnlyItsOwnSet drives the writer's
// merge: calibrating one server must leave every other server's bounds as
// they were and keep the sets ordered.
func TestNightlyBaseline_WithVersionReplacesOnlyItsOwnSet(t *testing.T) {
	old := nightlyBound{Name: "s", ExpectedStatus: 200, MaxOfNBytes: 1, CeilingBytes: 2}
	fresh := nightlyBound{Name: "s", ExpectedStatus: 200, MaxOfNBytes: 3, CeilingBytes: 4}
	base := nightlyBaseline{Versions: []nightlyVersionBaseline{
		{ClickHouse: "25.9", Sentinels: []nightlyBound{old}},
		{ClickHouse: "26.6", Sentinels: []nightlyBound{old}},
	}}

	replaced, err := base.withVersion("26.6", []nightlyBound{fresh})
	if err != nil {
		t.Fatal(err)
	}
	if got := replaced.Versions; len(got) != 2 || got[0].Sentinels[0] != old || got[1].Sentinels[0] != fresh {
		t.Fatalf("replacing 26.6 gave %+v", got)
	}

	added, err := base.withVersion("24.8", []nightlyBound{fresh})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, v := range added.Versions {
		order = append(order, v.ClickHouse)
	}
	if want := []string{"24.8", "25.9", "26.6"}; !slices.Equal(order, want) {
		t.Fatalf("adding 24.8 ordered the sets %v, want %v", order, want)
	}
	if added.Versions[1].Sentinels[0] != old || added.Versions[2].Sentinels[0] != old {
		t.Fatalf("adding 24.8 changed another server's bounds: %+v", added.Versions)
	}
}

func mustReadBaseline(t *testing.T) nightlyBaseline {
	t.Helper()
	baseline, err := readNightlyBaseline()
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(baseline.Versions) == 0 {
		t.Fatalf("%s carries no bound set", nightlyBaselinePath)
	}
	for _, version := range baseline.Versions {
		if len(version.Sentinels) == 0 {
			t.Fatalf("%s: ClickHouse %s carries no sentinel bounds", nightlyBaselinePath, version.ClickHouse)
		}
	}
	return baseline
}

// TestSentinels_NotOnNamesCalibratedServers holds every per-server exclusion
// to a server the lane runs on and to a stated reason: an exclusion naming a
// server the matrix dropped, or naming none, would silently shrink a set.
func TestSentinels_NotOnNamesCalibratedServers(t *testing.T) {
	baseline := mustReadBaseline(t)
	for _, s := range Sentinels {
		for version, reason := range s.NotOn {
			if _, ok := baseline.forVersion(version); !ok {
				t.Errorf("%s: NotOn names ClickHouse %s, which has no bound set (perf-nightly does not run there)", s.Name, version)
			}
			if reason == "" {
				t.Errorf("%s: NotOn names ClickHouse %s without a reason", s.Name, version)
			}
		}
	}
}

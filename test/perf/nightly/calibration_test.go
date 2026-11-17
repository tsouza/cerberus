package nightly

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The 26.9 label_values_matcher_stock_key_renamed peaks the cases below are
// built from: max-of-5 readings of one sentinel, unchanged code, one server.
const (
	// The two single-run calibrations, each of which committed its own max-of-N.
	labelValuesRenamedCalibrationLow  uint64 = 24_632_333
	labelValuesRenamedCalibrationHigh uint64 = 26_550_752
	// The two gating runs that went red against the first calibration's
	// ceiling, and the one that went red against the second's.
	labelValuesRenamedGatingRedFirst  uint64 = 30_693_646
	labelValuesRenamedGatingRedSecond uint64 = 30_301_384
	labelValuesRenamedGatingRedThird  uint64 = 32_643_773
	// The lowest and highest of fifteen gating runs' max-of-5.
	labelValuesRenamedGatingLowest uint64 = 25_890_318
)

const labelValuesRenamed = "label_values_matcher_stock_key_renamed"

// spreadRefusingHeadroom is a headroom whose margin the 26.9 gating runs'
// spread outgrows: 1.20x of their highest, 32.6M, leaves 6.53M against the
// 6.75M down to their lowest. The cases below that are built from those runs
// use it so they pin calibratedBound's anchoring and spread check, not the
// sentinel's own labelValuesBaselineHeadroom, which covers that spread.
const spreadRefusingHeadroom = 1.20

// runnerSets is nightlyCalibrationRunners runners that each measured rounds.
func runnerSets(rounds []uint64) [][]uint64 {
	runners := make([][]uint64, nightlyCalibrationRunners)
	for i := range runners {
		runners[i] = slices.Clone(rounds)
	}
	return runners
}

// TestCalibratedBound_LowTailRunnerCannotSetTheCeiling is the single-run
// calibration failure: one run landed in the low tail of the run-to-run
// distribution — its rounds all sat low — and the ceiling derived from it
// alone sat below what ordinary gating runs measure. calibratedBound must
// anchor the ceiling on the highest round of any runner, wherever the
// low-tail runner falls among them, so the gating runs held out of the
// calibration clear it. The low-tail runner is built around the low
// calibration's 24,632,333 and the other runners' rounds are chosen to sit
// inside spreadRefusingHeadroom's margin, so this pins the anchoring alone:
// the measured 26.9 gating spread is wider than that margin, and the next
// test shows it refused.
func TestCalibratedBound_LowTailRunnerCannotSetTheCeiling(t *testing.T) {
	const headroom = spreadRefusingHeadroom
	lowTail := []uint64{23_800_000, 24_100_000, labelValuesRenamedCalibrationLow, 24_300_000, 24_450_000}
	runners := [][]uint64{
		lowTail,
		{labelValuesRenamedCalibrationHigh, 26_100_000, 25_900_000, 26_300_000, 26_200_000},
		{29_349_973, 28_900_000, 29_100_000, 28_700_000, 29_000_000},
		{29_414_657, 29_312_309, 29_000_000, 28_800_000, 29_200_000},
	}
	const highest uint64 = 29_414_657
	heldOut := []uint64{labelValuesRenamedGatingRedFirst, labelValuesRenamedGatingRedSecond, labelValuesRenamedGatingRedThird}

	// The case is only meaningful if a ceiling derived from the low-tail
	// runner alone really is one the held-out gating runs cross.
	singleRun := nightlyBound{MaxOfNBytes: slices.Max(lowTail), CeilingBytes: committedCeilingBytes(slices.Max(lowTail), headroom)}
	for _, peak := range heldOut[:2] {
		if !exceedsCommittedCeiling(peak, singleRun) {
			t.Fatalf("gating peak %d clears the low-tail runner's own ceiling %d — the case proves nothing", peak, singleRun.CeilingBytes)
		}
	}

	for shift := range runners {
		rotated := append(slices.Clone(runners[shift:]), runners[:shift]...)
		bound, err := calibratedBound(labelValuesRenamed, http.StatusOK, rotated, headroom)
		if err != nil {
			t.Fatalf("runners %v: %v", rotated, err)
		}
		if bound.Name != labelValuesRenamed || bound.ExpectedStatus != http.StatusOK {
			t.Fatalf("runners %v: bound names %q expecting %d", rotated, bound.Name, bound.ExpectedStatus)
		}
		if bound.MaxOfNBytes != highest {
			t.Fatalf("runners %v: committed max-of-N %d, want the highest round of any runner %d", rotated, bound.MaxOfNBytes, highest)
		}
		if want := committedCeilingBytes(highest, headroom); bound.CeilingBytes != want {
			t.Fatalf("runners %v: committed ceiling %d, want committedCeilingBytes(%d, %v) = %d",
				rotated, bound.CeilingBytes, highest, headroom, want)
		}
		for _, peak := range append(slices.Concat(runners...), heldOut...) {
			if exceedsCommittedCeiling(peak, bound) {
				t.Fatalf("runners %v: ordinary peak %d exceeds the calibrated ceiling %d", rotated, peak, bound.CeilingBytes)
			}
		}
	}
}

// TestCalibratedBound_RefusesSpreadOverTheMargin pins the spread check over
// every runner's rounds together: runners whose own rounds each sit well
// inside the margin, but which between them span more than the margin the
// ceiling leaves above the highest round, are refused — the run-to-run noise
// a single runner cannot see is exactly what the check is for.
func TestCalibratedBound_RefusesSpreadOverTheMargin(t *testing.T) {
	// Four runners at the lowest, two middling and the highest of the 26.9
	// gating runs' max-of-5: 25.6M to 32.6M is a 7.04M span, and 1.20x of
	// 32.6M leaves 6.53M.
	runners := [][]uint64{
		{labelValuesRenamedGatingLowest, 25_700_000, 25_600_000, 25_800_000, 25_750_000},
		{26_900_754, 26_700_000, 26_800_000, 26_600_000, 26_850_000},
		{29_349_973, 29_100_000, 29_200_000, 29_000_000, 29_300_000},
		{labelValuesRenamedGatingRedThird, 32_400_000, 32_500_000, 32_300_000, 32_600_000},
	}
	if _, err := calibratedBound(labelValuesRenamed, http.StatusOK, runners, spreadRefusingHeadroom); err == nil {
		t.Fatalf("runners %v under a %.2fx headroom were accepted", runners, spreadRefusingHeadroom)
	}
	// The refusal is the runners' doing together: each runner's own rounds
	// span less than the margin its own highest round's ceiling would leave.
	for _, rounds := range runners {
		lowest, highest := slices.Min(rounds), slices.Max(rounds)
		if margin := committedCeilingBytes(highest, spreadRefusingHeadroom) - highest; highest-lowest >= margin {
			t.Fatalf("runner %v alone spans %d, not inside its own %d margin — the case does not isolate the cross-runner span",
				rounds, highest-lowest, margin)
		}
	}
	// The same runners under a headroom whose margin covers the span: the
	// refusal above is the span's doing, not the runners'.
	const coveringHeadroom = 1.25
	if _, err := calibratedBound(labelValuesRenamed, http.StatusOK, runners, coveringHeadroom); err != nil {
		t.Fatalf("runners %v under a %.2fx headroom: %v", runners, coveringHeadroom, err)
	}

	// The boundary: a margin exactly as wide as the span is accepted, one byte
	// narrower is refused. 1.5x of 100 is exactly 150, a 50-byte margin, and
	// the lowest and highest rounds sit on different runners.
	const headroom = 1.5
	exact := runnerSets([]uint64{60, 70, 80, 90, 90})
	exact[0][0], exact[3][4] = 50, 100
	if _, err := calibratedBound("s", http.StatusOK, exact, headroom); err != nil {
		t.Fatalf("a 50-byte span under a 50-byte margin: %v", err)
	}
	exact[0][0] = 49
	if _, err := calibratedBound("s", http.StatusOK, exact, headroom); err == nil {
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
	settled := runnerSets([]uint64{719_589_995, 722_049_382, 726_244_515, 740_000_000, 745_000_000})
	settled[2][4] = 751_191_629
	bound, err := calibratedBound("pod_status_reason_gauge", http.StatusUnprocessableEntity, settled, headroom)
	if err != nil {
		t.Fatalf("runners %v: %v", settled, err)
	}
	if bound.CeilingBytes != nightlyCapCeilingBytes || bound.ExpectedStatus != http.StatusUnprocessableEntity {
		t.Fatalf("runners %v: ceiling %d expecting %d, want the absolute ceiling %d expecting %d",
			settled, bound.CeilingBytes, bound.ExpectedStatus, nightlyCapCeilingBytes, http.StatusUnprocessableEntity)
	}

	// A 50M span: inside the 450M margin 1.5x of 900M would leave, outside the
	// 12.7M margin the clamped ceiling leaves.
	clamped := runnerSets([]uint64{850_000_000, 860_000_000, 870_000_000, 880_000_000, 900_000_000})
	if _, err := calibratedBound("s", http.StatusOK, clamped, headroom); err == nil {
		t.Fatalf("runners %v were accepted against a clamped ceiling %d", clamped, nightlyCapCeilingBytes)
	}
}

// TestCalibratedBound_RefusesRunsItCannotDeriveFrom: one runner — the shape of
// a single-run calibration — or any count short of nightlyCalibrationRunners
// samples too little of the run-to-run distribution; a runner short of
// nightlyCalibrationRounds rounds has a pass missing; a round that measured
// nothing has no peak; a highest round over the absolute ceiling already
// fails PRONG (a). Every case is built so the spread check alone would accept
// it.
func TestCalibratedBound_RefusesRunsItCannotDeriveFrom(t *testing.T) {
	const doubling = 2.0 // 2x of 100 leaves a 100-byte margin over a 100-byte span
	full := slices.Repeat([]uint64{labelValuesRenamedCalibrationLow}, nightlyCalibrationRounds)
	shortRunner := runnerSets(full)
	shortRunner[1] = shortRunner[1][:nightlyCalibrationRounds-1]
	zero := runnerSets([]uint64{100, 100, 100, 100, 100})
	zero[2][0] = 0
	for _, tc := range []struct {
		name     string
		runners  [][]uint64
		headroom float64
	}{
		{"one runner", [][]uint64{full}, labelValuesBaselineHeadroom},
		{"one runner short", runnerSets(full)[:nightlyCalibrationRunners-1], labelValuesBaselineHeadroom},
		{"a runner a round short", shortRunner, labelValuesBaselineHeadroom},
		{"a round of zero", zero, doubling},
		{"over the cap", runnerSets(slices.Repeat([]uint64{nightlyCapCeilingBytes + 1}, nightlyCalibrationRounds)), labelValuesBaselineHeadroom},
	} {
		if bound, err := calibratedBound("s", http.StatusOK, tc.runners, tc.headroom); err == nil {
			t.Errorf("%s: runners %v under %.2fx were accepted as %+v", tc.name, tc.runners, tc.headroom, bound)
		}
	}
	if _, err := calibratedBound("s", http.StatusOK, runnerSets(full), labelValuesBaselineHeadroom); err != nil {
		t.Errorf("%d runners of %d identical rounds: %v", nightlyCalibrationRunners, nightlyCalibrationRounds, err)
	}
}

// TestMergeCalibration_RefusedSentinelKeepsItsCommittedBound drives which
// bound a calibration writes for each sentinel: a new one for an accepted,
// selected sentinel; the committed one for an unselected sentinel, for one
// calibratedBound refuses, and for one no runner measured in full — the last
// two reported as refusals — and an error, not a partial file, for a
// sentinel left with no bound at all.
func TestMergeCalibration_RefusedSentinelKeepsItsCommittedBound(t *testing.T) {
	const headroom = 1.5 // 1.5x of 100 leaves a 50-byte margin
	sentinel := func(name string) Sentinel {
		return Sentinel{Name: name, ExpectedStatus: http.StatusOK, BaselineHeadroom: headroom}
	}
	committedBound := func(name string) nightlyBound {
		return nightlyBound{Name: name, ExpectedStatus: http.StatusOK, MaxOfNBytes: 7, CeilingBytes: 10}
	}
	set := []Sentinel{sentinel("accepted"), sentinel("refused"), sentinel("unmeasured"), sentinel("unselected")}
	committed := nightlyVersionBaseline{ClickHouse: "26.9", Sentinels: []nightlyBound{
		committedBound("unselected"), committedBound("unmeasured"), committedBound("refused"), committedBound("accepted"),
	}}
	refused := runnerSets([]uint64{70, 80, 90, 100, 100})
	refused[3][0] = 10
	unmeasured := runnerSets([]uint64{90, 100, 100, 100, 100})
	unmeasured[0] = nil
	runners := map[string][][]uint64{
		"accepted":   runnerSets([]uint64{60, 70, 80, 90, 100}),
		"refused":    refused,
		"unmeasured": unmeasured,
	}

	bounds, refusals, err := mergeCalibration(committed, set, runners)
	if err != nil {
		t.Fatal(err)
	}
	want := []nightlyBound{
		{Name: "accepted", ExpectedStatus: http.StatusOK, MaxOfNBytes: 100, CeilingBytes: 150},
		committedBound("refused"),
		committedBound("unmeasured"),
		committedBound("unselected"),
	}
	if !slices.Equal(bounds, want) {
		t.Fatalf("bounds %+v, want %+v", bounds, want)
	}
	if len(refusals) != 2 {
		t.Fatalf("refusals %v, want one each for the refused and the unmeasured sentinel", refusals)
	}

	// The same calibration on a server with no committed bounds: the refused
	// sentinel has nothing to keep.
	if bounds, _, err := mergeCalibration(nightlyVersionBaseline{ClickHouse: "26.9"}, set[:2], runners); err == nil {
		t.Fatalf("a refused sentinel with no committed bound was written as %+v", bounds)
	}
}

// writeRounds writes run as a rounds file under dir and returns its path.
func writeRounds(t *testing.T, dir, name string, run calibrationRun) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := writeCalibrationRun(path, run); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCombineCalibrationFiles_CombinesEveryRunnersRounds drives the combine
// perf-nightly.yml's aggregate job runs, end to end over rounds files: each
// sentinel's bound comes from every runner's rounds of its own server
// together, so neither a low-tail runner nor another server's rounds set it; a
// sentinel the runners do not name keeps its committed bound, as does every
// server no runner measured; and a refusal still writes the file, with the
// refused sentinel's committed bound kept.
func TestCombineCalibrationFiles_CombinesEveryRunnersRounds(t *testing.T) {
	committed := mustReadBaseline(t)
	const version, other = "26.9", "26.8"
	dir := t.TempDir()
	settled := []uint64{28_900_000, 29_000_000, 29_100_000, 29_200_000, 29_300_000}
	lowTail := []uint64{23_800_000, 24_100_000, labelValuesRenamedCalibrationLow, 24_300_000, 24_450_000}
	const highest uint64 = 29_414_657
	var paths []string
	for i := range nightlyCalibrationRunners {
		rounds := slices.Clone(settled)
		switch i {
		case 0:
			rounds = lowTail
		case nightlyCalibrationRunners - 1:
			rounds[0] = highest
		}
		paths = append(paths, writeRounds(t, dir, fmt.Sprintf("runner-%d.json", i), calibrationRun{
			ClickHouse: version,
			Runner:     fmt.Sprintf("runner-%d", i),
			Sentinels: []sentinelRounds{
				{Name: labelValuesRenamed, RoundMaxBytes: rounds},
				// No pass measured this one in full: refused.
				{Name: "label_values_matcher_stock_key", RoundMaxBytes: nil},
			},
		}))
	}
	// Another server's runner, far higher: it must not reach 26.9's bound.
	paths = append(paths, writeRounds(t, dir, "other.json", calibrationRun{
		ClickHouse: other,
		// The same runner name as a 26.9 file: a runner is one per server.
		Runner:    "runner-0",
		Sentinels: []sentinelRounds{{Name: labelValuesRenamed, RoundMaxBytes: slices.Repeat([]uint64{40_000_000}, nightlyCalibrationRounds)}},
	}))

	out := filepath.Join(dir, "nightly-baseline.json")
	refusals, err := CombineCalibrationFiles(nightlyBaselinePath, out, paths)
	if err != nil {
		t.Fatal(err)
	}
	// label_values_matcher_stock_key on both servers' rounds, and the other
	// server's renamed sentinel, measured by one runner only.
	if len(refusals) != 2 {
		t.Fatalf("refusals %v, want 26.9's unmeasured sentinel and 26.8's one-runner sentinel", refusals)
	}
	written, err := readNightlyBaselineFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range committed.Versions {
		got, ok := written.forVersion(v.ClickHouse)
		if !ok {
			t.Fatalf("ClickHouse %s's bound set is missing from the combined file", v.ClickHouse)
		}
		for _, want := range v.Sentinels {
			bound, _ := baselineFor(got, want.Name)
			if v.ClickHouse == version && want.Name == labelValuesRenamed {
				wantNew := nightlyBound{
					Name: labelValuesRenamed, ExpectedStatus: http.StatusOK,
					MaxOfNBytes: highest, CeilingBytes: committedCeilingBytes(highest, labelValuesBaselineHeadroom),
				}
				if bound != wantNew {
					t.Errorf("ClickHouse %s %s: %+v, want %+v", v.ClickHouse, want.Name, bound, wantNew)
				}
				continue
			}
			if bound != want {
				t.Errorf("ClickHouse %s %s: %+v, want its committed %+v", v.ClickHouse, want.Name, bound, want)
			}
		}
	}
}

// TestCombineCalibrationFiles_WritesNothingItCannotStandBehind: no rounds
// file, an unreadable one, one that names no server or no runner, one naming
// a sentinel outside its server's set, and one run's rounds passed as two
// runners all fail with no file written.
func TestCombineCalibrationFiles_WritesNothingItCannotStandBehind(t *testing.T) {
	dir := t.TempDir()
	rounds := []sentinelRounds{{Name: labelValuesRenamed, RoundMaxBytes: slices.Repeat([]uint64{29_000_000}, nightlyCalibrationRounds)}}
	unknown := writeRounds(t, dir, "unknown.json", calibrationRun{
		ClickHouse: "26.9",
		Runner:     "runner-0",
		Sentinels:  []sentinelRounds{{Name: "no_such_sentinel", RoundMaxBytes: slices.Repeat([]uint64{1}, nightlyCalibrationRounds)}},
	})
	unnamed := writeRounds(t, dir, "unnamed.json", calibrationRun{Runner: "runner-0", Sentinels: rounds})
	noRunner := writeRounds(t, dir, "no-runner.json", calibrationRun{ClickHouse: "26.9", Sentinels: rounds})
	var twice []string
	for i := range nightlyCalibrationRunners {
		// Every runner distinct but the last, which repeats the first.
		runner := fmt.Sprintf("runner-%d", i%(nightlyCalibrationRunners-1))
		twice = append(twice, writeRounds(t, dir, fmt.Sprintf("twice-%d.json", i), calibrationRun{ClickHouse: "26.9", Runner: runner, Sentinels: rounds}))
	}
	garbled := filepath.Join(dir, "garbled.json")
	if err := os.WriteFile(garbled, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, paths := range map[string][]string{
		"no rounds files":  nil,
		"a missing file":   {filepath.Join(dir, "missing.json")},
		"a garbled file":   {garbled},
		"no server":        {unnamed},
		"no runner":        {noRunner},
		"unknown sentinel": {unknown},
		"a runner twice":   twice,
	} {
		out := filepath.Join(dir, "out.json")
		if _, err := CombineCalibrationFiles(nightlyBaselinePath, out, paths); err == nil {
			t.Errorf("%s: combined without an error", name)
		}
		if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: a bound file was written (stat: %v)", name, err)
		}
	}
}

// TestCalibrationRun_RoundTripsThroughItsFile pins the rounds file's format
// the harness writes and the combine reads.
func TestCalibrationRun_RoundTripsThroughItsFile(t *testing.T) {
	run := calibrationRun{ClickHouse: "26.9", Runner: "runner-0", Sentinels: []sentinelRounds{
		{Name: labelValuesRenamed, RoundMaxBytes: []uint64{1, 2, 3, 4, 5}},
		{Name: "label_values_matcher_stock_key"},
	}}
	path := writeRounds(t, t.TempDir(), "rounds.json", run)
	got, err := readCalibrationRun(path)
	if err != nil {
		t.Fatal(err)
	}
	buf, _ := json.Marshal(got)
	want, _ := json.Marshal(run)
	if string(buf) != string(want) {
		t.Fatalf("read back %s, want %s", buf, want)
	}
}

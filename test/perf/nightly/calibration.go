// calibration.go — how the perf-nightly corpus's committed bounds are derived
// from calibration measurements taken on several independent runners.
//
// A calibration dispatch of perf-nightly.yml runs nightlyCalibrationRunners
// legs per ClickHouse server, each on its own runner. Each leg makes
// nightlyCalibrationRounds passes over the sentinel corpus
// (realch_perfnightly_integration_test.go) and writes the max-of-N peak of
// every pass, per calibrated sentinel, to a rounds file. perf-nightly.yml's
// aggregate job then hands every leg's rounds file to CombineCalibrationFiles,
// which derives each sentinel's bound from all of its runners' rounds
// together (calibratedBound) and writes nightly-baseline.json.
//
// Untagged, like baseline.go, so the derivation is asserted by the unit lane.
package nightly

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
)

// nightlyCalibrationRounds is how many max-of-N rounds each calibration
// runner measures each sentinel over. Each round is one whole pass over the
// sentinel corpus, as a gating run makes, on the runner's one server and one
// sample load, so a runner's rounds sample the variance that arises per query
// — how a multi-threaded read's per-stream buffers happen to overlap
// (docs/performance.background.md). Five rounds per runner give
// calibratedBound a measured within-run range on every runner.
const nightlyCalibrationRounds = 5

// nightlyCalibrationRunners is how many independent runners a calibration
// takes per ClickHouse server. A gating run's max-of-N varies from run to run
// by more than one run's rounds see, so a ceiling derived from one runner can
// sit wherever that run happened to land. The ceiling comes from the highest
// round of any runner, so it is set by the low tail only when every runner
// lands there: four runners make that less than one calibration in a hundred
// on the measured 26.9 spread (docs/performance.background.md).
const nightlyCalibrationRunners = 4

// calibratedBound derives one sentinel's committed bound from the max-of-N
// peaks its calibration runners measured (runners, one set of rounds per
// runner) and its per-sentinel headroom.
//
// The committed max-of-N is the HIGHEST round of any runner, so neither a
// low-tail round nor a low-tail runner can set the ceiling. The range every
// runner's rounds together span (highest minus lowest) must then fit inside
// the margin the ceiling leaves above that highest round. This is a rule, not
// a bound on the next draw: it refuses a ceiling that an ordinary run could
// reach by sitting as far above the highest round as the lowest round sits
// below it, the reach the measurements themselves demonstrated. A refused
// calibration means the sentinel's headroom is narrower than its measured
// noise; widening it is a decision about which regression the sentinel can
// still catch, and dispatching calibrations until one passes would select the
// measurements that understate the noise.
//
// Also refused: fewer than nightlyCalibrationRunners runners, a runner with
// fewer than nightlyCalibrationRounds rounds, a round that measured nothing,
// and a highest round already over the absolute ceiling (PRONG (a) fails on
// the measurement that would set the bound).
func calibratedBound(name string, expectedStatus int, runners [][]uint64, headroom float64) (nightlyBound, error) {
	if len(runners) < nightlyCalibrationRunners {
		return nightlyBound{}, fmt.Errorf("%s: measured on %d calibration runners, want at least %d — "+
			"fewer runners sample less of the run-to-run distribution the ceiling has to clear",
			name, len(runners), nightlyCalibrationRunners)
	}
	var rounds []uint64
	for i, runnerRounds := range runners {
		if len(runnerRounds) < nightlyCalibrationRounds {
			return nightlyBound{}, fmt.Errorf("%s: calibration runner %d measured %d rounds in full, want at least %d",
				name, i, len(runnerRounds), nightlyCalibrationRounds)
		}
		rounds = append(rounds, runnerRounds...)
	}
	lowest, highest := slices.Min(rounds), slices.Max(rounds)
	if lowest == 0 {
		return nightlyBound{}, fmt.Errorf("%s: a calibration round measured a peak of 0 bytes (runners %v)", name, runners)
	}
	if exceedsCapCeiling(highest) {
		return nightlyBound{}, fmt.Errorf("%s: the highest calibration round, %d bytes, exceeds the absolute ceiling %d (runners %v)",
			name, highest, nightlyCapCeilingBytes, runners)
	}
	ceiling := committedCeilingBytes(highest, headroom)
	spread := highest - lowest
	var margin uint64
	if ceiling > highest {
		margin = ceiling - highest
	}
	if margin < spread {
		return nightlyBound{}, fmt.Errorf("%s: the calibration rounds' max-of-N span %d bytes (%d to %d, runners %v), "+
			"more than the %d-byte margin the ceiling %d leaves above the highest round — an ordinary run can "+
			"cross that ceiling, so the sentinel's %.2fx headroom does not cover its run-to-run noise",
			name, spread, lowest, highest, runners, margin, ceiling, headroom)
	}
	return nightlyBound{Name: name, ExpectedStatus: expectedStatus, MaxOfNBytes: highest, CeilingBytes: ceiling}, nil
}

// mergeCalibration is the bound set a calibration writes for one server: for
// each sentinel of set, in set order, a new bound from its runners' rounds
// when the calibration selected it (it is a key of runners), and its committed
// bound (committed) otherwise. A selected sentinel calibratedBound refuses
// keeps its committed bound, and the refusal is returned so the calibration
// fails naming it while every other sentinel's new bound is still written. A
// sentinel left with no bound at all, refused or unselected without a
// committed one, is an error: no file can be written that omits it.
func mergeCalibration(committed nightlyVersionBaseline, set []Sentinel, runners map[string][][]uint64,
) (bounds []nightlyBound, refusals []error, err error) {
	bounds = make([]nightlyBound, 0, len(set))
	for _, sentinel := range set {
		if measured, selected := runners[sentinel.Name]; selected {
			bound, refusal := calibratedBound(sentinel.Name, sentinel.ExpectedStatus, measured, sentinel.BaselineHeadroom)
			if refusal == nil {
				bounds = append(bounds, bound)
				continue
			}
			refusals = append(refusals, refusal)
		}
		bound, ok := baselineFor(committed, sentinel.Name)
		if !ok {
			return nil, refusals, fmt.Errorf("%s has neither a new bound nor a committed one to keep, so no bound set can be written", sentinel.Name)
		}
		bounds = append(bounds, bound)
	}
	return bounds, refusals, nil
}

// nightlyRoundsJSONEnv names the file a calibration run writes its rounds to.
const nightlyRoundsJSONEnv = "PERF_NIGHTLY_ROUNDS_JSON"

// calibrationRun is one calibration runner's rounds file: the ClickHouse
// release line it measured and, for every sentinel the calibration selected,
// the max-of-N peak of each pass that measured it in full. A selected
// sentinel no pass measured in full is listed with no rounds, so the combine
// refuses it rather than mistaking it for one the calibration left alone.
type calibrationRun struct {
	ClickHouse string           `json:"clickhouse"`
	Sentinels  []sentinelRounds `json:"sentinels"`
}

type sentinelRounds struct {
	Name          string   `json:"name"`
	RoundMaxBytes []uint64 `json:"round_max_bytes"`
}

// combineCalibration files a new bound set into baseline for every
// ClickHouse line runs measured, each from mergeCalibration over every
// runner's rounds of that line; every other line's set is unchanged.
func combineCalibration(baseline nightlyBaseline, runs []calibrationRun) (nightlyBaseline, []error, error) {
	byVersion := make(map[string]map[string][][]uint64)
	var versions []string
	for _, run := range runs {
		if _, seen := byVersion[run.ClickHouse]; !seen {
			byVersion[run.ClickHouse] = make(map[string][][]uint64)
			versions = append(versions, run.ClickHouse)
		}
		for _, s := range run.Sentinels {
			byVersion[run.ClickHouse][s.Name] = append(byVersion[run.ClickHouse][s.Name], s.RoundMaxBytes)
		}
	}
	var refusals []error
	for _, version := range versions {
		set := SentinelsFor(version)
		for name := range byVersion[version] {
			if !slices.ContainsFunc(set, func(s Sentinel) bool { return s.Name == name }) {
				return nightlyBaseline{}, refusals, fmt.Errorf("ClickHouse %s: rounds name %q, which is not in that server's sentinel set", version, name)
			}
		}
		committed, _ := baseline.forVersion(version)
		bounds, versionRefusals, err := mergeCalibration(committed, set, byVersion[version])
		for _, r := range versionRefusals {
			refusals = append(refusals, fmt.Errorf("ClickHouse %s: %w", version, r))
		}
		if err != nil {
			return nightlyBaseline{}, refusals, fmt.Errorf("ClickHouse %s: %w", version, err)
		}
		if baseline, err = baseline.withVersion(version, bounds); err != nil {
			return nightlyBaseline{}, refusals, err
		}
	}
	return baseline, refusals, nil
}

// errNoCalibrationRuns is CombineCalibrationFiles given no rounds file.
var errNoCalibrationRuns = errors.New("no calibration rounds files given")

// CombineCalibrationFiles reads the committed bound file at baselinePath and
// every runner's rounds file, and writes the combined bound set to outPath.
// Each refusal is returned alongside a written file — the refused sentinel
// kept its committed bound; a non-nil error means nothing was written.
func CombineCalibrationFiles(baselinePath, outPath string, roundsPaths []string) (refusals []error, err error) {
	if len(roundsPaths) == 0 {
		return nil, errNoCalibrationRuns
	}
	baseline, err := readNightlyBaselineFile(baselinePath)
	if err != nil {
		return nil, err
	}
	runs := make([]calibrationRun, 0, len(roundsPaths))
	for _, path := range roundsPaths {
		run, err := readCalibrationRun(path)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	combined, refusals, err := combineCalibration(baseline, runs)
	if err != nil {
		return refusals, err
	}
	return refusals, writeNightlyBaselineFile(outPath, combined)
}

func readCalibrationRun(path string) (calibrationRun, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return calibrationRun{}, fmt.Errorf("read calibration rounds %s: %w", path, err)
	}
	var run calibrationRun
	if err := json.Unmarshal(buf, &run); err != nil {
		return calibrationRun{}, fmt.Errorf("parse calibration rounds %s: %w", path, err)
	}
	if run.ClickHouse == "" {
		return calibrationRun{}, fmt.Errorf("calibration rounds %s name no ClickHouse release line", path)
	}
	return run, nil
}

// writeCalibrationRun writes one runner's rounds file to path. Its only caller
// is the UPDATE_NIGHTLY_PERF_BASELINE=1 calibration path of the harness.
func writeCalibrationRun(path string, run calibrationRun) error {
	buf, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal calibration rounds: %w", err)
	}
	if err := os.WriteFile(path, append(buf, '\n'), 0o600); err != nil {
		return fmt.Errorf("write calibration rounds: %w", err)
	}
	return nil
}

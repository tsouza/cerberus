// results.go — the nightly gate's own per-sentinel verdict, serialised so
// perf-nightly-step-summary.mjs can render an unambiguous table in the
// workflow's $GITHUB_STEP_SUMMARY.
//
// Why this exists: 2 of the 4 sentinels in sentinels.go are CALIBRATED to
// be rejected with HTTP 422 (issue #2429) — that rejection is the correct,
// expected outcome, not a failure, and cerberus's own engine.go logs a
// WARN-level "cerberus query failed" line for every non-2xx query outcome
// unconditionally (see docs/observability.md) because a query genuinely
// failing IS a WARN-worthy event from the engine's own perspective. A human
// skimming a nightly run's raw log has no way to tell "the 2 sentinels that
// are SUPPOSED to 422" apart from "something regressed" without reading and
// correlating every WARN line by hand. This file's SentinelResult is the
// single source of truth for each sentinel's pass/fail verdict — Go decides
// it once, here, exactly where the gate's own assertions live; the .mjs
// script only renders the JSON, it never re-derives the verdict.
//
// No build tag: this file (and its marshaling) is exercised by
// results_test.go under the default build, independent of the `integration`
// build tag realch_perfnightly_integration_test.go carries (Docker
// required) — matching the same default-build-unit-coverage pattern
// loader_test.go and sentinels_test.go already established for this
// package.
package nightly

import (
	"encoding/json"
	"os"
)

// nightlyResultsJSONEnv names the environment variable
// .github/workflows/perf-nightly.yml sets to a path under $RUNNER_TEMP;
// perf-nightly-step-summary.mjs reads the exact same path from the exact
// same variable, so the two sides can never drift onto different files.
const nightlyResultsJSONEnv = "PERF_NIGHTLY_RESULTS_JSON"

// SentinelResult is one sentinel's fully-resolved outcome for one nightly
// run.
type SentinelResult struct {
	// Name / Family mirror the Sentinel this result came from.
	Name   string `json:"name"`
	Family string `json:"family"`

	// ExpectedStatus / ActualStatus / StatusOK — the THIRD check
	// realch_perfnightly_integration_test.go's own doc comment describes: a
	// status-class mismatch is itself the regression for the two
	// ExpectedStatus=422 sentinels, checked before either memory prong.
	ExpectedStatus int  `json:"expected_status"`
	ActualStatus   int  `json:"actual_status"`
	StatusOK       bool `json:"status_ok"`

	// MaxOfNBytes is the max-of-N peak memory_usage measurement (or, for a
	// correctly-rejected sentinel, the peak memory used before the
	// rejection fired).
	MaxOfNBytes uint64 `json:"max_of_n_bytes"`

	// CapCeilingBytes / CapFractionPct / CapOK — PRONG (a), the absolute
	// cap-relative ceiling (nightlyMemoryCapFraction).
	CapCeilingBytes uint64  `json:"cap_ceiling_bytes"`
	CapFractionPct  float64 `json:"cap_fraction_pct"`
	CapOK           bool    `json:"cap_ok"`

	// HasBaseline / BaselineCeilingBytes / BaselineOK — PRONG (b), the
	// committed per-sentinel ceiling. HasBaseline is false only for a
	// calibration (UPDATE_NIGHTLY_PERF_BASELINE=1) run, where PRONG (b)
	// does not apply yet.
	HasBaseline          bool   `json:"has_baseline"`
	BaselineCeilingBytes uint64 `json:"baseline_ceiling_bytes,omitempty"`
	BaselineOK           bool   `json:"baseline_ok"`

	// Pass is the single overall verdict for this sentinel: StatusOK &&
	// CapOK && (!HasBaseline || BaselineOK).
	Pass bool `json:"pass"`

	// Rejected is true for a sentinel whose ExpectedStatus != 200 — the
	// step-summary rendering uses this to label a passing 422 result
	// "correctly rejected (expected)" rather than a bare "PASS", so a
	// reader never mistakes the intentional #2429 rejection for a fluke.
	Rejected bool `json:"rejected"`
}

// NightlyResults is the top-level document written to
// PERF_NIGHTLY_RESULTS_JSON.
type NightlyResults struct {
	Sentinels []SentinelResult `json:"sentinels"`
	AllPass   bool             `json:"all_pass"`
}

// nightlyResultsPath resolves where to write the results document: the
// path named by PERF_NIGHTLY_RESULTS_JSON when set (the workflow always
// sets this), or a fixed name in os.TempDir() for a local/manual run so
// `go test -tags=integration -run TestPerfNightlyRealCH` still works
// without erroring.
func nightlyResultsPath() string {
	if p := os.Getenv(nightlyResultsJSONEnv); p != "" {
		return p
	}
	return os.TempDir() + string(os.PathSeparator) + "perf-nightly-results.json"
}

// writeResultsJSON serialises results as pretty JSON to nightlyResultsPath().
// Unlike nightly-baseline.json (invariant 9: never hand-edited, committed to
// git) this file is a per-run scratch artifact — never committed, written
// unconditionally at the end of every non-calibration run so the step
// summary has something to render even when a sentinel subtest failed
// (a Go subtest's t.Errorf/t.Fatalf aborts only that subtest's goroutine;
// the parent test function — and this write — still runs after the loop).
func writeResultsJSON(results []SentinelResult) error {
	allPass := true
	for _, r := range results {
		if !r.Pass {
			allPass = false
			break
		}
	}
	doc := NightlyResults{Sentinels: results, AllPass: allPass}
	buf, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	return os.WriteFile(nightlyResultsPath(), buf, 0o644) //nolint:gosec // per-run scratch artifact, world-readable is fine
}

package nightly

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteResultsJSON_RoundTrip asserts the results document written by
// writeResultsJSON is valid JSON at the path PERF_NIGHTLY_RESULTS_JSON
// names, and that AllPass reflects every sentinel's own Pass field exactly
// — this is the single piece of business logic
// perf-nightly-step-summary.mjs relies on Go having already decided, so a
// wrong AllPass here would make every downstream rendering wrong too.
func TestWriteResultsJSON_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "results.json")
	t.Setenv(nightlyResultsJSONEnv, path)

	results := []SentinelResult{
		{
			Name: "classic_histogram_quantile_by_route", Family: "histogram_quantile",
			ExpectedStatus: 200, ActualStatus: 200, StatusOK: true,
			MaxOfNBytes: 1234, CapCeilingBytes: 912680550, CapFractionPct: 0.1, CapOK: true,
			HasBaseline: true, BaselineCeilingBytes: 5000, BaselineOK: true,
			Pass: true, Rejected: false,
		},
		{
			Name: "request_rate_by_method", Family: "plain counter rate",
			ExpectedStatus: 422, ActualStatus: 422, StatusOK: true,
			MaxOfNBytes: 278203396, CapCeilingBytes: 912680550, CapFractionPct: 25.9, CapOK: true,
			HasBaseline: true, BaselineCeilingBytes: 417305094, BaselineOK: true,
			Pass: true, Rejected: true,
		},
	}

	if err := writeResultsJSON(results); err != nil {
		t.Fatalf("writeResultsJSON: %v", err)
	}

	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read results: %v", err)
	}
	var got NightlyResults
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal results: %v", err)
	}
	if !got.AllPass {
		t.Errorf("AllPass = false, want true (every sentinel above has Pass=true)")
	}
	if len(got.Sentinels) != len(results) {
		t.Fatalf("len(Sentinels) = %d, want %d", len(got.Sentinels), len(results))
	}
	if got.Sentinels[1].Rejected != true {
		t.Errorf("Sentinels[1].Rejected = false, want true")
	}
}

// TestWriteResultsJSON_AllPassFalseOnAnyFailure asserts a single failing
// sentinel flips the document-level AllPass to false, even when every
// other sentinel passed — the step summary's headline verdict line reads
// this field directly.
func TestWriteResultsJSON_AllPassFalseOnAnyFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "results.json")
	t.Setenv(nightlyResultsJSONEnv, path)

	results := []SentinelResult{
		{Name: "a", Pass: true},
		{Name: "b", Pass: false},
		{Name: "c", Pass: true},
	}
	if err := writeResultsJSON(results); err != nil {
		t.Fatalf("writeResultsJSON: %v", err)
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read results: %v", err)
	}
	var got NightlyResults
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal results: %v", err)
	}
	if got.AllPass {
		t.Errorf("AllPass = true, want false (Sentinels[1].Pass is false)")
	}
}

// TestNightlyResultsPath_DefaultsWhenEnvUnset asserts a local run (no
// PERF_NIGHTLY_RESULTS_JSON set) still resolves to a usable path rather
// than an empty string, so writeResultsJSON never silently no-ops.
func TestNightlyResultsPath_DefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv(nightlyResultsJSONEnv, "")
	if got := nightlyResultsPath(); got == "" {
		t.Errorf("nightlyResultsPath() = %q, want a non-empty default", got)
	}
}

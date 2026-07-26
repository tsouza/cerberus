package lib

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSuiteFormatWithoutARequestKeepsThePrettyFormatterAlone asserts the
// no-report path: a bare `go test` during local development produces the same
// human-readable output it always did, and writes no artifact nobody reads.
func TestSuiteFormatWithoutARequestKeepsThePrettyFormatterAlone(t *testing.T) {
	t.Setenv(EnvSuiteReport, "")

	got, err := SuiteFormat()
	if err != nil {
		t.Fatalf("SuiteFormat with no %s set: %v", EnvSuiteReport, err)
	}
	if got != prettyFormat {
		t.Fatalf("SuiteFormat = %q, want %q", got, prettyFormat)
	}
}

// TestSuiteFormatAddsTheCucumberReportWithoutDroppingPretty asserts the
// formatter is ADDED, not substituted: the job log keeps its per-step output
// (the only diagnosis a red runner leaves behind) while the attestation gate
// gets its machine-readable report.
func TestSuiteFormatAddsTheCucumberReportWithoutDroppingPretty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reports", "tier0.cucumber.json")
	t.Setenv(EnvSuiteReport, path)

	got, err := SuiteFormat()
	if err != nil {
		t.Fatalf("SuiteFormat: %v", err)
	}
	want := prettyFormat + "," + cucumberFormat + ":" + path
	if got != want {
		t.Fatalf("SuiteFormat = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, prettyFormat+",") {
		t.Fatalf("SuiteFormat dropped the pretty formatter: %q", got)
	}
	// godog opens the report with os.Create, which does not create parents —
	// a missing directory would fail the whole suite at Summary() time, long
	// after the scenarios have run.
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("SuiteFormat did not create the report's parent directory: %v", err)
	}
}

// TestSuiteFormatRefusesARelativeReportPath pins the reason MODE=run resolves
// the path before exporting it: `go test` runs a package's test binary with
// the PACKAGE directory as its working directory, so a relative path would
// write the report next to the tier's test file instead of where the caller
// asked. Writing it somewhere nobody looks is worse than refusing.
func TestSuiteFormatRefusesARelativeReportPath(t *testing.T) {
	t.Setenv(EnvSuiteReport, "build/migration-reports/tier0.cucumber.json")

	if _, err := SuiteFormat(); err == nil {
		t.Fatal("SuiteFormat accepted a relative report path")
	}
}

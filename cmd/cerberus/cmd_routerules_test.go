package main

import (
	"os"
	"strings"
	"testing"
)

// runCapture invokes routeRulesRun with temp files for stdout/stderr and returns
// their contents plus the error, so a test can assert on the CLI's real output
// the way an operator would see it.
func runCapture(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	outF, e := os.CreateTemp(t.TempDir(), "out")
	if e != nil {
		t.Fatalf("temp out: %v", e)
	}
	errF, e := os.CreateTemp(t.TempDir(), "err")
	if e != nil {
		t.Fatalf("temp err: %v", e)
	}
	err = routeRulesRun(args, outF, errF)
	_ = outF.Close()
	_ = errF.Close()
	ob, _ := os.ReadFile(outF.Name())
	eb, _ := os.ReadFile(errF.Name())
	return string(ob), string(eb), err
}

// TestBenchmarkSubcommandE2E is the CLI end-to-end proof: `route-rules benchmark`
// runs the embedded catalog over the generated labeled corpus and prints the
// per-rule precision/recall/F1 table with an OVERALL row. It exercises the same
// path an operator or CI invokes, no corpus file required.
func TestBenchmarkSubcommandE2E(t *testing.T) {
	stdout, stderr, err := runCapture(t, "benchmark", "--seed", "1")
	if err != nil {
		t.Fatalf("benchmark subcommand failed: %v (stderr=%s)", err, stderr)
	}
	for _, want := range []string{
		"detection-fidelity benchmark",
		"RULE", "PRECISION", "RECALL", "F1",
		"oom_on_route_a",
		"cerberus_side_rejection_pressure",
		"OVERALL (micro)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("benchmark output missing %q:\n%s", want, stdout)
		}
	}
}

// TestBenchmarkSubcommandDeterministic confirms the benchmark is reproducible:
// the same seed yields byte-identical output across runs.
func TestBenchmarkSubcommandDeterministic(t *testing.T) {
	a, _, err := runCapture(t, "benchmark", "--seed", "5")
	if err != nil {
		t.Fatalf("run a: %v", err)
	}
	b, _, err := runCapture(t, "benchmark", "--seed", "5")
	if err != nil {
		t.Fatalf("run b: %v", err)
	}
	if a != b {
		t.Errorf("benchmark output is not deterministic for a fixed seed:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
}

// TestBenchmarkSubcommandParamOverride confirms --min-support flows into the
// scored config: a tiny floor admits thin classes (more findings counted), a
// realistic floor does not, so the two runs differ.
func TestBenchmarkSubcommandParamOverride(t *testing.T) {
	tiny, _, err := runCapture(t, "benchmark", "--seed", "1", "--min-support", "1")
	if err != nil {
		t.Fatalf("tiny floor: %v", err)
	}
	nominal, _, err := runCapture(t, "benchmark", "--seed", "1", "--min-support", "5")
	if err != nil {
		t.Fatalf("nominal floor: %v", err)
	}
	if tiny == nominal {
		t.Error("min-support override had no effect on the benchmark output")
	}
}

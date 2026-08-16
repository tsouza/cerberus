package regression

import (
	"os"
	"strings"
	"testing"
)

// TestCoverageRecipeFailsClosedOnGoTestFailure pins the coverage profile to a
// successful test run. Before #2089, both go test pipelines ended in `|| true`,
// so an entire package could fail while its partial profile still reached the
// floor comparison as apparently valid evidence.
func TestCoverageRecipeFailsClosedOnGoTestFailure(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	justfile := string(buf)
	start := strings.Index(justfile, "\ncoverage:\n")
	end := strings.Index(justfile, "\nupdate-coverage-floor:")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("cannot isolate the coverage recipe in Justfile")
	}
	recipe := justfile[start:end]

	if got := strings.Count(recipe, "go test -timeout"); got != 2 {
		t.Fatalf("coverage recipe carries %d go test runs, want default and chdb-tagged runs", got)
	}
	if strings.Contains(recipe, "|| true") {
		t.Fatalf("coverage recipe tolerates a failed command with `|| true`; a partial profile is not valid evidence")
	}
	if !strings.Contains(justfile, `set shell := ["bash", "-eu", "-o", "pipefail", "-c"]`) {
		t.Fatalf("Justfile shell does not enable pipefail; a failed go test before the output-filter pipe would be hidden")
	}
}

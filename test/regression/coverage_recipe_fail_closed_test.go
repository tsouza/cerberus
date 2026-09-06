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
//
// tsouza/cerberus#2634 split the old single `coverage` recipe's two `go test`
// pipelines into their own `coverage-default` / `coverage-chdb` recipes (so
// CI can shard them across parallel jobs); `coverage-merge` folds the two
// profiles and `coverage` chains all three for local use. The isolated span
// below covers exactly those three recipes, by name, rather than the chaining
// `coverage:` recipe, which contains no `go test` invocation itself.
//
// Reads via justDump() (#3093) for the three recipe bodies — they live in
// just/test.just now, not the root Justfile — but the pipefail check stays a
// direct root-Justfile read: `set shell := [...]` is a per-invocation `just`
// SETTING, which can only be declared once, in the root file itself.
func TestCoverageRecipeFailsClosedOnGoTestFailure(t *testing.T) {
	t.Parallel()

	d := justDump(t)
	var recipe strings.Builder
	for _, name := range []string{"coverage-default", "coverage-chdb", "coverage-merge"} {
		recipe.WriteString(d.recipe(t, name).bodyText(t))
		recipe.WriteString("\n")
	}
	body := recipe.String()

	if got := strings.Count(body, "go test -timeout"); got != 2 {
		t.Fatalf("coverage recipes carry %d go test runs, want default and chdb-tagged runs", got)
	}
	if strings.Contains(body, "|| true") {
		t.Fatalf("coverage recipes tolerate a failed command with `|| true`; a partial profile is not valid evidence")
	}

	rootJustfile, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	if !strings.Contains(string(rootJustfile), `set shell := ["bash", "-eu", "-o", "pipefail", "-c"]`) {
		t.Fatalf("Justfile shell does not enable pipefail; a failed go test before the output-filter pipe would be hidden")
	}
}

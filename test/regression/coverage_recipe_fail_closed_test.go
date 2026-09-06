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
// profiles and `coverage` chains all three for local use.
//
// tsouza/cerberus#3113 then extracted the chdb-tagged `go test` invocation
// itself out of the coverage-chdb recipe body into coverage-chdb.mjs's own
// source (mirroring coverage-merge's own #3095 extraction) — so the
// evidence for that HALF of this test moved from `just --dump`'s recipe-body
// text to the script's source: it still must declare an explicit -timeout
// and must not tolerate a failed run. The default-tag lane's `go test`
// invocation is unchanged and is still read via justDump() (#3093); the
// pipefail check stays a direct root-Justfile read: `set shell := [...]` is
// a per-invocation `just` SETTING, which can only be declared once, in the
// root file itself.
func TestCoverageRecipeFailsClosedOnGoTestFailure(t *testing.T) {
	t.Parallel()

	d := justDump(t)
	var recipe strings.Builder
	for _, name := range []string{"coverage-default", "coverage-merge"} {
		recipe.WriteString(d.recipe(t, name).bodyText(t))
		recipe.WriteString("\n")
	}
	body := recipe.String()

	if got := strings.Count(body, "go test -timeout"); got != 1 {
		t.Fatalf("coverage-default/coverage-merge recipes carry %d literal go test run(s), want exactly the default-tag lane's", got)
	}
	if strings.Contains(body, "|| true") {
		t.Fatalf("coverage recipes tolerate a failed command with `|| true`; a partial profile is not valid evidence")
	}

	scriptSource, err := os.ReadFile("../../.github/scripts/coverage-chdb.mjs")
	if err != nil {
		t.Fatalf("read coverage-chdb.mjs: %v", err)
	}
	script := string(scriptSource)
	if !strings.Contains(script, "'-timeout', `${MAIN_SWEEP_TIMEOUT_MINUTES}m`,") {
		t.Fatal("coverage-chdb.mjs's main sweep no longer declares an explicit go test -timeout")
	}
	if strings.Contains(script, "|| true") || strings.Contains(script, ".catch(") {
		t.Fatal("coverage-chdb.mjs tolerates a failed command; a partial profile is not valid evidence")
	}
	if !strings.Contains(script, "if (code !== 0) return code;") {
		t.Fatal("coverage-chdb.mjs no longer propagates its go test sweep's exit code")
	}

	rootJustfile, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	if !strings.Contains(string(rootJustfile), `set shell := ["bash", "-eu", "-o", "pipefail", "-c"]`) {
		t.Fatalf("Justfile shell does not enable pipefail; a failed go test before the output-filter pipe would be hidden")
	}
}

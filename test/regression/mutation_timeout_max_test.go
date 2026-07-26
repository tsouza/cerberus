package regression

import (
	"os"
	"strings"
	"testing"
)

// mutationWorkflowPath drives the gremlins mutation-testing lane.
const mutationWorkflowPath = "../../.github/workflows/mutation.yml"

// timeoutMaxFlag bounds a single mutant's test run regardless of how slow the
// mutated package's own tests are.
const timeoutMaxFlag = "--timeout-max"

// unleashArgvPrefix opens the argv array the workflow hands to gremlins.
const unleashArgvPrefix = "args=(unleash"

// gremlinsForkTagPrefix is the fork tag family that supports timeoutMaxFlag.
// The flag landed in the fork; a tag from before it makes gremlins reject the
// argument outright.
const gremlinsForkTagPrefix = "github.com/tsouza/gremlins/cmd/gremlins@v0.6.0-cerberus-timeout-max"

// TestMutationLaneCapsPerMutantTimeout pins the one bound that keeps the
// mutation lane from killing its own runner.
//
// gremlins derives a mutant's test timeout as `timeout-coefficient x the
// package's baseline test duration`. That scales the leash by how SLOW the
// package's tests are, which has nothing to do with how much damage a runaway
// mutant does in that time. Mutating a loop-advance statement (i++ -> i--)
// inside a scanner loop whose body appends per iteration yields a mutant that
// never terminates and allocates until the runner is out of memory; the OOM
// killer then reaps the runner agent and the job dies with no verdict at all.
//
// Measured over 91 heavy runs on 2026-07-26: 55 of 55 runner deaths were
// stalled on a lexer/scanner mutant, and the rate on the worst leg was 4 of the
// last 13 runs. --timeout-max removes the race by bounding exposure
// independently of the baseline.
//
// The pin matters because the failure it prevents does not look like a missing
// flag — it looks like flake. Without the bound the lane goes red roughly one
// push in three, on a different leg each time, and the tempting fix is to
// exclude whichever file the log names. That was tried twice and made it worse:
// excluding a file relocates its runaway mutants into whichever leg still owns
// it and burns real mutation coverage. So the flag and the fork tag that
// understands it are pinned together.
func TestMutationLaneCapsPerMutantTimeout(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(mutationWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", mutationWorkflowPath, err)
	}
	body := string(workflow)

	// Match the argv line itself, not the file: the flag is also named in the
	// comments that explain it, and prose must not be able to satisfy the pin.
	var argvLine string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, unleashArgvPrefix) {
			argvLine = line

			break
		}
	}
	switch {
	case argvLine == "":
		t.Errorf("%s has no %q line; the pin below cannot see how gremlins is invoked. If the "+
			"invocation was restructured, re-point this test at the new shape rather than deleting it.",
			mutationWorkflowPath, unleashArgvPrefix)
	case !strings.Contains(argvLine, timeoutMaxFlag):
		t.Errorf("%s does not pass %s to gremlins unleash. Without an absolute ceiling a mutant that "+
			"inverts a scanner loop-advance runs until the runner is out of memory, and the job dies "+
			"with no verdict — which reads as flake, not as a missing bound.",
			mutationWorkflowPath, timeoutMaxFlag)
	}

	if !strings.Contains(body, gremlinsForkTagPrefix) {
		t.Errorf("%s does not install a %s* build of gremlins. Earlier fork tags reject %s as an "+
			"unknown flag, so the lane would fail on every leg rather than run unbounded — loud, but "+
			"still broken.", mutationWorkflowPath, gremlinsForkTagPrefix, timeoutMaxFlag)
	}
}

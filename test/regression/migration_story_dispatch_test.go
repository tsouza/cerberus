package regression

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The migration lane's `story` dispatch input narrows a manual run to ONE MIG
// id, so an operator debugging a single story does not pay for the full
// three-tier substrate. The narrowing is per-tier: each tier job's MODE=run
// step forwards STORY, and migration-e2e.mjs turns it into the godog tag
// filter `@<story> && @<tier>` plus a hard error when that story has no
// scenario at that tier.
//
// Forwarding it is therefore not a nicety — a tier job that omits STORY runs
// its ENTIRE scenario set on a narrowed dispatch, and a story that tier does
// not carry runs everything instead of failing loudly. Tier-2 shipped without
// the line while tier-0 and tier-1 had it, which is exactly the drift a
// hand-repeated env block invites; this pin is the gate rather than a fourth
// hand-copied line.
const migrationStoryEnv = "STORY"

// migrationRunModeEnv marks the steps the pin applies to. MODE=run is the only
// mode that EXECUTES scenarios; emit / verify / attest / rollup read the
// enumeration or the reports, and attest forwards STORY on its own account
// (narrowing what it holds to have executed), so keying on MODE keeps the pin
// pointed at the drift it exists to catch.
const migrationRunModeEnv = "run"

// workflowSteps is the slice of the Actions schema this pin reads: job names,
// step names, and each step's env block. Everything else in the workflow —
// triggers, `uses`, `with`, matrices — is deliberately absent so an unrelated
// edit cannot break the parse.
type workflowSteps struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string            `yaml:"name"`
			Env  map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func TestMigrationLaneForwardsStoryToEveryTierRun(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile(migrationWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", migrationWorkflowPath, err)
	}
	var wf workflowSteps
	if err := yaml.Unmarshal(buf, &wf); err != nil {
		t.Fatalf("parse %s: %v", migrationWorkflowPath, err)
	}

	var runSteps int
	for job, spec := range wf.Jobs {
		for _, step := range spec.Steps {
			if step.Env["MODE"] != migrationRunModeEnv {
				continue
			}
			runSteps++
			story, ok := step.Env[migrationStoryEnv]
			if !ok {
				t.Errorf("%s: job %q step %q runs scenarios (MODE=%s) but does not forward %s. "+
					"A `story`-narrowed dispatch would run this tier's whole scenario set instead of the one "+
					"story asked for, and a story this tier does not carry would run everything rather than "+
					"failing. Add `%s: ${{ inputs.story }}` alongside TIER.",
					migrationWorkflowPath, job, step.Name, migrationRunModeEnv, migrationStoryEnv, migrationStoryEnv)
				continue
			}
			if !strings.Contains(story, "inputs.story") {
				t.Errorf("%s: job %q step %q sets %s to %q, which is not the dispatch input. "+
					"The narrowing has exactly one source: `${{ inputs.story }}`.",
					migrationWorkflowPath, job, step.Name, migrationStoryEnv, story)
			}
		}
	}

	// Without this the pin passes vacuously the day the workflow renames MODE
	// or restructures the tier jobs — the failure mode a shape assertion over
	// someone else's file is most exposed to.
	if runSteps == 0 {
		t.Fatalf("%s declares no MODE=%s step; the tier jobs are the lane's whole point, so this pin "+
			"is reading the wrong file or the wrong schema rather than passing",
			migrationWorkflowPath, migrationRunModeEnv)
	}
}

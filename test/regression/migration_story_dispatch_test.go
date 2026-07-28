package regression

import (
	"fmt"
	"os"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

// The migration lane's `story` dispatch input narrows a manual run to ONE MIG
// id, so an operator debugging a single story does not pay for the full
// three-tier substrate. The narrowing is per-tier: each tier job's
// scenario-executing step forwards STORY, and migration-e2e.mjs turns it into
// the godog tag filter `@<story> && @<tier>` plus a hard error when that story
// has no scenario at that tier.
//
// Forwarding it is therefore not a nicety — a tier job that omits STORY runs
// its ENTIRE scenario set on a narrowed dispatch, and a story that tier does
// not carry runs everything instead of failing loudly. Tier-2 shipped without
// the line while tier-0 and tier-1 had it, which is exactly the drift a
// hand-repeated env block invites; this pin is the gate rather than a fourth
// hand-copied line.
//
// The pin is driven per-TIER, not per-step-shape: the tier set comes from
// migration-e2e.mjs's TIER_JOBS — the same table emit derives the tier list
// from — and every tier in it must have a job with a step pinned to that
// tier that forwards the input. Keying on a step's MODE or `run:` line would
// let the drift back in through the door it came from, because a tier whose
// step stops matching the shape simply stops being inspected.
const migrationStoryEnv = "STORY"

// migrationTierEnv names the env var that pins a step to one tier's scenario
// slice. It is what makes a step the tier's executing step: migration-e2e.mjs
// reads it to build the `@<tier>` half of the tag filter, so a step carrying
// `TIER: <tier>` inside that tier's job is running that tier's scenarios
// whatever the surrounding plumbing (MODE env, `just` recipe, direct `node`
// invocation) currently looks like.
const migrationTierEnv = "TIER"

// migrationStoryExpr is the ONLY accepted value for STORY. The narrowing has
// exactly one source, so this is an equality check rather than a substring
// one: `${{ inputs.stories }}` and `${{ inputs.story }}-x` are different
// inputs that would silently narrow to something else.
const migrationStoryExpr = "${{ inputs.story }}"

// migrationTierJobFmt is the workflow's tier-job naming convention — the
// TIER_JOBS comment in migration-e2e.mjs names the jobs `migration-tier0` /
// `migration-tier1` / `migration-tier2` on exactly this shape.
const migrationTierJobFmt = "migration-%s"

// migrationScriptPath holds TIER_JOBS: the one table binding a tier to a CI
// job, and what emit filters the run's tier list out of.
const migrationScriptPath = "../../.github/scripts/migration-e2e.mjs"

// workflowSteps is the slice of the Actions schema this pin reads: job names,
// job-level env, step names, and each step's env block. Everything else in the
// workflow — triggers, `uses`, `with`, matrices — is deliberately absent so an
// unrelated edit cannot break the parse. Job-level env is read because Actions
// merges it under each step's own block, so hoisting TIER or STORY up a level
// is a refactor the pin must see through rather than be blinded by.
type workflowSteps struct {
	Jobs map[string]struct {
		Env   map[string]string `yaml:"env"`
		Steps []struct {
			Name string            `yaml:"name"`
			Env  map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

var (
	migrationTierJobsBlockRe = regexp.MustCompile(`(?s)export const TIER_JOBS = \{(.*?)\n\};`)
	migrationTierJobsKeyRe   = regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*:\s*\{`)
)

// migrationTierJobTiers returns the tier ids TIER_JOBS declares a CI job for,
// in declaration order.
func migrationTierJobTiers(t *testing.T) []string {
	t.Helper()

	buf, err := os.ReadFile(migrationScriptPath)
	if err != nil {
		t.Fatalf("read %s: %v", migrationScriptPath, err)
	}
	block := migrationTierJobsBlockRe.FindSubmatch(buf)
	if block == nil {
		t.Fatalf("%s declares no `export const TIER_JOBS = {…};` table; it is the tier→job binding "+
			"this pin and emit both read, so a rename here silently unbinds both", migrationScriptPath)
	}
	var tiers []string
	for _, m := range migrationTierJobsKeyRe.FindAllSubmatch(block[1], -1) {
		tiers = append(tiers, string(m[1]))
	}
	if len(tiers) == 0 {
		t.Fatalf("%s declares TIER_JOBS with no tier in it; the lane has nothing to run, so this pin "+
			"is reading the wrong shape rather than passing", migrationScriptPath)
	}
	return tiers
}

// stepEnv merges a job's env block with a step's, step wins — the precedence
// Actions itself applies.
func stepEnv(job, step map[string]string) map[string]string {
	merged := make(map[string]string, len(job)+len(step))
	for k, v := range job {
		merged[k] = v
	}
	for k, v := range step {
		merged[k] = v
	}
	return merged
}

func TestMigrationLaneForwardsStoryToEveryTierRun(t *testing.T) {
	t.Parallel()

	tiers := migrationTierJobTiers(t)

	buf, err := os.ReadFile(migrationWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", migrationWorkflowPath, err)
	}
	var wf workflowSteps
	if err := yaml.Unmarshal(buf, &wf); err != nil {
		t.Fatalf("parse %s: %v", migrationWorkflowPath, err)
	}

	for _, tier := range tiers {
		jobName := fmt.Sprintf(migrationTierJobFmt, tier)
		job, ok := wf.Jobs[jobName]
		if !ok {
			t.Errorf("%s declares no %q job, but %s binds tier %q to one in TIER_JOBS. "+
				"Either the job was renamed (update %s alongside it) or the tier ships without a lane.",
				migrationWorkflowPath, jobName, migrationScriptPath, tier, migrationTierJobFmt)
			continue
		}

		var tierSteps int
		for _, step := range job.Steps {
			env := stepEnv(job.Env, step.Env)
			if env[migrationTierEnv] != tier {
				continue
			}
			tierSteps++
			story, ok := env[migrationStoryEnv]
			if !ok {
				t.Errorf("%s: job %q step %q runs tier %q's scenarios (%s: %s) but does not forward %s. "+
					"A `story`-narrowed dispatch would run this tier's whole scenario set instead of the one "+
					"story asked for, and a story this tier does not carry would run everything rather than "+
					"failing. Add `%s: %s` alongside %s.",
					migrationWorkflowPath, jobName, step.Name, tier, migrationTierEnv, tier, migrationStoryEnv,
					migrationStoryEnv, migrationStoryExpr, migrationTierEnv)
				continue
			}
			if story != migrationStoryExpr {
				t.Errorf("%s: job %q step %q sets %s to %q, which is not the dispatch input. "+
					"The narrowing has exactly one source: %s.",
					migrationWorkflowPath, jobName, step.Name, migrationStoryEnv, story, migrationStoryExpr)
			}
		}

		// Per-tier anti-vacuity. Without it the pin passes the day a tier job
		// drops or renames the env that pins its step to a tier — the failure
		// mode a shape assertion over someone else's file is most exposed to,
		// and the one that let tier-2 ship unnarrowed in the first place.
		if tierSteps == 0 {
			t.Errorf("%s job %q has no step carrying %s: %s, so this pin cannot see which step executes "+
				"tier %q's scenarios and would pass whether or not it forwards %s. Pin the executing step "+
				"to its tier with `%s: %s`.",
				migrationWorkflowPath, jobName, migrationTierEnv, tier, tier, migrationStoryEnv,
				migrationTierEnv, tier)
		}
	}
}

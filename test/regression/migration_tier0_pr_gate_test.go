package regression

// Pins the Tier-0 migration scenarios to a required pull-request lane.
//
// The Tier-0 slice asserts, byte for byte, that `cerberus migrate` still
// emits the committed explain goldens under
// test/e2e/migration/archetypes/*/expected/explain.txt. Those goldens are
// the migration lane's only assertion over generated SQL, which makes them
// the tripwire for any change that rewrites a query shape.
//
// The tripwire was wired to the wrong side of the merge. migration-e2e.yml
// deliberately carries no `pull_request:` trigger, so the goldens were
// checked only on push-to-main — and three emitter changes that wrapped
// Map-valued identity keys in `mapSort(...)` merged green, each leaving the
// goldens further behind, until `main` went red. release.yml's preflight
// requires migration-e2e green on the exact commit being published, so a
// red `main` is a blocked release, and the diagnosis lands on whoever cuts
// it rather than on the change that moved the SQL.
//
// `go vet -tags=migration` already runs on every PR, but vetting only
// type-checks the runner: a golden can drift arbitrarily far while the
// harness compiles perfectly. Executing the Tier-0 suite is what closes the
// gap, and it is cheap enough to do on every PR — committed fixtures, no
// Docker, no reference backend, no network.
//
// This test pins the property that matters (the goldens are *executed* on a
// PR, not merely compiled) rather than the exact command spelling, so the
// step can be rephrased freely but not quietly downgraded back to a vet.

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	// The workflow + job that carry the required PR-blocking checks. The
	// lane must be one branch protection actually requires; moving the step
	// to an unrequired job would restore the hole this guards.
	tier0GateWorkflow = "../../.github/workflows/ci.yml"
	tier0GateJob      = "lint"

	// The build tag the Tier-0 runner sits behind, and the package holding
	// it. A step naming both is running these scenarios and no others.
	tier0BuildTag = "-tags=migration"
	tier0Package  = "test/e2e/migration/tiers/tier0-offline"
)

// tier0LintSteps returns the steps of the required lane that owns the gate.
func tier0LintSteps(t *testing.T) []workflowStep {
	t.Helper()

	src, err := os.ReadFile(tier0GateWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", tier0GateWorkflow, err)
	}

	var doc workflowDoc
	if err := yaml.Unmarshal(src, &doc); err != nil {
		t.Fatalf("parse %s: %v", tier0GateWorkflow, err)
	}

	job, ok := doc.Jobs[tier0GateJob]
	if !ok {
		t.Fatalf("%s declares no %q job; the required PR lane was renamed and the "+
			"Tier-0 explain-golden gate no longer runs on pull requests",
			tier0GateWorkflow, tier0GateJob)
	}
	return job.Steps
}

// TestTier0ExplainGoldensRunOnPullRequests fails if the required lane stops
// executing the Tier-0 scenarios — the state in which an emitter change can
// merge green and turn `main` red.
func TestTier0ExplainGoldensRunOnPullRequests(t *testing.T) {
	for _, step := range tier0LintSteps(t) {
		run := step.Run
		if !strings.Contains(run, tier0Package) || !strings.Contains(run, tier0BuildTag) {
			continue
		}
		// `go vet` over the same package is the state this guards against:
		// it compiles the runner without ever comparing a golden.
		if !strings.Contains(run, "go test") {
			continue
		}
		return
	}

	t.Fatalf("no step in %s job %q runs `go test %s ./%s/...`.\n"+
		"The Tier-0 explain goldens are the only assertion over emitted SQL in the "+
		"migration lane, and that lane has no pull_request trigger — without this "+
		"step a change to a query shape merges green and turns main red, blocking "+
		"the release that needs migration-e2e green.",
		tier0GateWorkflow, tier0GateJob, tier0BuildTag, tier0Package)
}

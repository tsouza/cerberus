//go:build migration_tier2

// Package tier2ruler runs the Tier-2 Gherkin slice of the Layer-14 migration
// scenarios: a godog suite over test/e2e/migration/features, filtered to
// @tier2, against the live ruler substrate `just migration-tier2-up` brings
// up (see ../../tier2_ruler_test.go for the substrate self-check this
// mechanism rests on — that file proves Grafana can evaluate a rule against
// cerberus and a fired notification reaches the dead-end receiver; this one
// drives the same substrate through the MIG-09/MIG-24 story scenarios).
//
// Not every scenario this suite runs needs the live stack: MIG-24 folds
// purely offline artifacts, the same way a Tier-0 scenario does, and is
// tagged @tier2 only because docs/migration-testing.md's story table scopes
// it there. Registering every step (offline and live) on one World, the same
// way ../../steps/world.go already does for Tier-0, means a scenario simply
// never calls the steps it does not need — see steps/world.go's
// InitializeScenario for the full registration list.
//
// The `migration_tier2` build tag keeps this suite out of `just test`, which
// must not pay for Docker. `go vet -tags=migration_tier2` type-checks this
// package on every pull request; the migration lane runs it against a real
// stack.
package tier2ruler

import (
	"os"
	"testing"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/test/e2e/migration/lib"
	"github.com/tsouza/cerberus/test/e2e/migration/steps"
)

// featurePath is the feature directory, relative to this package.
const featurePath = "../../features"

// defaultTags restricts the suite to the ruler tier.
const defaultTags = "@tier2"

// tagsEnv overrides defaultTags so the migration lane can drive one story.
const tagsEnv = "MIGRATION_TAGS"

// suiteConcurrency is fixed at one: the World is a single shared value that
// each scenario's Before hook resets, so scenarios must not interleave.
const suiteConcurrency = 1

func TestTier2(t *testing.T) {
	root, err := lib.RepoRoot()
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	bin, err := lib.BuildCerberus(root, t.TempDir())
	if err != nil {
		t.Fatalf("build the cerberus binary the offline scenarios drive: %v", err)
	}

	tags := os.Getenv(tagsEnv)
	if tags == "" {
		tags = defaultTags
	}

	world := steps.NewWorld(root, bin)
	suite := godog.TestSuite{
		Name:                "migration-tier2",
		ScenarioInitializer: world.InitializeScenario,
		Options: &godog.Options{
			Format: "pretty",
			Output: os.Stdout,
			Paths:  []string{featurePath},
			Tags:   tags,
			// Strict fails the suite on an undefined, pending or ambiguous
			// step — see tier0_test.go's identical comment for why.
			Strict:      true,
			Concurrency: suiteConcurrency,
			TestingT:    t,
		},
	}

	if status := suite.Run(); status != 0 {
		t.Fatalf("migration tier-2 suite failed with status %d (tags %q)", status, tags)
	}
	if world.ScenariosRun() == 0 {
		t.Fatalf("the tier-2 suite ran no scenario for tags %q — the feature tree matched nothing", tags)
	}
}

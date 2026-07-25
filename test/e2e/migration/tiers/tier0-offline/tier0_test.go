//go:build migration

// Package tier0 runs the Tier-0 offline slice of the Layer-14 migration
// scenarios: a godog suite over test/e2e/migration/features, filtered to
// @tier0, against committed fixtures. No Docker, no reference backend, no
// network — every command is `cerberus migrate …` reading files on disk.
//
// The `migration` build tag keeps the suite out of `just test`, which must not
// pay for building and exec'ing a binary. `go vet -tags=migration` type-checks
// this package on every pull request, and the migration lane runs it.
package tier0

import (
	"os"
	"testing"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/test/e2e/migration/lib"
	"github.com/tsouza/cerberus/test/e2e/migration/steps"
)

// featurePath is the feature directory, relative to this package.
const featurePath = "../../features"

// defaultTags restricts the suite to the offline tier.
const defaultTags = "@tier0"

// tagsEnv overrides defaultTags so the migration lane can drive one story.
const tagsEnv = "MIGRATION_TAGS"

// suiteConcurrency is fixed at one: the World is a single shared value that
// each scenario's Before hook resets, so scenarios must not interleave.
const suiteConcurrency = 1

func TestTier0(t *testing.T) {
	root, err := lib.RepoRoot()
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	bin, err := lib.BuildCerberus(root, t.TempDir())
	if err != nil {
		t.Fatalf("build the cerberus binary the scenarios drive: %v", err)
	}

	tags := os.Getenv(tagsEnv)
	if tags == "" {
		tags = defaultTags
	}

	world := steps.NewWorld(root, bin)
	suite := godog.TestSuite{
		Name:                "migration-tier0",
		ScenarioInitializer: world.InitializeScenario,
		Options: &godog.Options{
			Format: "pretty",
			Output: os.Stdout,
			Paths:  []string{featurePath},
			Tags:   tags,
			// Strict fails the suite on an undefined, pending or ambiguous
			// step. A Cucumber runner's default is to report an unimplemented
			// step as pending and carry on, which is a skip wearing a hat.
			Strict:      true,
			Concurrency: suiteConcurrency,
			TestingT:    t,
		},
	}

	if status := suite.Run(); status != 0 {
		t.Fatalf("migration tier-0 suite failed with status %d (tags %q)", status, tags)
	}
	// A suite that matched no scenario reports success. Assert the run
	// actually exercised something, so an empty or mis-tagged feature tree
	// fails rather than passing over nothing.
	if world.ScenariosRun() == 0 {
		t.Fatalf("the tier-0 suite ran no scenario for tags %q — the feature tree matched nothing", tags)
	}
}

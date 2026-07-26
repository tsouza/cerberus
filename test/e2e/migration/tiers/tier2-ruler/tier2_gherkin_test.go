//go:build migration_tier2

// Package tier2 runs the Tier-2 ruler slice of the Layer-14 migration
// scenarios: a godog suite over test/e2e/migration/features, filtered to
// @tier2, against the live stack `just migration-tier2-up` brings up.
// Mirrors tiers/tier0-offline/tier0_test.go's shape exactly; see that file's
// header for the rationale shared by both.
//
// This file is a sibling to tier2_ruler_test.go (the package-root substrate
// self-check that proves the mechanism), not a replacement for it — that
// file stays the pre-scenario proof that Grafana evaluates rules against
// cerberus and the dead-end receiver captures notifications; this one runs
// the actual MIG-13/MIG-18/MIG-19 scenarios against the same live stack.
package tier2

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
		t.Fatalf("build the cerberus binary the scenarios drive: %v", err)
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
			// step — a Cucumber runner's default is to report an
			// unimplemented step as pending and carry on, which is a skip
			// wearing a hat.
			Strict:      true,
			Concurrency: suiteConcurrency,
			TestingT:    t,
		},
	}

	if status := suite.Run(); status != 0 {
		t.Fatalf("migration tier-2 suite failed with status %d (tags %q)", status, tags)
	}
	// A suite that matched no scenario reports success. Assert the run
	// actually exercised something, so an empty or mis-tagged feature tree
	// fails rather than passing over nothing.
	if world.ScenariosRun() == 0 {
		t.Fatalf("the tier-2 suite ran no scenario for tags %q — the feature tree matched nothing", tags)
	}
}

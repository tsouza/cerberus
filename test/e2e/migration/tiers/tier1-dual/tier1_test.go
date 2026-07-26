//go:build migration_tier1

// Package tier1 runs the Tier-1 dual-backend slice of the Layer-14 migration
// scenarios: a godog suite over test/e2e/migration/features, filtered to
// @tier1, against the LIVE docker-compose.dual.yml stack (reference
// Prometheus/Loki/Tempo, ClickHouse, the OTel collector, and cerberus).
//
// Unlike Tier-0, this package drives no offline fixture and blackholes no
// network — the World's Given steps read the live stack's endpoints from the
// environment `just migration-tier1-up` / `just migration-tier1-seed`
// publish (see test/e2e/migration/lib/live.go) and fail clearly if the stack
// was never brought up. It shares the same `migration_tier1` build tag as
// tier1_stack_test.go / tier1_parity_test.go — the plain-Go substrate
// self-checks that prove the stack, seeder and comparator are sound — so all
// three live in the same CI lane and the same local `just` recipes.
package tier1

import (
	"os"
	"testing"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/test/e2e/migration/lib"
	"github.com/tsouza/cerberus/test/e2e/migration/steps"
)

// featurePath is the feature directory, relative to this package.
const featurePath = "../../features"

// defaultTags restricts the suite to the dual-backend tier.
const defaultTags = "@tier1"

// tagsEnv overrides defaultTags so the migration lane can drive one story,
// mirroring tier0_test.go's MIGRATION_TAGS contract.
const tagsEnv = "MIGRATION_TAGS"

// suiteConcurrency is fixed at one: the World is a single shared value that
// each scenario's Before hook resets, so scenarios must not interleave.
const suiteConcurrency = 1

func TestTier1(t *testing.T) {
	root, err := lib.RepoRoot()
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	// The live stack's cerberus container is what is under test; this binary
	// only drives the `cerberus migrate verify` CLI against it over HTTP.
	// CERBERUS_BIN short-circuits a rebuild when the migration-tier1-run
	// recipe already produced one; otherwise it is built fresh, exactly as
	// Tier-0 does.
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
		Name:                "migration-tier1",
		ScenarioInitializer: world.InitializeScenario,
		Options: &godog.Options{
			Format: "pretty",
			Output: os.Stdout,
			Paths:  []string{featurePath},
			Tags:   tags,
			// Strict fails the suite on an undefined, pending or ambiguous
			// step — the same posture as Tier-0, for the same reason: a
			// Cucumber runner's default of reporting an unimplemented step as
			// pending and carrying on is a skip wearing a hat.
			Strict:      true,
			Concurrency: suiteConcurrency,
			TestingT:    t,
		},
	}

	if status := suite.Run(); status != 0 {
		t.Fatalf("migration tier-1 suite failed with status %d (tags %q)", status, tags)
	}
	// A suite that matched no scenario reports success. Assert the run
	// actually exercised something, so a tag typo or an empty feature tree
	// fails rather than reading as a green run over nothing.
	if world.ScenariosRun() == 0 {
		t.Fatalf("the tier-1 suite ran no scenario for tags %q — the feature tree matched nothing", tags)
	}
}

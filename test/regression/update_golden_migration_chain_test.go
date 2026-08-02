package regression

import (
	"strings"
	"testing"
)

// migrationGoldenRecipeName regenerates the Tier-0 explain reports. It is the only
// thing that repairs them; `update-golden` must therefore reach it.
const migrationGoldenRecipeName = "migration-golden"

// migrationGoldenEnv is the switch migration-golden's body sets to put the
// harness in rewrite mode. Chaining a recipe that no longer sets it would be
// chaining a no-op.
const migrationGoldenEnv = "MIGRATION_UPDATE_GOLDENS"

// migrationGoldenDir holds the explain reports the recipe rewrites, and is the
// path `update-golden`'s closing review diff has to cover.
const migrationGoldenDir = "test/e2e/migration/archetypes/"

// goldenReviewDiffCommand opens the closing diff-stat line — the built-in
// review prompt that tells a contributor what to inspect before committing.
const goldenReviewDiffCommand = "git --no-pager diff --stat"

// migrationGoldenLibPath implements the harness's golden comparison, including
// the CI refusal that must stay a hard error rather than a silent skip.
const migrationGoldenLibPath = "../../test/e2e/migration/lib/golden.go"

// ciRefusalError is the shape of that refusal: an error value, not a `nil`
// early return. `refusing to regenerate` is the message it carries.
const ciRefusalError = "refusing to regenerate"

// TestUpdateGoldenChainsMigrationGolden pins the chaining that closes #1573.
//
// The Tier-0 explain reports record the emitted SQL for every corpus query
// verbatim, so any change that moves a plan shape drifts them. They regenerate
// only under `just migration-golden`. Before this chaining, a contributor who
// changed SQL emission ran `just update-golden` — the recipe every doc points
// at — watched it rewrite every fixture under test/spec/ and report zero
// remaining churn, and still arrived at CI with drifted migration goldens and a
// red `lint` job on "run the Tier-0 migration scenarios (explain-golden
// drift)". That happened at least twice (#1571, #1592).
//
// CI cannot repair it: the harness refuses to regenerate a golden when `CI` is
// set, because a golden is a reviewed artifact and a lane that rewrites its own
// expectations asserts only that the tool still does whatever it does. That
// refusal is correct, and it is precisely what makes the omission a trap — the
// one command that repairs it locally was the one nobody was told to run.
//
// Three things have to hold, and this test fails if any one is removed:
//
//  1. `update-golden` reaches `migration-golden` at all.
//  2. It reaches it as a PRIOR dependency. `just` runs subsequent (`&&`)
//     dependencies after the body, so a subsequent `migration-golden` would
//     regenerate the reports after the closing diff-stat had already printed —
//     the drift would be repaired but stay invisible in the diff the
//     contributor is asked to review, which is the second half of what #1573
//     reported. Nothing pulls the other way: the recipe re-derives each report
//     from the migration corpus's own committed dashboards and rules and never
//     reads test/spec/**, so the body's rewrite cannot change what it records.
//  3. The closing diff-stat actually covers the migration goldens. Scoped to
//     test/spec/ alone it under-reports exactly the drift this chaining exists
//     to surface.
func TestUpdateGoldenChainsMigrationGolden(t *testing.T) {
	t.Parallel()

	justfile := readFileString(t, justfilePath)
	body := justRecipeBody(t, justfile, strings.TrimSuffix(updateGoldenRecipePrefix, ":"))
	header, _, _ := strings.Cut(body, "\n")

	deps := strings.TrimPrefix(header, updateGoldenRecipePrefix)
	prior, subsequent, _ := strings.Cut(deps, subsequentDepSeparator)

	switch {
	case strings.Contains(prior, migrationGoldenRecipeName):
		// The pinned shape.
	case strings.Contains(subsequent, migrationGoldenRecipeName):
		t.Errorf("%s: %q is a SUBSEQUENT (%q) dependency of %q. `just` runs those after the "+
			"body, so the closing %q has already printed by the time the explain reports are "+
			"rewritten: the drift gets repaired but never reaches the diff the contributor is "+
			"asked to review. Move it to the prior-dependency list.",
			justfilePath, migrationGoldenRecipeName, subsequentDepSeparator, updateGoldenRecipePrefix,
			goldenReviewDiffCommand)
	default:
		t.Errorf("%s: %q does not depend on %q (dependencies are %q). The Tier-0 explain reports "+
			"record emitted SQL verbatim and regenerate under no other recipe, so without this "+
			"chaining a plan-shape change reports zero remaining churn locally and still lands a "+
			"red `lint` job on the explain-golden drift step. Restore it as a prior dependency "+
			"rather than deleting this pin.",
			justfilePath, updateGoldenRecipePrefix, migrationGoldenRecipeName, strings.TrimSpace(deps))
	}

	// A chained recipe that no longer switches the harness into rewrite mode is
	// chaining nothing: the lane would run the Tier-0 scenarios in ASSERT mode,
	// pass on an in-sync corpus, and look identical to a working regeneration
	// right up until a plan shape moves.
	migration := justRecipeBody(t, justfile, migrationGoldenRecipeName)
	if !strings.Contains(migration, migrationGoldenEnv) {
		t.Errorf("%s: the %q recipe no longer sets %s, so it asserts the goldens instead of "+
			"rewriting them. Chaining it into %q would then be inert — green on an in-sync corpus, "+
			"useless on the drift it exists to repair.",
			justfilePath, migrationGoldenRecipeName, migrationGoldenEnv, updateGoldenRecipePrefix)
	}

	// The closing review prompt has to name the path the chained recipe writes.
	var reviewLine string
	for _, line := range strings.Split(body, "\n")[1:] {
		if strings.Contains(line, goldenReviewDiffCommand) {
			reviewLine = line
		}
	}
	if reviewLine == "" {
		t.Fatalf("%s: %q has no %q line. That closing diff-stat is the built-in review prompt for "+
			"everything the chain regenerated; this pin cannot check its scope without it.",
			justfilePath, updateGoldenRecipePrefix, goldenReviewDiffCommand)
	}
	if !strings.Contains(reviewLine, migrationGoldenDir) {
		t.Errorf("%s: %q's closing diff-stat (%q) does not cover %s. Regenerating the explain "+
			"reports and then scoping the review prompt so they cannot appear in it leaves the "+
			"drift as invisible as it was before the chaining.",
			justfilePath, updateGoldenRecipePrefix, strings.TrimSpace(reviewLine), migrationGoldenDir)
	}

	// The CI refusal must stay an error. Downgraded to a silent early return it
	// would turn every CI invocation into a lane that regenerates nothing while
	// reporting success — the failure mode that looks exactly like working.
	lib := readFileString(t, migrationGoldenLibPath)
	if !strings.Contains(lib, ciRefusalError) {
		t.Errorf("%s no longer refuses regeneration with an error containing %q. The refusal is "+
			"what keeps a CI run from rewriting the expectations it is supposed to check; if it "+
			"became a silent skip, the lane would pass while asserting nothing.",
			migrationGoldenLibPath, ciRefusalError)
	}
}

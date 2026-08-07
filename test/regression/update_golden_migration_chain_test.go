package regression

import (
	"strings"
	"testing"
)

// migrationGoldenRecipeName regenerates the Tier-0 explain reports. It is the only
// thing that repairs them; the `migration` shard must therefore reach it.
const migrationGoldenRecipeName = "migration-golden"

// migrationGoldenEnv is the switch migration-golden's body sets to put the
// harness in rewrite mode. A shard that runs a recipe no longer setting it would
// be running a no-op.
const migrationGoldenEnv = "MIGRATION_UPDATE_GOLDENS"

// migrationGoldenDir holds the explain reports the recipe rewrites, and is the
// path `update-golden`'s closing review diff has to cover.
const migrationGoldenDir = "test/e2e/migration/archetypes"

// migrationTriggerFile is a plan-shape input: every Tier-0 explain report
// records SQL this package emits. It is the change that must pull the
// `migration` shard in.
const migrationTriggerFile = "internal/chsql/emit.go"

// migrationGoldenLibPath implements the harness's golden comparison, including
// the CI refusal that must stay a hard error rather than a silent skip.
const migrationGoldenLibPath = "../../test/e2e/migration/lib/golden.go"

// ciRefusalError is the shape of that refusal: an error value, not a `nil`
// early return. `refusing to regenerate` is the message it carries.
const ciRefusalError = "refusing to regenerate"

// TestUpdateGoldenChainsMigrationGolden pins the reachability that closes #1573.
//
// The Tier-0 explain reports record the emitted SQL for every corpus query
// verbatim, so any change that moves a plan shape drifts them. They regenerate
// only under `just migration-golden`. Before this reachability existed, a
// contributor who changed SQL emission ran `just update-golden` — the recipe
// every doc points at — watched it rewrite every fixture under test/spec/ and
// report zero remaining churn, and still arrived at CI with drifted migration
// goldens and a red `lint` job on "run the Tier-0 migration scenarios
// (explain-golden drift)". That happened at least twice (#1571, #1592).
//
// CI cannot repair it: the harness refuses to regenerate a golden when `CI` is
// set, because a golden is a reviewed artifact and a lane that rewrites its own
// expectations asserts only that the tool still does whatever it does. That
// refusal is correct, and it is precisely what makes the omission a trap — the
// one command that repairs it locally was the one nobody was told to run.
//
// Sharding #1898 moved the guarantee without weakening it: the reports are no
// longer regenerated unconditionally on every invocation, they are regenerated
// whenever the caller's shard set includes `migration` — and the coverage check
// DEMANDS that shard the moment the branch touches anything the reports are
// derived from. Four things have to hold, and this test fails if any is removed:
//
//  1. The `migration` shard reaches `migration-golden` at all.
//  2. A plan-shape change pulls the shard in. This is the half that replaces the
//     old unconditional chaining; without it a contributor could name `promql`
//     alone and walk straight back into #1573.
//  3. The shard runs BEFORE the fixture body, so its output lands inside the
//     closing diff-stat rather than after it — the drift would otherwise be
//     repaired but stay invisible in the diff the contributor is asked to
//     review, which is the second half of what #1573 reported. Nothing pulls the
//     other way: the recipe re-derives each report from the migration corpus's
//     own committed dashboards and rules and never reads test/spec/**, so the
//     body's rewrite cannot change what it records.
//  4. The closing diff-stat actually covers the migration goldens. Scoped to
//     test/spec/ alone it under-reports exactly the drift this exists to
//     surface.
func TestUpdateGoldenChainsMigrationGolden(t *testing.T) {
	t.Parallel()

	plan := goldenUpdatePlan(t)

	step := planStepFor(t, plan, migrationShard)
	if !strings.Contains(step, migrationGoldenRecipeName) {
		t.Errorf("the %q shard runs %q, which does not reach %q. The Tier-0 explain reports "+
			"record emitted SQL verbatim and regenerate under no other recipe, so a shard that "+
			"no longer invokes it leaves a plan-shape change reporting zero remaining churn "+
			"locally and still landing a red `lint` job on the explain-golden drift step.",
			migrationShard, strings.TrimSpace(step), migrationGoldenRecipeName)
	}

	out, code := checkGoldenShardCoverage(t, "promql", migrationTriggerFile)
	if code == 0 || !strings.Contains(out, "`"+migrationShard+"` shard") {
		t.Errorf("changing %s did not demand the %q shard (exit %d). Every Tier-0 explain report "+
			"records SQL that package emits, so the reports go stale — and a contributor who "+
			"named only `promql` would be told there is nothing left to regenerate. That is "+
			"#1573 verbatim, one level up from where it was fixed.\n%s",
			migrationTriggerFile, migrationShard, code, out)
	}

	if got, body := indexOfShard(t, plan, migrationShard), planBodyStart(t, plan); got > body {
		t.Errorf("the %q shard runs at plan step %d, after the fixture body starts at step %d. "+
			"Output produced after the body lands outside the closing diff-stat: the drift gets "+
			"repaired but never reaches the diff the contributor is asked to review.\n%s",
			migrationShard, got, body, strings.Join(plan, "\n"))
	}

	// A shard whose recipe no longer switches the harness into rewrite mode
	// regenerates nothing: the lane would run the Tier-0 scenarios in ASSERT
	// mode, pass on an in-sync corpus, and look identical to a working
	// regeneration right up until a plan shape moves.
	justfile := readFileString(t, justfilePath)
	migration := justRecipeBody(t, justfile, migrationGoldenRecipeName)
	if !strings.Contains(migration, migrationGoldenEnv) {
		t.Errorf("%s: the %q recipe no longer sets %s, so it asserts the goldens instead of "+
			"rewriting them. The %q shard would then be inert — green on an in-sync corpus, "+
			"useless on the drift it exists to repair.",
			justfilePath, migrationGoldenRecipeName, migrationGoldenEnv, migrationShard)
	}

	// The closing review prompt has to name the path the shard writes.
	if scope := planDiffScope(t, plan); !strings.Contains(scope, migrationGoldenDir) {
		t.Errorf("the closing diff-stat (%q) does not cover %s. Regenerating the explain reports "+
			"and then scoping the review prompt so they cannot appear in it leaves the drift as "+
			"invisible as it was before.", strings.TrimSpace(scope), migrationGoldenDir)
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

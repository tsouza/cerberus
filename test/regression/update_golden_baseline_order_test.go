package regression

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// justfilePath is the canonical task runner; `update-golden` lives here.
const justfilePath = "../../Justfile"

// prepareRoundTripPath holds the seam that decides WHICH SQL the cardinality
// profiler measures. It is the reason the stage ordering below is not free.
const prepareRoundTripPath = "../../test/spec/prepare_chdb.go"

// solverBaselineShard re-derives its rows from each fixture's
// `-- query.promql --` input, so the golden rewrite cannot change what it
// records; it must run BEFORE the body, which asserts it.
const solverBaselineShard = "solver"

// cardinalityBaselineShard profiles each fixture's RECORDED `-- sql --`
// section, so it must run AFTER the body rewrites that section.
const cardinalityBaselineShard = "cardinality"

// migrationShard and parityShard re-derive from inputs the body never writes,
// and run before it so their drift lands inside the closing diff-stat.
const (
	migrationShard = "migration"
	parityShard    = "parity"
)

// goldenUpdateEnvMarker appears on exactly the plan steps that rewrite fixture
// goldens — the body the two ratchet baselines bracket.
const goldenUpdateEnvMarker = "GOLDEN_UPDATE=1"

// recordedSQLField is the field spec.PrepareRoundTrip reads to build the query
// the profiler measures: the fixture's recorded `-- sql --` section, verbatim.
const recordedSQLField = "rt.SQL"

// emptySQLGate is the guard that makes a fixture with an unfilled `-- sql --`
// section invisible to the profiler entirely.
const emptySQLGate = `strings.TrimSpace(rt.SQL) == ""`

// goldenUpdatePlan asks the runner for the ordered regeneration plan it would
// execute for `all`. One line per step: `<stage> <shard> <command>`.
//
// Pinning the PLAN rather than the Justfile's dependency line is what keeps
// this test honest across the sharding change: the ordering is no longer
// spelled in just's `dep && dep` syntax, it is a property of the shard table,
// and the plan is that property observed rather than re-read from source.
func goldenUpdatePlan(t *testing.T) []string {
	t.Helper()

	cmd := exec.Command("node", goldenUpdateModule)
	cmd.Dir = repoRootFromRegression
	cmd.Env = append(os.Environ(), "GOLDEN_UPDATE_PRINT_PLAN=1", "GOLDEN_SHARDS=all")
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("%s exited %d printing the plan:\n%s", goldenUpdateModule, exitErr.ExitCode(), out)
		}
		t.Fatalf("run %s: %v\n%s", goldenUpdateModule, err, out)
	}

	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("%s printed an empty plan for `all`; this pin cannot see the stage ordering",
			goldenUpdateModule)
	}

	return lines
}

// indexOfShard returns the first plan-line index belonging to a shard.
func indexOfShard(t *testing.T, plan []string, shard string) int {
	t.Helper()

	for i, line := range plan {
		if fields := strings.Fields(line); len(fields) > 1 && fields[1] == shard {
			return i
		}
	}
	t.Fatalf("the `all` plan has no step for the %q shard:\n%s", shard, strings.Join(plan, "\n"))

	return -1
}

// planStepFor returns the plan line a shard contributes, command included.
func planStepFor(t *testing.T, plan []string, shard string) string {
	t.Helper()

	return plan[indexOfShard(t, plan, shard)]
}

// planBodyStart is the index of the first fixture-rewriting step in the plan.
// Everything a shard has to precede is measured against it.
func planBodyStart(t *testing.T, plan []string) int {
	t.Helper()

	for i, line := range plan {
		if strings.Contains(line, goldenUpdateEnvMarker) {
			return i
		}
	}
	t.Fatalf("the `all` plan runs no %s step, so it rewrites no fixture goldens at all:\n%s",
		goldenUpdateEnvMarker, strings.Join(plan, "\n"))

	return -1
}

// planDiffScopePrefix opens the plan's final line, which names the pathspecs of
// the closing review diff-stat — the built-in prompt telling a contributor what
// to inspect before committing.
const planDiffScopePrefix = "diff "

// planDiffScope returns that line.
func planDiffScope(t *testing.T, plan []string) string {
	t.Helper()

	for _, line := range plan {
		if strings.HasPrefix(line, planDiffScopePrefix) {
			return line
		}
	}
	t.Fatalf("the `all` plan has no %q line. That closing diff-stat is the review prompt for "+
		"everything the run regenerated; this pin cannot check its scope without it.\n%s",
		strings.TrimSpace(planDiffScopePrefix), strings.Join(plan, "\n"))

	return ""
}

// TestUpdateGoldenRecordsCardinalityBaselineAfterRewrite pins the side of the
// fixture-golden body each ratchet baseline runs on.
//
// `just update-golden` rewrites every fixture's `-- sql --` section. The
// cardinality baseline does NOT re-lower `-- input --`; it profiles the
// recorded `-- sql --` text — spec.PrepareRoundTrip reads rt.SQL, rewrites it,
// and profile.ProfileFixture measures exactly that. So recording the baseline
// before the rewrite profiles SQL the same invocation is about to replace, and
// pins a fan factor for a query shape that no longer exists.
//
// The brand-new-fixture case is worse than stale: a new fixture's `-- sql --`
// is empty until the body fills it, PrepareRoundTrip returns ok=false on an
// empty section, and profile.FindExecutableFixtures therefore never sees the
// fixture at all. Its row is silently skipped — which is precisely the miss
// (#1096, #1098: unrecorded fixture, red `perf-guards` on main after merge)
// that chaining the baseline into `update-golden` was added to close. Running
// it first leaves the feature inert for its own primary use case, and inert
// looks identical to working: the recipe exits 0 and prints an empty diff.
//
// The solver-decision baseline is the mirror image and must stay ahead of the
// body: it re-derives from the `-- query.promql --` input, and the body's
// default-tag lane runs TestSolverDecisionRatchet in ASSERT mode, which fails
// on a fixture not yet in the routing baseline. `migration` and `parity`
// likewise precede the body — neither reads `test/spec/**`, and running them
// first puts their drift inside the closing diff-stat a contributor reviews.
func TestUpdateGoldenRecordsCardinalityBaselineAfterRewrite(t *testing.T) {
	t.Parallel()

	plan := goldenUpdatePlan(t)

	bodyFirst, bodyLast := -1, -1
	for i, line := range plan {
		if strings.Contains(line, goldenUpdateEnvMarker) {
			if bodyFirst == -1 {
				bodyFirst = i
			}
			bodyLast = i
		}
	}
	if bodyFirst == -1 {
		t.Fatalf("the `all` plan runs no %s step, so it rewrites no fixture goldens at all. "+
			"An update-golden that regenerates nothing exits 0 and prints an empty diff, which "+
			"is indistinguishable from a corpus already in sync:\n%s",
			goldenUpdateEnvMarker, strings.Join(plan, "\n"))
	}

	if got := indexOfShard(t, plan, cardinalityBaselineShard); got < bodyLast {
		t.Errorf("the %q shard runs at plan step %d, before the fixture body ends at step %d. "+
			"It profiles the recorded `-- sql --` section that the body rewrites, so running it "+
			"first records a fan factor for SQL that no longer exists — and skips a brand-new "+
			"fixture entirely, because its `-- sql --` is still empty at that point.\n%s",
			cardinalityBaselineShard, got, bodyLast, strings.Join(plan, "\n"))
	}

	for _, shard := range []string{solverBaselineShard, migrationShard, parityShard} {
		if got := indexOfShard(t, plan, shard); got > bodyFirst {
			t.Errorf("the %q shard runs at plan step %d, after the fixture body starts at step "+
				"%d. The body's default-tag lane runs TestSolverDecisionRatchet in ASSERT mode, "+
				"which fails on a brand-new fixture absent from the routing baseline and aborts "+
				"the whole run; and output produced after the body lands outside the closing "+
				"diff-stat a contributor is asked to review.\n%s",
				shard, got, bodyFirst, strings.Join(plan, "\n"))
		}
	}

	// Pin the provenance the ordering rests on, alongside the ordering itself.
	// If the profiler is ever changed to re-lower `-- input --` instead of
	// reading the recorded SQL, the constraint above dissolves and the comment
	// block in the Justfile becomes false — this is what makes that visible
	// instead of leaving a stale rationale behind.
	prep, err := os.ReadFile(prepareRoundTripPath)
	if err != nil {
		t.Fatalf("read %s: %v", prepareRoundTripPath, err)
	}
	body := string(prep)
	if !strings.Contains(body, recordedSQLField) {
		t.Errorf("%s no longer builds the profiled query from %s (the fixture's recorded "+
			"`-- sql --` section). If it now re-derives from `-- input --`, the ordering pinned "+
			"above is no longer required — re-derive it deliberately and update the Justfile "+
			"rationale rather than leaving a stale explanation in place.",
			prepareRoundTripPath, recordedSQLField)
	}
	if !strings.Contains(body, emptySQLGate) {
		t.Errorf("%s no longer gates on %s. That guard is why a brand-new fixture is invisible "+
			"to the cardinality profiler until the golden rewrite fills its `-- sql --` section; "+
			"the recipe ordering above depends on it.", prepareRoundTripPath, emptySQLGate)
	}
}

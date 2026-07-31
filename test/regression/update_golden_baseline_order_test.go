package regression

import (
	"os"
	"strings"
	"testing"
)

// justfilePath is the canonical task runner; `update-golden` lives here.
const justfilePath = "../../Justfile"

// prepareRoundTripPath holds the seam that decides WHICH SQL the cardinality
// profiler measures. It is the reason the recipe ordering below is not free.
const prepareRoundTripPath = "../../test/spec/prepare_chdb.go"

// updateGoldenRecipePrefix opens the recipe header whose dependency shape this
// file pins. Matched at column 0 so the prose above it cannot satisfy the pin.
const updateGoldenRecipePrefix = "update-golden:"

// solverBaselineRecipe re-derives its rows from each fixture's
// `-- query.promql --` input, so the golden rewrite cannot change what it
// records; it must run BEFORE the body, which asserts it.
const solverBaselineRecipe = "update-solver-decision-baseline"

// cardinalityBaselineRecipe profiles each fixture's RECORDED `-- sql --`
// section, so it must run AFTER the body rewrites that section.
const cardinalityBaselineRecipe = "update-cardinality-baseline"

// subsequentDepSeparator is just's marker for dependencies that run after the
// recipe body instead of before it.
const subsequentDepSeparator = "&&"

// recordedSQLField is the field spec.PrepareRoundTrip reads to build the query
// the profiler measures: the fixture's recorded `-- sql --` section, verbatim.
const recordedSQLField = "rt.SQL"

// emptySQLGate is the guard that makes a fixture with an unfilled `-- sql --`
// section invisible to the profiler entirely.
const emptySQLGate = `strings.TrimSpace(rt.SQL) == ""`

// TestUpdateGoldenRecordsCardinalityBaselineAfterRewrite pins the side of the
// body each ratchet baseline runs on.
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
// The solver-decision baseline is the mirror image and must stay a PRIOR dep:
// it re-derives from the `-- query.promql --` input, and the body's default-tag
// lane runs TestSolverDecisionRatchet in ASSERT mode, which fails on a fixture
// not yet in the routing baseline. Satisfying both is why just's `&&`
// subsequent-dependency form is used rather than moving everything one way.
func TestUpdateGoldenRecordsCardinalityBaselineAfterRewrite(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile(justfilePath)
	if err != nil {
		t.Fatalf("read %s: %v", justfilePath, err)
	}

	var recipeLine string
	for _, line := range strings.Split(string(buf), "\n") {
		if strings.HasPrefix(line, updateGoldenRecipePrefix) {
			recipeLine = line

			break
		}
	}
	if recipeLine == "" {
		t.Fatalf("%s has no %q recipe header at column 0; this pin cannot see the dependency "+
			"shape. If the recipe was renamed, re-point this test at the new name rather than "+
			"deleting it.", justfilePath, updateGoldenRecipePrefix)
	}

	deps := strings.TrimPrefix(recipeLine, updateGoldenRecipePrefix)
	prior, subsequent, split := strings.Cut(deps, subsequentDepSeparator)
	if !split {
		t.Fatalf("%s: %q has no %q separator, so every dependency runs BEFORE the body. %s "+
			"profiles each fixture's recorded `-- sql --` section, which this recipe's body "+
			"rewrites — and on a brand-new fixture that section is still empty, so the fixture is "+
			"not even discovered and its row is never recorded. Make it a subsequent dependency: "+
			"`%s %s %s %s`.",
			justfilePath, strings.TrimSpace(recipeLine), subsequentDepSeparator,
			cardinalityBaselineRecipe, updateGoldenRecipePrefix, solverBaselineRecipe,
			subsequentDepSeparator, cardinalityBaselineRecipe)
	}

	if !strings.Contains(subsequent, cardinalityBaselineRecipe) {
		t.Errorf("%s: %q does not run AFTER the body (not found among the %q dependencies %q). "+
			"It profiles the recorded `-- sql --` section that the body rewrites, so running it "+
			"first records a fan factor for SQL that no longer exists — and skips a brand-new "+
			"fixture entirely, because its `-- sql --` is still empty at that point.",
			justfilePath, cardinalityBaselineRecipe, subsequentDepSeparator,
			strings.TrimSpace(subsequent))
	}
	if strings.Contains(prior, cardinalityBaselineRecipe) {
		t.Errorf("%s: %q is still a PRIOR dependency (%q). Recording the cardinality baseline "+
			"before the golden rewrite is the bug this test exists to prevent; it must appear "+
			"only after %q.",
			justfilePath, cardinalityBaselineRecipe, strings.TrimSpace(prior),
			subsequentDepSeparator)
	}

	if !strings.Contains(prior, solverBaselineRecipe) {
		t.Errorf("%s: %q does not run BEFORE the body (prior dependencies are %q). The body's "+
			"default-tag lane runs TestSolverDecisionRatchet in ASSERT mode, which fails on a "+
			"brand-new fixture absent from the routing baseline and aborts the whole recipe.",
			justfilePath, solverBaselineRecipe, strings.TrimSpace(prior))
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

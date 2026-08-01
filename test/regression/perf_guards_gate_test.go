package regression

import (
	"strings"
	"testing"
)

// The cardinality / fan-out ratchet (test/perf.TestCardinalityRatchet) only
// blocks a merge through a chain of four links, and three of them live outside
// the Go package the ratchet is written in:
//
//	TestCardinalityRatchet  ->  `just perf-chdb`  ->  chdb.yml `perf-guards`
//	                        ->  branch protection / RELEASE_REQUIRED_CHECKS
//
// Break any link and the ratchet keeps passing, keeps being run, keeps
// printing nothing — and stops gating. That is not a hypothetical: the
// ratchet's own doc comment asserted for months that it ran "inside the
// already-required `perf-guards` job", while `perf-guards` was not in the
// required-contexts set at all. A fixture whose fan-out grew reddened one
// non-blocking lane and merged. The failure mode is invisible precisely
// because every visible signal (the test runs, the job runs, the assertions
// assert) looks exactly the same either way.
//
// The link branch protection owns cannot be asserted from a checkout — it is
// repo settings, watched on a schedule by release-gate-drift.yml against the
// live API. What this file pins is everything a PR diff CAN break:
//
//   - the JOB NAME branch protection's required context refers to. Rename the
//     job and the context is never posted; the required check sits "Expected"
//     forever, and the fix looks like "drop the stale context" rather than
//     "restore the lane";
//   - the RECIPE that job runs, and that the recipe's scope still covers the
//     package the ratchet lives in. Narrowing `perf-chdb` to a `-run` filter
//     leaves the required check green while the ratchet stops executing —
//     the hollow-green shape, where a gate degrades to OFF and reports
//     success;
//   - `continue-on-error`, which turns the job into decoration while keeping
//     its name and its logs;
//   - membership in release.yml's RELEASE_REQUIRED_CHECKS, so a publish waits
//     on the same lane a PR does. release.yml has no `pull_request:` trigger,
//     so nothing else checks that at PR time.
const (
	chdbWorkflowPath = "../../.github/workflows/chdb.yml"

	// The job branch protection names, and the recipe it must run. Both are
	// strings some other system resolves by exact text.
	perfGuardsJob    = "perf-guards"
	perfGuardsRecipe = "perf-chdb"

	// The package glob that has to stay in the recipe: the ratchet is
	// test/perf/cardinality_ratchet_test.go, so a recipe that no longer runs
	// the whole package no longer runs the ratchet.
	perfPackageGlob = "./test/perf/..."

	// The chdb build tag, without which the ratchet's file is not compiled and
	// `go test` reports a passing package containing no tests.
	chdbBuildTag = "-tags chdb"

	// The escape hatch that would keep the job's name and logs while removing
	// its verdict.
	continueOnError = "continue-on-error"
)

// TestPerfGuardsLaneRunsTheRatchet pins the two in-repo links of the chain:
// the job name branch protection resolves, and the recipe that job runs still
// covering the package the ratchet lives in.
func TestPerfGuardsLaneRunsTheRatchet(t *testing.T) {
	t.Parallel()

	job := workflowJobBody(t, readFileString(t, chdbWorkflowPath), perfGuardsJob)

	if !strings.Contains(job, perfGuardsRecipe) {
		t.Errorf("%s job %q no longer runs `just %s`, so the required context is posted by a job that "+
			"does not run the cardinality ratchet. Body:\n%s",
			chdbWorkflowPath, perfGuardsJob, perfGuardsRecipe, job)
	}
	if strings.Contains(job, continueOnError) {
		t.Errorf("%s job %q sets %s — it keeps its name and its logs and loses its verdict, which is "+
			"strictly worse than not gating at all because the required check now reports green over a "+
			"failing ratchet.",
			chdbWorkflowPath, perfGuardsJob, continueOnError)
	}

	recipe := justRecipeBody(t, readFileString(t, justfilePath), perfGuardsRecipe)
	if !strings.Contains(recipe, perfPackageGlob) {
		t.Errorf("%s recipe %q no longer runs %s. Narrowing the recipe (a `-run` filter, a single test "+
			"file) leaves the required check green while the cardinality ratchet stops executing — the "+
			"gate degrades to OFF and reports success. Recipe:\n%s",
			justfilePath, perfGuardsRecipe, perfPackageGlob, recipe)
	}
	if !strings.Contains(recipe, chdbBuildTag) {
		t.Errorf("%s recipe %q dropped %q. The ratchet's file is `//go:build chdb`, so without the tag "+
			"`go test` compiles a package with no tests in it and exits 0 — a green lane over an "+
			"unexecuted gate. Recipe:\n%s",
			justfilePath, perfGuardsRecipe, chdbBuildTag, recipe)
	}
}

// TestReleasePreflightRequiresThePerfGuardsLane pins the release half. A lane
// every PR must pass, that a publish does not wait for, ships artifacts past a
// gate the repo already decided matters.
func TestReleasePreflightRequiresThePerfGuardsLane(t *testing.T) {
	t.Parallel()

	job := workflowJobBody(t, readFileString(t, releaseWorkflowPath), preflightJob)
	required := requiredChecksFromPreflight(t, job)

	for _, name := range required {
		if name == perfGuardsJob {
			return
		}
	}
	t.Errorf("%s job %q does not require %q (required set: %q). The preflight is observation-derived "+
		"for everything outside that set, so a lane that never ran contributes zero problems and the "+
		"release publishes on a silence. release-gate-drift.yml reports the same gap from the other "+
		"side on a schedule; this fails at PR time instead.",
		releaseWorkflowPath, preflightJob, perfGuardsJob, required)
}

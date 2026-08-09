package regression

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The cardinality / fan-out ratchet (test/perf.TestCardinalityRatchet) only
// blocks a merge through a chain of five links, and four of them live outside
// the Go package the ratchet is written in:
//
//	TestCardinalityRatchet  ->  `just perf-chdb`  ->  chdb.yml
//	                            `perf-guards-shard` (the matrix)
//	                        ->  chdb.yml `perf-guards` (the aggregator)
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
//   - the RECIPE the lane runs, and that the recipe's scope still covers the
//     package the ratchet lives in. Narrowing `perf-chdb` to a `-run` filter
//     leaves the required check green while the ratchet stops executing —
//     the hollow-green shape, where a gate degrades to OFF and reports
//     success;
//   - the AGGREGATOR's dependency on the matrix. Since #2002 the lane is
//     sharded and the required context is posted by a job that runs no tests
//     at all, so a `perf-guards` that stopped `needs:`-ing `perf-guards-shard`
//     would report green in five seconds over a matrix that failed — the same
//     hollow green one level up;
//   - the SHARD WIRING. The partition divides by PERF_SHARD_COUNT, so a count
//     that disagrees with the number of legs actually dispatched leaves part of
//     the corpus owned by no leg, ratcheted by nothing, with every check green.
//     That is why the workflow derives it from `strategy.job-total` rather than
//     repeating the literal, and why that expression is pinned here;
//   - `continue-on-error`, which turns a job into decoration while keeping its
//     name and its logs;
//   - membership in release.yml's RELEASE_REQUIRED_CHECKS, so a publish waits
//     on the same lane a PR does. release.yml has no `pull_request:` trigger,
//     so nothing else checks that at PR time.
const (
	chdbWorkflowPath = "../../.github/workflows/chdb.yml"

	// The job branch protection names, the matrix job that does the work, and
	// the recipe that matrix runs. All three are strings some other system
	// resolves by exact text.
	perfGuardsJob      = "perf-guards"
	perfGuardsShardJob = "perf-guards-shard"
	perfGuardsRecipe   = "perf-chdb"

	// The aggregator's verdict script. Without it the job is an empty green.
	perfGuardsAggregateScript = "perf-guards-aggregate.mjs"

	// The shard contract, spelled exactly as the workflow must spell it.
	// PERF_SHARD_COUNT is `strategy.job-total` — the number of legs the matrix
	// really fans out to — precisely so it cannot drift from the `shard:` list.
	perfShardIndexExpr = "PERF_SHARD_INDEX: ${{ matrix.shard }}"
	perfShardCountExpr = "PERF_SHARD_COUNT: ${{ strategy.job-total }}"

	// Where the partition's balance and coverage properties are asserted, and
	// the constant that says at which shard count. The properties are only
	// evidence about the lane if that count is the count CI dispatches.
	shardPartitionTestPath = "../perf/profile/shard_test.go"

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

// TestPerfGuardsLaneRunsTheRatchet pins the in-repo links of the chain: the
// matrix job that actually runs the recipe, the recipe's scope still covering
// the package the ratchet lives in, and the aggregator that lends the whole
// thing the name branch protection resolves.
func TestPerfGuardsLaneRunsTheRatchet(t *testing.T) {
	t.Parallel()

	workflow := readFileString(t, chdbWorkflowPath)
	shardJob := workflowJobBody(t, workflow, perfGuardsShardJob)
	aggregate := workflowJobBody(t, workflow, perfGuardsJob)

	if !strings.Contains(shardJob, perfGuardsRecipe) {
		t.Errorf("%s job %q no longer runs `just %s`, so nothing under the required %q context runs the "+
			"cardinality ratchet. Body:\n%s",
			chdbWorkflowPath, perfGuardsShardJob, perfGuardsRecipe, perfGuardsJob, shardJob)
	}
	for _, job := range []struct{ name, body string }{
		{perfGuardsShardJob, shardJob},
		{perfGuardsJob, aggregate},
	} {
		if strings.Contains(job.body, continueOnError) {
			t.Errorf("%s job %q sets %s — it keeps its name and its logs and loses its verdict, which is "+
				"strictly worse than not gating at all because the required check now reports green over a "+
				"failing ratchet.",
				chdbWorkflowPath, job.name, continueOnError)
		}
	}

	// The aggregator posts the required context and runs no tests, so its ONLY
	// value is the dependency edge and the verdict it derives from it.
	//
	// Read the `needs:` LINE, not the job body. The body also mentions
	// `needs.perf-guards-shard.result` in the step env, so a plain substring
	// search over it stays green after the dependency itself is removed — which
	// is the exact edit that turns this gate off.
	needsLine := needsDeclarationRe.FindStringSubmatch(aggregate)
	if needsLine == nil || !strings.Contains(needsLine[1], perfGuardsShardJob) {
		declared := "(none)"
		if needsLine != nil {
			declared = needsLine[1]
		}
		t.Errorf("%s job %q declares `needs: %s`, which does not include %q. It is the job that posts the "+
			"required context and it runs no tests of its own, so without that edge it reports green in "+
			"seconds no matter what the shard matrix did.",
			chdbWorkflowPath, perfGuardsJob, declared, perfGuardsShardJob)
	}
	if !strings.Contains(aggregate, perfGuardsAggregateScript) {
		t.Errorf("%s job %q no longer runs %s. `needs:` alone does not gate: a matrix rolls up to one "+
			"result and the aggregate must actually read it — including telling a docs-only skip apart "+
			"from a `changes` job that crashed before deciding. Body:\n%s",
			chdbWorkflowPath, perfGuardsJob, perfGuardsAggregateScript, aggregate)
	}

	// The shard contract. A leg that divides by the wrong count owns the wrong
	// slice, and the slices no leg owns are ratcheted by nobody.
	for _, want := range []string{perfShardIndexExpr, perfShardCountExpr} {
		if !strings.Contains(shardJob, want) {
			t.Errorf("%s job %q does not wire `%s`. PERF_SHARD_COUNT must come from `strategy.job-total` "+
				"(the number of legs that actually run), never a repeated literal: a count left behind "+
				"after the `shard:` list changed leaves part of the corpus owned by no leg, and every leg "+
				"still reports green. Body:\n%s",
				chdbWorkflowPath, perfGuardsShardJob, want, shardJob)
		}
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

var (
	// A job's own `needs:` declaration — the two-space-deeper key, not the
	// `needs.<job>.result` expressions that appear in step bodies.
	needsDeclarationRe = regexp.MustCompile(`(?m)^\s{4}needs:\s*(.+)$`)

	// The matrix leg list, e.g. `        shard: [1, 2, 3, 4, 5, 6, 7, 8]`.
	shardMatrixListRe = regexp.MustCompile(`(?m)^\s*shard:\s*\[([0-9,\s]*)\]\s*$`)

	// The count test/perf/profile's partition properties are asserted at.
	productionShardCountRe = regexp.MustCompile(`(?m)^const productionShardCount = (\d+)`)
)

// TestPerfGuardsShardMatrixCoversEverySlice pins the shape of the partition's
// two halves against each other.
//
// The partition itself is a pure function of the fixture id modulo N
// (test/perf/profile/shard.go), and its coverage, disjointness and balance are
// asserted in that package — at ONE count, the `productionShardCount` constant.
// Those assertions are evidence about this lane only while that constant is the
// number of legs chdb.yml really dispatches. Let the two drift and the
// properties are still proven, just about a partition nobody runs: the balance
// floor would be measured at 8 while CI splits 12 ways.
//
// The leg list itself has to be 1..N with no gaps and no repeats, because
// PERF_SHARD_INDEX is that literal. A list of [1, 2, 4] dispatches three legs,
// so `strategy.job-total` is 3, so the partition splits three ways — and the leg
// labelled 4 asks for a slice that does not exist while slice 3 is owned by
// nobody. Every leg still passes.
func TestPerfGuardsShardMatrixCoversEverySlice(t *testing.T) {
	t.Parallel()

	shardJob := workflowJobBody(t, readFileString(t, chdbWorkflowPath), perfGuardsShardJob)

	list := shardMatrixListRe.FindStringSubmatch(shardJob)
	if list == nil {
		t.Fatalf("%s job %q declares no `shard: [...]` matrix list. The lane is sharded precisely because "+
			"one process cannot finish it; a matrix that stopped fanning out is the serial lane back, under "+
			"a name that says otherwise. Body:\n%s", chdbWorkflowPath, perfGuardsShardJob, shardJob)
	}

	var legs []int
	for _, field := range strings.Split(list[1], ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("%s job %q has a non-integer shard %q in `shard: [%s]`",
				chdbWorkflowPath, perfGuardsShardJob, field, list[1])
		}
		legs = append(legs, n)
	}

	sort.Ints(legs)
	want := make([]int, 0, len(legs))
	for i := range legs {
		want = append(want, i+1)
	}
	if fmt.Sprint(legs) != fmt.Sprint(want) {
		t.Errorf("%s job %q declares shards %v, which is not the contiguous 1..%d PERF_SHARD_INDEX needs. "+
			"The count the partition divides by is `strategy.job-total` = %d, so an index outside [1, %d] "+
			"owns a slice that does not exist and some slice that does exist is owned by nobody — with "+
			"every leg green.",
			chdbWorkflowPath, perfGuardsShardJob, legs, len(legs), len(legs), len(legs))
	}

	partitionTest := readFileString(t, shardPartitionTestPath)
	declared := productionShardCountRe.FindStringSubmatch(partitionTest)
	if declared == nil {
		t.Fatalf("%s declares no `const productionShardCount` — the partition's balance and coverage "+
			"assertions no longer say which shard count they hold at", shardPartitionTestPath)
	}
	asserted, err := strconv.Atoi(declared[1])
	if err != nil {
		t.Fatalf("%s productionShardCount = %q is not an integer", shardPartitionTestPath, declared[1])
	}
	if asserted != len(legs) {
		t.Errorf("%s dispatches %d perf-guards shards but %s asserts the partition's coverage, "+
			"disjointness and balance at %d. Those properties then hold for a partition CI never runs: "+
			"the split that ships is unmeasured, and the split that is measured never ships.",
			chdbWorkflowPath, len(legs), shardPartitionTestPath, asserted)
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

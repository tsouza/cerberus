package regression

import (
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The corpus-wide fan-out profiler (test/perf/profile, Component B of the
// perf-assessment framework) only reaches release.yml's RELEASE_REQUIRED_CHECKS
// through a chain of links, and every one of them lives outside the Go package
// the profiler is written in:
//
//	cmd/perf-profile  ->  perf-profile.yml `profile-shard` (the matrix)
//	                  ->  perf-profile.yml `profile` (the aggregator)
//	                  ->  release.yml RELEASE_REQUIRED_CHECKS
//
// Break any link and the lane keeps existing, keeps reporting a green
// `profile` check — release.yml resolves that name by exact text — and stops
// meaning anything. This is the same failure shape
// test/regression/perf_guards_gate_test.go pins for chdb.yml's
// `perf-guards-shard` / `perf-guards` (#2002): both lanes shard the SAME
// corpus through the SAME partition (test/perf/profile.ProfileCorpusShard /
// ShardOf), so the same class of silent breakage applies to both, and
// tsouza/cerberus#2375 is why this one was sharded too — 9 of 11 recent
// non-PR runs of the old single-job lane were killed by their own timeout as
// the corpus grew past what a serial walk could finish in budget.
//
// What this file pins, everything a PR diff CAN break:
//
//   - the JOB NAME release.yml's RELEASE_REQUIRED_CHECKS refers to. Rename
//     the aggregator and the required context is never posted again;
//   - the AGGREGATOR's dependency on the matrix. A `profile` that stopped
//     `needs:`-ing `profile-shard` would report green in seconds over a
//     matrix that failed or never ran — the hollow-green shape;
//   - the AGGREGATOR's verdict script. `needs:` alone does not gate: a
//     matrix rolls up to one `.result` and something has to actually read
//     it, including telling an ordinary-PR skip apart from a heavy run
//     whose matrix crashed before dispatching;
//   - the SHARD WIRING. The partition divides by PERF_SHARD_COUNT, so a
//     count that disagrees with the number of legs actually dispatched
//     leaves part of the corpus profiled by no leg, with every leg still
//     green;
//   - `continue-on-error`, which turns a job into decoration while keeping
//     its name and its logs;
//   - the shard job still building cmd/perf-profile with the `chdb` tag
//     against the full `test/spec` corpus, rather than a narrowed/no-op
//     invocation that would leave the required check green over nothing;
//   - membership in release.yml's RELEASE_REQUIRED_CHECKS, so a publish
//     waits on the same lane a release PR does. release.yml has no
//     `pull_request:` trigger for ordinary PRs, so nothing else checks that
//     at PR time.
const (
	perfProfileWorkflowPath = "../../.github/workflows/perf-profile.yml"

	// The aggregator job branch protection / release.yml names, and the
	// matrix job that does the profiling. Both strings some other system
	// resolves by exact text.
	perfProfileJob      = "profile"
	perfProfileShardJob = "profile-shard"

	// The aggregator's verdict script. Without it the job is an empty green.
	perfProfileAggregateScript = "perf-profile-aggregate.mjs"

	// The shard contract, spelled exactly as the workflow must spell it —
	// identical to perfGuards*'s, since both matrices derive PERF_SHARD_COUNT
	// from `strategy.job-total` for the same reason: it cannot drift from the
	// `shard:` list that way.
	perfProfileShardIndexExpr = "PERF_SHARD_INDEX: ${{ matrix.shard }}"
	perfProfileShardCountExpr = "PERF_SHARD_COUNT: ${{ strategy.job-total }}"

	// What the shard job must still invoke: the chdb-tagged binary, over the
	// whole corpus. A recipe/invocation narrowed to a `-run`-style filter or a
	// smaller `-spec` would leave the required check green while profiling
	// less than the full corpus.
	perfProfileChdbTag  = "-tags chdb"
	perfProfileSpecFlag = "-spec test/spec"
)

// TestPerfProfileLaneRunsTheProfiler pins the in-repo links: the matrix job
// that actually profiles, and the aggregator that lends the whole thing the
// name release.yml resolves.
func TestPerfProfileLaneRunsTheProfiler(t *testing.T) {
	t.Parallel()

	workflow := readFileString(t, perfProfileWorkflowPath)
	shardJob := workflowJobBody(t, workflow, perfProfileShardJob)
	aggregate := workflowJobBody(t, workflow, perfProfileJob)

	for _, want := range []string{perfProfileChdbTag, perfProfileSpecFlag} {
		if !strings.Contains(shardJob, want) {
			t.Errorf("%s job %q no longer contains %q — the shard job may have stopped profiling the "+
				"chdb-tagged binary over the whole corpus. Body:\n%s",
				perfProfileWorkflowPath, perfProfileShardJob, want, shardJob)
		}
	}

	for _, job := range []struct{ name, body string }{
		{perfProfileShardJob, shardJob},
		{perfProfileJob, aggregate},
	} {
		if strings.Contains(job.body, continueOnError) {
			t.Errorf("%s job %q sets %s — it keeps its name and its logs and loses its verdict, which is "+
				"strictly worse than not gating at all because the required check now reports green over "+
				"a lane that did not actually profile anything.",
				perfProfileWorkflowPath, job.name, continueOnError)
		}
	}

	// Read the `needs:` LINE, not the job body: the body also mentions
	// `needs.profile-shard.result` inside the aggregate script's step env, so
	// a plain substring search over the whole body stays green after the
	// dependency edge itself is removed — the exact edit that turns the gate
	// off.
	needsLine := needsDeclarationRe.FindStringSubmatch(aggregate)
	if needsLine == nil || !strings.Contains(needsLine[1], perfProfileShardJob) {
		declared := "(none)"
		if needsLine != nil {
			declared = needsLine[1]
		}
		t.Errorf("%s job %q declares `needs: %s`, which does not include %q. It is the job that posts "+
			"the required context, and without that edge it reports green in seconds no matter what the "+
			"shard matrix did.",
			perfProfileWorkflowPath, perfProfileJob, declared, perfProfileShardJob)
	}
	if !strings.Contains(aggregate, perfProfileAggregateScript) {
		t.Errorf("%s job %q no longer runs %s. `needs:` alone does not gate: a matrix rolls up to one "+
			"result and the aggregate must actually read it — including telling an ordinary-PR skip apart "+
			"from a heavy run whose matrix never dispatched. Body:\n%s",
			perfProfileWorkflowPath, perfProfileJob, perfProfileAggregateScript, aggregate)
	}

	for _, want := range []string{perfProfileShardIndexExpr, perfProfileShardCountExpr} {
		if !strings.Contains(shardJob, want) {
			t.Errorf("%s job %q does not wire `%s`. PERF_SHARD_COUNT must come from `strategy.job-total` "+
				"(the number of legs that actually run), never a repeated literal: a count left behind "+
				"after the `shard:` list changed leaves part of the corpus profiled by no leg, and every "+
				"leg still reports green. Body:\n%s",
				perfProfileWorkflowPath, perfProfileShardJob, want, shardJob)
		}
	}
}

// TestPerfProfileShardMatrixCoversEverySlice pins the shape of the
// profile-shard matrix against the SAME productionShardCount
// test/perf/profile/shard_test.go asserts the partition's balance at — the
// partition function (and the corpus it partitions) is shared with
// `perf-guards-shard`, so one constant serves both lanes' pins. Let the
// matrix's leg count and that constant drift and the balance properties are
// still proven, just about a partition this lane does not run.
//
// Reuses shardMatrixListRe / productionShardCountRe from
// perf_guards_gate_test.go: same file shape, same regexes, same package.
func TestPerfProfileShardMatrixCoversEverySlice(t *testing.T) {
	t.Parallel()

	shardJob := workflowJobBody(t, readFileString(t, perfProfileWorkflowPath), perfProfileShardJob)

	list := shardMatrixListRe.FindStringSubmatch(shardJob)
	if list == nil {
		t.Fatalf("%s job %q declares no `shard: [...]` matrix list. The lane is sharded precisely "+
			"because one process cannot finish it in budget (tsouza/cerberus#2375); a matrix that "+
			"stopped fanning out is the old serial lane back, under a name that says otherwise. "+
			"Body:\n%s", perfProfileWorkflowPath, perfProfileShardJob, shardJob)
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
				perfProfileWorkflowPath, perfProfileShardJob, field, list[1])
		}
		legs = append(legs, n)
	}

	sort.Ints(legs)
	want := make([]int, 0, len(legs))
	for i := range legs {
		want = append(want, i+1)
	}
	if intsString(legs) != intsString(want) {
		t.Errorf("%s job %q declares shards %v, which is not the contiguous 1..%d PERF_SHARD_INDEX "+
			"needs. The count the partition divides by is `strategy.job-total` = %d, so an index "+
			"outside [1, %d] owns a slice that does not exist and some slice that does exist is owned "+
			"by nobody — with every leg green.",
			perfProfileWorkflowPath, perfProfileShardJob, legs, len(legs), len(legs), len(legs))
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
		t.Errorf("%s dispatches %d profile-shard legs but %s asserts the shared partition's coverage, "+
			"disjointness and balance at %d. Those properties then hold for a partition this lane does "+
			"not run at the count it actually runs.",
			perfProfileWorkflowPath, len(legs), shardPartitionTestPath, asserted)
	}
}

// TestReleasePreflightRequiresThePerfProfileLane pins the release half: a
// lane every PR must pass conditionally (release/* PRs) is not automatically
// a lane every publish waits for.
func TestReleasePreflightRequiresThePerfProfileLane(t *testing.T) {
	t.Parallel()

	job := workflowJobBody(t, readFileString(t, releaseWorkflowPath), preflightJob)
	required := requiredChecksFromPreflight(t, job)

	for _, name := range required {
		if name == perfProfileJob {
			return
		}
	}
	t.Errorf("%s job %q does not require %q (required set: %q). The preflight is observation-derived "+
		"for everything outside that set, so a lane that never ran contributes zero problems and the "+
		"release publishes on a silence. release-gate-drift.yml reports the same gap from the other "+
		"side on a schedule; this fails at PR time instead.",
		releaseWorkflowPath, preflightJob, perfProfileJob, required)
}

package regression

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The promql leg of `roundtrip (<ql>)` only blocks a release through a chain of
// links, most of which live outside the Go package the corpus walk itself is
// written in (test/spec/promql, internal/promql):
//
//	TestRoundTripChDB   ->  chdb-roundtrip.mjs  ->  chdb.yml
//	                        `roundtrip-promql-shard` (the matrix)
//	                    ->  chdb.yml `roundtrip-promql` (the aggregator)
//	                    ->  release.yml RELEASE_REQUIRED_CHECKS
//
// Break any link and the corpus walk keeps running, keeps printing nothing
// wrong — and stops gating. tsouza/cerberus#2629 split promql off the
// single-runner `roundtrip` matrix onto its own multi-RUNNER shard matrix
// (chDB threads within a single query, so oversubscribing a single 4-core
// runner past ~3-4 processes hurts rather than helps — the in-runner fan-out
// this replaces had a hard ceiling the growing TXTAR corpus kept hitting).
// That split renames the thing GitHub reports for the matrix legs
// (`roundtrip-promql-shard (n)`, never `roundtrip (promql)`), exactly like
// `perf-guards-shard` / `perf-guards` (test/regression/perf_guards_gate_test.go)
// and `roundtrip-promql-shard` / `roundtrip-promql` are the same shape.
//
// What this file pins is everything a PR diff CAN break:
//
//   - the JOB NAME release.yml's RELEASE_REQUIRED_CHECKS refers to. Rename the
//     aggregator and the context is never posted; the required check sits
//     "Expected" forever;
//   - the RUNNER the lane actually executes on. Narrowing `roundtrip-promql-shard`
//     to skip chdb-roundtrip.mjs leaves the required check green while the
//     corpus walk stops executing;
//   - the AGGREGATOR's dependency on the matrix. A `roundtrip-promql` that
//     stopped `needs:`-ing `roundtrip-promql-shard` would report green in
//     seconds over a matrix that failed;
//   - the SHARD WIRING. The partition composes ROUNDTRIP_SHARD_INDEX /
//     ROUNDTRIP_SHARD_COUNT from `strategy.job-total`, so a count that
//     disagrees with the number of legs actually dispatched leaves part of
//     the corpus owned by no leg;
//   - `continue-on-error`, which turns a job into decoration while keeping its
//     name and its logs;
//   - membership in release.yml's RELEASE_REQUIRED_CHECKS.
const (
	// The job the aggregator posts under, the matrix job that does the work,
	// and the scripts each one runs. All are strings some other system
	// resolves by exact text.
	roundtripPromqlJob             = "roundtrip-promql"
	roundtripPromqlShardJob        = "roundtrip-promql-shard"
	roundtripPromqlRunnerScript    = "chdb-roundtrip.mjs"
	roundtripPromqlAggregateScript = "roundtrip-promql-aggregate.mjs"

	// The exact status-check text release.yml's RELEASE_REQUIRED_CHECKS and
	// .github/ci-lanes.json both resolve by.
	roundtripPromqlContext = "roundtrip (promql)"

	// The shard contract, spelled exactly as the workflow must spell it.
	// ROUNDTRIP_SHARD_COUNT is `strategy.job-total` — the number of legs the
	// matrix really fans out to — precisely so it cannot drift from the
	// `shard:` list.
	roundtripShardIndexExpr = "ROUNDTRIP_SHARD_INDEX: ${{ matrix.shard }}"
	roundtripShardCountExpr = "ROUNDTRIP_SHARD_COUNT: ${{ strategy.job-total }}"
)

// jobNameLineRe matches a job's own `name:` declaration — the two-space-deeper
// key a job body starts with, not any `needs.<job>.result` or `matrix.<key>`
// expression that mentions "name" elsewhere in the body.
var jobNameLineRe = regexp.MustCompile(`(?m)^\s{4}name:\s*(.+?)\s*$`)

// jobIfLineRe matches a job's own `if:` declaration, the same two-space-deeper
// scope jobNameLineRe reads at.
var jobIfLineRe = regexp.MustCompile(`(?m)^\s{4}if:\s*(.+?)\s*$`)

// TestRoundtripPromqlLaneRunsTheCorpus pins the in-repo links of the chain: the
// matrix job that actually runs the corpus walk, the aggregator that lends the
// whole thing the name release.yml resolves, and the shard wiring between them.
func TestRoundtripPromqlLaneRunsTheCorpus(t *testing.T) {
	t.Parallel()

	workflow := readFileString(t, chdbWorkflowPath)
	shardJob := workflowJobBody(t, workflow, roundtripPromqlShardJob)
	aggregate := workflowJobBody(t, workflow, roundtripPromqlJob)

	if !strings.Contains(shardJob, roundtripPromqlRunnerScript) {
		t.Errorf("%s job %q no longer runs %s, so nothing under the required %q context walks the "+
			"promql TXTAR corpus. Body:\n%s",
			chdbWorkflowPath, roundtripPromqlShardJob, roundtripPromqlRunnerScript, roundtripPromqlContext, shardJob)
	}
	for _, job := range []struct{ name, body string }{
		{roundtripPromqlShardJob, shardJob},
		{roundtripPromqlJob, aggregate},
	} {
		if strings.Contains(job.body, continueOnError) {
			t.Errorf("%s job %q sets %s — it keeps its name and its logs and loses its verdict, which is "+
				"strictly worse than not gating at all because the required check now reports green over a "+
				"failing corpus walk.",
				chdbWorkflowPath, job.name, continueOnError)
		}
	}

	// The aggregator posts the required context and runs no corpus fixtures
	// itself, so its ONLY value is the dependency edge and the verdict it
	// derives from it.
	//
	// Read the `needs:` LINE, not the job body. The body also mentions
	// `needs.roundtrip-promql-shard.result` in the step env, so a plain
	// substring search over it stays green after the dependency itself is
	// removed — which is the exact edit that turns this gate off.
	needsLine := needsDeclarationRe.FindStringSubmatch(aggregate)
	if needsLine == nil || !strings.Contains(needsLine[1], roundtripPromqlShardJob) {
		declared := "(none)"
		if needsLine != nil {
			declared = needsLine[1]
		}
		t.Errorf("%s job %q declares `needs: %s`, which does not include %q. It is the job that posts the "+
			"required context and it walks no fixtures of its own, so without that edge it reports green in "+
			"seconds no matter what the shard matrix did.",
			chdbWorkflowPath, roundtripPromqlJob, declared, roundtripPromqlShardJob)
	}
	if !strings.Contains(aggregate, roundtripPromqlAggregateScript) {
		t.Errorf("%s job %q no longer runs %s. `needs:` alone does not gate: a matrix rolls up to one "+
			"result and the aggregate must actually read it — including telling a docs-only skip apart "+
			"from a `changes` job that crashed before deciding. Body:\n%s",
			chdbWorkflowPath, roundtripPromqlJob, roundtripPromqlAggregateScript, aggregate)
	}
	ifMatch := jobIfLineRe.FindStringSubmatch(aggregate)
	if ifMatch == nil || (ifMatch[1] != "always()" && ifMatch[1] != "${{ always() }}") {
		got := "(none)"
		if ifMatch != nil {
			got = ifMatch[1]
		}
		t.Errorf("%s job %q declares `if: %s`, want `always()`. A docs-only or non-heavy (ordinary PR) "+
			"skip of the shard matrix would then skip the aggregator too, and a required context that "+
			"never posts blocks every PR forever.",
			chdbWorkflowPath, roundtripPromqlJob, got)
	}

	nameMatch := jobNameLineRe.FindStringSubmatch(aggregate)
	if nameMatch == nil || nameMatch[1] != roundtripPromqlContext {
		got := "(none)"
		if nameMatch != nil {
			got = nameMatch[1]
		}
		t.Errorf("%s job %q declares `name: %s`, want exactly %q — release.yml's RELEASE_REQUIRED_CHECKS "+
			"and .github/ci-lanes.json both resolve this context by exact text, so any other spelling "+
			"posts a check-run nothing is waiting for while the required one sits Expected forever.",
			chdbWorkflowPath, roundtripPromqlJob, got, roundtripPromqlContext)
	}

	// The shard contract. A leg that divides by the wrong count owns the
	// wrong slice, and the slices no leg owns are walked by nobody.
	for _, want := range []string{roundtripShardIndexExpr, roundtripShardCountExpr} {
		if !strings.Contains(shardJob, want) {
			t.Errorf("%s job %q does not wire `%s`. ROUNDTRIP_SHARD_COUNT must come from "+
				"`strategy.job-total` (the number of legs that actually run), never a repeated literal: "+
				"a count left behind after the `shard:` list changed leaves part of the corpus owned by "+
				"no leg, and every leg still reports green. Body:\n%s",
				chdbWorkflowPath, roundtripPromqlShardJob, want, shardJob)
		}
	}
}

// TestRoundtripPromqlShardMatrixIsContiguous pins the shape of the promql
// shard list: 1..N with no gaps and no repeats, because ROUNDTRIP_SHARD_INDEX
// is that literal. A list of [1, 2, 4] dispatches three legs, so
// `strategy.job-total` is 3, so the partition splits three ways — and the leg
// labelled 4 asks for a slice that does not exist while slice 3 is owned by
// nobody. Every leg still passes.
func TestRoundtripPromqlShardMatrixIsContiguous(t *testing.T) {
	t.Parallel()

	shardJob := workflowJobBody(t, readFileString(t, chdbWorkflowPath), roundtripPromqlShardJob)

	list := shardMatrixListRe.FindStringSubmatch(shardJob)
	if list == nil {
		t.Fatalf("%s job %q declares no `shard: [...]` matrix list. The lane is sharded precisely "+
			"because one runner cannot clear the corpus inside a sane timeout; a matrix that stopped "+
			"fanning out is the single-runner job back, under a name that says otherwise. Body:\n%s",
			chdbWorkflowPath, roundtripPromqlShardJob, shardJob)
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
				chdbWorkflowPath, roundtripPromqlShardJob, field, list[1])
		}
		legs = append(legs, n)
	}
	if len(legs) < 2 {
		t.Fatalf("%s job %q declares %d shard(s); a single-entry list is the unsharded job back "+
			"under a new name, which defeats the reason this split exists", chdbWorkflowPath, roundtripPromqlShardJob, len(legs))
	}

	sort.Ints(legs)
	want := make([]int, len(legs))
	for i := range legs {
		want[i] = i + 1
	}
	equal := len(legs) == len(want)
	if equal {
		for i := range legs {
			if legs[i] != want[i] {
				equal = false
				break
			}
		}
	}
	if !equal {
		t.Errorf("%s job %q declares shards %v, which is not the contiguous 1..%d ROUNDTRIP_SHARD_INDEX "+
			"needs. The count the partition divides by is `strategy.job-total` = %d, so an index outside "+
			"[1, %d] owns a slice that does not exist and some slice that does exist is owned by nobody "+
			"— with every leg green.",
			chdbWorkflowPath, roundtripPromqlShardJob, legs, len(legs), len(legs), len(legs))
	}
}

// TestReleasePreflightRequiresTheRoundtripPromqlLane pins the release half. A
// lane every PR must pass, that a publish does not wait for, ships artifacts
// past a gate the repo already decided matters.
func TestReleasePreflightRequiresTheRoundtripPromqlLane(t *testing.T) {
	t.Parallel()

	job := workflowJobBody(t, readFileString(t, releaseWorkflowPath), preflightJob)
	required := requiredChecksFromPreflight(t, job)

	for _, name := range required {
		if name == roundtripPromqlContext {
			return
		}
	}
	t.Errorf("%s job %q does not require %q (required set: %q). The preflight is observation-derived "+
		"for everything outside that set, so a lane that never ran contributes zero problems and the "+
		"release publishes on a silence.",
		releaseWorkflowPath, preflightJob, roundtripPromqlContext, required)
}

package regression

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// cardinalityUpdateModule is the runner behind `just update-cardinality-baseline`.
const cardinalityUpdateModule = ".github/scripts/cardinality-baseline-update.mjs"

// The env pair the fanned-out legs declare their corpus slice through. These
// spellings are the CONTRACT between the runner, which writes them, and
// test/perf/profile/shard.go's ShardFromEnv, which reads them.
const (
	perfShardIndexEnv = "PERF_SHARD_INDEX"
	perfShardCountEnv = "PERF_SHARD_COUNT"
)

// perfShardSourcePath is the Go side of that contract.
const perfShardSourcePath = "../perf/profile/shard.go"

// The count the perf partition's cover and balance properties are asserted at
// is read through perf_guards_gate_test.go's `shardPartitionTestPath` and
// `productionShardCountRe`. Sharing them is the point: that file pins the CI
// matrix to the same constant, so both fan-outs — the gating one and this
// regeneration one — are held against one declaration rather than against two
// copies that can drift apart while each looks pinned.

// cardinalityUpdateEnvMarker appears on exactly the plan steps that REWRITE the
// baseline. A leg without it asserts the baseline instead, so the regeneration
// silently becomes a check.
const cardinalityUpdateEnvMarker = "UPDATE_CARDINALITY_BASELINE=1"

// cardinalitySealTest is the closing step's assertion: the one that states the
// legs together covered the corpus. Without it a fan-out missing a leg leaves a
// tree that still parses, still looks regenerated, and is stale in the slice
// nobody profiled.
const cardinalitySealTest = "TestCardinalityBaselineCoversTheCorpus"

// cardinalityRatchetTest is the profiling step every leg runs.
const cardinalityRatchetTest = "TestCardinalityRatchet"

// cardinalityRatchetSourcePath holds both of those test functions. The plan
// names them as `-run` REGEXES, and `go test -run '^NoSuchTest$'` prints "no
// tests to run" and exits 0 — so the names have to be pinned against the source
// that defines them, not only against the plan text that mentions them.
const cardinalityRatchetSourcePath = "../perf/cardinality_ratchet_test.go"

// cardinalityUpdatePlan asks the runner for the ordered regeneration plan it
// would execute. One line per step: `<label> <ENV=v>... <command>`.
//
// Pinning the PLAN rather than the recipe text is what keeps this honest: the
// fan-out is a property of the runner, and the plan is that property observed
// rather than re-read from source.
func cardinalityUpdatePlan(t *testing.T) []string {
	t.Helper()

	cmd := exec.Command("node", cardinalityUpdateModule)
	cmd.Dir = repoRootFromRegression
	// CARDINALITY_BASELINE_FANOUT is cleared, not merely left unset: it is a
	// documented override, so a developer who exported it to measure the split
	// on their own machine would otherwise see this pin fail against a leg count
	// nobody committed. An empty value falls through the runner's `if (!raw)`
	// back to the compiled-in default, which is the number under test.
	cmd.Env = append(os.Environ(), "CARDINALITY_UPDATE_PRINT_PLAN=1", "CARDINALITY_BASELINE_FANOUT=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("%s exited %d printing the plan:\n%s", cardinalityUpdateModule, exitErr.ExitCode(), out)
		}
		t.Fatalf("run %s: %v\n%s", cardinalityUpdateModule, err, out)
	}

	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("%s printed an empty plan; this pin cannot see the fan-out", cardinalityUpdateModule)
	}

	return lines
}

// cardinalityLegs splits the plan into the profiling legs and the closing seal
// step, by the test each line runs.
func cardinalityLegs(plan []string) (legs, seal []string) {
	for _, line := range plan {
		switch {
		case strings.Contains(line, cardinalitySealTest):
			seal = append(seal, line)
		case strings.Contains(line, cardinalityRatchetTest):
			legs = append(legs, line)
		}
	}

	return legs, seal
}

// TestCardinalityBaselineFansOutTheProfilePass pins the split that makes
// `just update-cardinality-baseline` finish in a fraction of its former serial
// pass, and — more importantly — pins the ways that split can silently stop
// working while every leg still exits 0.
//
// The first is a fan-out that collapsed to one command: the recipe is then
// exactly as slow as it was before, and nothing says so. The second is a leg
// list that is not the contiguous 1..N the partition divides by — an index
// outside [1, N] names a corpus slice that does not exist, so that leg
// regenerates nothing while the slice it should have owned is owned by nobody,
// and the recipe returns a diff that only LOOKS complete.
func TestCardinalityBaselineFansOutTheProfilePass(t *testing.T) {
	t.Parallel()

	legs, _ := cardinalityLegs(cardinalityUpdatePlan(t))

	if len(legs) < 2 {
		t.Fatalf("the plan runs %s as %d command(s). It profiles the ~950-fixture corpus through "+
			"chDB one fixture at a time; a fan-out that collapsed to one command is the serial pass "+
			"back, under a name that says otherwise.\n%s",
			cardinalityRatchetTest, len(legs), strings.Join(legs, "\n"))
	}

	var indices, counts []int
	for _, line := range legs {
		index, hasIndex := planEnvValue(line, perfShardIndexEnv)
		count, hasCount := planEnvValue(line, perfShardCountEnv)
		if !hasIndex || !hasCount {
			t.Fatalf("a leg declares no %s / %s pair. A leg with no partition profiles the WHOLE "+
				"corpus, so it redoes every sibling's work — and it also hands the writer records "+
				"outside its own slice.\n%s", perfShardIndexEnv, perfShardCountEnv, line)
		}
		i, err := strconv.Atoi(index)
		if err != nil {
			t.Fatalf("plan line declares a non-integer %s=%q:\n%s", perfShardIndexEnv, index, line)
		}
		c, err := strconv.Atoi(count)
		if err != nil {
			t.Fatalf("plan line declares a non-integer %s=%q:\n%s", perfShardCountEnv, count, line)
		}
		indices = append(indices, i)
		counts = append(counts, c)
	}

	sorted := append([]int(nil), indices...)
	sort.Ints(sorted)
	want := make([]int, 0, len(sorted))
	for i := range sorted {
		want = append(want, i+1)
	}
	if fmt.Sprint(sorted) != fmt.Sprint(want) {
		t.Errorf("the fan-out declares legs %v, which is not the contiguous 1..%d the partition "+
			"divides by. An index outside [1, %d] owns a slice that does not exist, and some slice "+
			"that does exist is owned by nobody — with every leg green.\n%s",
			sorted, len(sorted), len(sorted), strings.Join(legs, "\n"))
	}
	for _, c := range counts {
		if c != len(legs) {
			t.Errorf("a leg declares %s=%d while the fan-out dispatches %d commands. The count is "+
				"what the partition divides by, so a mismatch re-slices the corpus into pieces the "+
				"leg list does not cover.\n%s", perfShardCountEnv, c, len(legs), strings.Join(legs, "\n"))
		}
	}

	for _, line := range legs {
		if !strings.Contains(line, cardinalityUpdateEnvMarker) {
			t.Errorf("a leg does not carry %s, so it ASSERTS the baseline instead of rewriting it — "+
				"the regeneration silently becomes a check.\n%s", cardinalityUpdateEnvMarker, line)
		}
	}
}

// TestCardinalityBaselinePlanSealsTheFanOut pins the closing step, which is the
// only thing in the plan that can state a fact no single leg can: that the legs
// together covered the corpus.
//
// Each leg writes and prunes only its own slice, so a leg that was never
// dispatched — a fan-out silently narrowed, a crashed process, a re-run at a
// different N — cannot corrupt anything, but it also leaves its slice at
// whatever the last regeneration wrote. That tree still parses and still looks
// regenerated. Drop the seal and the staleness waits for CI to find.
func TestCardinalityBaselinePlanSealsTheFanOut(t *testing.T) {
	t.Parallel()

	plan := cardinalityUpdatePlan(t)
	legs, seal := cardinalityLegs(plan)

	if len(seal) != 1 {
		t.Fatalf("the plan runs %s as %d step(s), want exactly 1. Without it, a fan-out missing a "+
			"leg returns a diff that only looks complete.\n%s",
			cardinalitySealTest, len(seal), strings.Join(plan, "\n"))
	}
	if strings.Contains(seal[0], cardinalityUpdateEnvMarker) {
		t.Errorf("the seal step carries %s, so it REGENERATES rather than asserting — a check that "+
			"rewrites what it checks cannot fail.\n%s", cardinalityUpdateEnvMarker, seal[0])
	}

	// The seal reads the tree the legs write, so it has to run last.
	sealAt, lastLegAt := -1, -1
	for i, line := range plan {
		if strings.Contains(line, cardinalitySealTest) {
			sealAt = i
		}
		if strings.Contains(line, cardinalityRatchetTest) {
			lastLegAt = i
		}
	}
	if len(legs) > 0 && sealAt < lastLegAt {
		t.Errorf("the seal runs at plan step %d, before the last profiling leg at step %d. It would "+
			"then assert the PREVIOUS regeneration's tree.\n%s", sealAt, lastLegAt, strings.Join(plan, "\n"))
	}
}

// TestCardinalityPlanNamesTestsThatExist closes the gap between the plan's
// `-run` regexes and the Go functions they are supposed to select.
//
// `go test -run '^NoSuchTest$' ./pkg/` prints "no tests to run" and exits **0**.
// Every other assertion in this file matches those names against the plan TEXT,
// which a rename satisfies on both sides at once, so on its own the pin cannot
// see the failure it most needs to:
//
//   - rename the seal and the closing step passes vacuously — the fan-out keeps
//     its only check that a leg never ran, in name only.
//   - rename the ratchet and all N legs no-op, the seal then passes over the
//     untouched (still-matching) tree, and `just update-cardinality-baseline`
//     becomes a complete no-op that exits 0 and prints an empty diff — which
//     reads exactly like "no drift".
//
// Both are the repo's hollow-green class: a lane that reports success while
// measuring nothing.
func TestCardinalityPlanNamesTestsThatExist(t *testing.T) {
	t.Parallel()

	src := readFileString(t, cardinalityRatchetSourcePath)
	for _, name := range []string{cardinalityRatchetTest, cardinalitySealTest} {
		if !strings.Contains(src, "func "+name+"(") {
			t.Errorf("the regeneration plan selects %q with `go test -run`, but %s defines no such "+
				"function. `-run` matching nothing exits 0 after running nothing, so this renames a "+
				"step into a no-op that still reports success.", name, cardinalityRatchetSourcePath)
		}
	}
}

// TestCardinalityFanOutMatchesTheGoPartition pins the two halves of the split
// against each other: the .mjs side that WRITES the shard variables and the Go
// side that READS them.
//
// This is the hollow-green case, and it is invisible from either side alone.
// Rename the pair on one side only and profile.ShardFromEnv finds nothing, falls
// back to the whole corpus by design, and every one of the N legs profiles all
// ~950 fixtures: N times the work, and — because each leg then hands the writer
// the whole corpus while declaring a slice of it — N legs refused by the
// writer's own ownership check, which is loud but is not the failure anyone
// would go looking for.
func TestCardinalityFanOutMatchesTheGoPartition(t *testing.T) {
	t.Parallel()

	src := readFileString(t, perfShardSourcePath)
	for _, env := range []string{perfShardIndexEnv, perfShardCountEnv} {
		if !strings.Contains(src, `"`+env+`"`) {
			t.Errorf("%s does not read %q, but the regeneration plan sets it. A leg whose partition "+
				"variables the profiler does not read falls back to the WHOLE corpus.",
				perfShardSourcePath, env)
		}
	}

	partitionTest := readFileString(t, shardPartitionTestPath)
	declared := productionShardCountRe.FindStringSubmatch(partitionTest)
	if declared == nil {
		t.Fatalf("%s declares no `const productionShardCount` — the partition's cover and balance "+
			"assertions no longer say which leg count they hold at", shardPartitionTestPath)
	}
	asserted, err := strconv.Atoi(declared[1])
	if err != nil {
		t.Fatalf("%s declares a non-integer productionShardCount %q", shardPartitionTestPath, declared[1])
	}

	legs, _ := cardinalityLegs(cardinalityUpdatePlan(t))
	if len(legs) != asserted {
		t.Errorf("the regeneration fans out to %d legs while %s asserts the partition's cover and "+
			"balance at %d. Those properties are what say N legs divide the corpus into N non-empty, "+
			"roughly equal pieces — evidence about this fan-out only while the two agree.",
			len(legs), shardPartitionTestPath, asserted)
	}
}

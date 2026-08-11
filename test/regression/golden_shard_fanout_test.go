package regression

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The promql shard's two generators, in the order the shard declares them:
// `TestLower` writes each fixture's `-- sql --`, and `TestRoundTripChDB` then
// EXECUTES that freshly-written SQL to fill `-- expected_rows --`. Only the
// second is fanned out; the ordering between them is a data dependency and the
// fan-out must not dissolve it.
// promqlShard is the shard whose round-trip generator is fanned out.
const promqlShard = "promql"

const (
	promqlLowerGenerator     = "^TestLower$"
	promqlRoundTripGenerator = "^TestRoundTripChDB$"
)

// The env pair the fanned-out legs declare their corpus slice through. These
// spellings are the CONTRACT between .github/scripts/lib/golden-shards.mjs,
// which writes them, and test/spec/shard.go's ShardFromEnv, which reads them.
const (
	specShardIndexEnv = "SPEC_SHARD_INDEX"
	specShardCountEnv = "SPEC_SHARD_COUNT"
)

// specShardSourcePath is the Go side of that contract.
const specShardSourcePath = "../spec/shard.go"

// specShardTestPath declares the count the partition's cover and balance
// properties are asserted at.
const specShardTestPath = "../spec/shard_test.go"

// goldenUpdateShardCountRe reads that count.
var goldenUpdateShardCountRe = regexp.MustCompile(`(?m)^const goldenUpdateShardCount = (\d+)`)

// planLegs returns the (index, count) pairs of every plan line belonging to the
// named shard that runs the given generator, in plan order.
//
// Scoping to the shard is not cosmetic: `TestRoundTripChDB` is the generator
// name in ALL THREE head shards, so an unscoped match would pull in the logql
// and traceql commands — neither of which is fanned out — and read their absent
// partition as a promql leg that forgot to declare one.
func planLegs(t *testing.T, plan []string, shard, generator string) (indices, counts []int, lines []string) {
	t.Helper()

	for _, line := range plan {
		if !planLineIsShard(line, shard) || !strings.Contains(line, generator) {
			continue
		}
		lines = append(lines, line)
		index, hasIndex := planEnvValue(line, specShardIndexEnv)
		count, hasCount := planEnvValue(line, specShardCountEnv)
		if !hasIndex || !hasCount {
			continue
		}
		i, err := strconv.Atoi(index)
		if err != nil {
			t.Fatalf("plan line declares a non-integer %s=%q:\n%s", specShardIndexEnv, index, line)
		}
		c, err := strconv.Atoi(count)
		if err != nil {
			t.Fatalf("plan line declares a non-integer %s=%q:\n%s", specShardCountEnv, count, line)
		}
		indices = append(indices, i)
		counts = append(counts, c)
	}
	return indices, counts, lines
}

// planLineIsShard reports whether a `<stage> <shard> <command>` plan line
// belongs to the named shard.
func planLineIsShard(line, shard string) bool {
	fields := strings.Fields(line)
	return len(fields) > 1 && fields[1] == shard
}

// planEnvValue extracts `NAME=value` from a plan line.
func planEnvValue(line, name string) (string, bool) {
	for _, field := range strings.Fields(line) {
		if after, ok := strings.CutPrefix(field, name+"="); ok {
			return after, true
		}
	}
	return "", false
}

// TestGoldenUpdateFansOutTheRoundTripBody pins the split that makes
// `just update-golden promql` finish in minutes rather than in one long serial
// walk, and — more importantly — pins the two ways that split can silently stop
// working while every leg still exits 0.
//
// The first is a fan-out that collapsed back to one command: the recipe is then
// exactly as slow as it was before, and nothing says so. The second is a leg
// list that is not the contiguous 1..N the partition divides by — an index
// outside [1, N] names a corpus slice that does not exist, so that leg
// regenerates nothing while the slice it should have owned is owned by nobody,
// and `update-golden` returns a diff that only LOOKS complete. That is the #1573
// staleness trap re-entered through the parallelism.
func TestGoldenUpdateFansOutTheRoundTripBody(t *testing.T) {
	t.Parallel()

	plan := goldenUpdatePlan(t)
	indices, counts, lines := planLegs(t, plan, promqlShard, promqlRoundTripGenerator)

	if len(lines) < 2 {
		t.Fatalf("the `all` plan runs %s as %d command(s). It walks the ~570-fixture PromQL corpus "+
			"through chDB and is the longest single step of the recipe; a fan-out that collapsed to "+
			"one command is the serial walk back, under a name that says otherwise.\n%s",
			promqlRoundTripGenerator, len(lines), strings.Join(lines, "\n"))
	}
	if len(indices) != len(lines) {
		t.Fatalf("%d of the %d %s commands declare no %s / %s pair. A leg with no partition walks the "+
			"WHOLE corpus, so every such leg redoes all the work the split exists to divide — and "+
			"several processes rewrite the same fixture files at once.\n%s",
			len(lines)-len(indices), len(lines), promqlRoundTripGenerator,
			specShardIndexEnv, specShardCountEnv, strings.Join(lines, "\n"))
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
			sorted, len(sorted), len(sorted), strings.Join(lines, "\n"))
	}
	for _, c := range counts {
		if c != len(lines) {
			t.Errorf("a leg declares %s=%d while the fan-out dispatches %d commands. The count is what "+
				"the partition divides by, so a mismatch re-slices the corpus into pieces the leg list "+
				"does not cover.\n%s", specShardCountEnv, c, len(lines), strings.Join(lines, "\n"))
		}
	}

	// The fan-out parallelises ONE generator; it must not reorder the shard's
	// generators against each other. TestRoundTripChDB executes the `-- sql --`
	// TestLower writes, so a leg that started first would execute the previous
	// run's SQL — or, for a brand-new fixture, an empty section.
	lowerAt := -1
	for i, line := range plan {
		if planLineIsShard(line, promqlShard) && strings.Contains(line, promqlLowerGenerator) {
			lowerAt = i
			break
		}
	}
	if lowerAt == -1 {
		t.Fatalf("the `all` plan runs no %s step, so nothing writes the `-- sql --` the round-trip "+
			"legs execute:\n%s", promqlLowerGenerator, strings.Join(plan, "\n"))
	}
	for i, line := range plan {
		if planLineIsShard(line, promqlShard) && strings.Contains(line, promqlRoundTripGenerator) && i < lowerAt {
			t.Errorf("a round-trip leg runs at plan step %d, before %s at step %d. The legs execute "+
				"the SQL that step writes.\n%s", i, promqlLowerGenerator, lowerAt, line)
		}
	}

	for _, line := range lines {
		if !strings.Contains(line, goldenUpdateEnvMarker) {
			t.Errorf("a fanned-out leg does not carry %s, so it ASSERTS the goldens instead of "+
				"rewriting them — the regeneration silently becomes a check.\n%s",
				goldenUpdateEnvMarker, line)
		}
	}
}

// TestGoldenUpdateFanOutMatchesTheGoPartition pins the two halves of the split
// against each other: the .mjs side that WRITES the shard variables and the Go
// side that READS them.
//
// This is the hollow-green case, and it is invisible from either side alone.
// Rename the pair on one side only and `spec.ShardFromEnv` finds nothing, falls
// back to the whole corpus by design, and every one of the N legs walks all ~570
// fixtures: N times the work, N concurrent writers on each fixture file, and N
// green legs. Nothing in the output distinguishes it from a working fan-out
// except the wall clock.
func TestGoldenUpdateFanOutMatchesTheGoPartition(t *testing.T) {
	t.Parallel()

	src := readFileString(t, specShardSourcePath)
	for _, env := range []string{specShardIndexEnv, specShardCountEnv} {
		if !strings.Contains(src, `"`+env+`"`) {
			t.Errorf("%s does not read %q, but the regeneration plan sets it. A leg whose partition "+
				"variables the walk does not read falls back to the WHOLE corpus and reports green "+
				"after redoing every other leg's work.", specShardSourcePath, env)
		}
	}

	partitionTest := readFileString(t, specShardTestPath)
	declared := goldenUpdateShardCountRe.FindStringSubmatch(partitionTest)
	if declared == nil {
		t.Fatalf("%s declares no `const goldenUpdateShardCount` — the partition's cover and balance "+
			"assertions no longer say which leg count they hold at", specShardTestPath)
	}
	asserted, err := strconv.Atoi(declared[1])
	if err != nil {
		t.Fatalf("%s declares a non-integer goldenUpdateShardCount %q", specShardTestPath, declared[1])
	}

	_, _, lines := planLegs(t, goldenUpdatePlan(t), promqlShard, promqlRoundTripGenerator)
	if len(lines) != asserted {
		t.Errorf("the plan fans out to %d legs while %s asserts the partition's cover and balance at "+
			"%d. Those properties are evidence about this recipe only while the two agree: let them "+
			"drift and the balance floor is measured at a split nobody runs.",
			len(lines), specShardTestPath, asserted)
	}
}

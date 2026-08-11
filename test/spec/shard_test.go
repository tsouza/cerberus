// Untagged on purpose — see shard.go's header. The partition is the one part of
// the sharded walk that decides WHAT gets walked, so it is also the one part
// whose bug is invisible: a shard that silently holds the wrong slice rewrites
// some fixtures' goldens twice and others' never, and every leg exits 0.

package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// shardedCorpusDir is the fixture directory the golden-update fan-out splits:
// the PromQL corpus, by far the largest of the three heads, and the one whose
// chdb-tagged round-trip dominates `just update-golden promql`'s wall clock.
// Asserting the partition against the REAL corpus (rather than synthetic names
// that would balance by construction) is what makes the balance floor evidence.
const shardedCorpusDir = "promql"

// goldenUpdateShardCount is the number of concurrent legs
// `.github/scripts/lib/golden-shards.mjs` fans the promql shard's
// `TestRoundTripChDB` generator out to. The balance assertion below is only
// meaningful at the count the fan-out actually runs, and
// test/regression/golden_shard_fanout_test.go pins the .mjs side to this same
// number from the other direction.
const goldenUpdateShardCount = 4

// maxShardImbalance is how far the biggest leg may exceed the smallest, as a
// ratio, over the real corpus at goldenUpdateShardCount. A hash partition
// balances STATISTICALLY, not exactly: dropping ~570 names into 4 buckets is a
// binomial with mean ≈142 and σ = √(569·¼·¾) ≈ 10.3, so a ±3σ swing spans
// ≈111–173 and a max/min ratio near 1.6 is ordinary sampling noise, not a
// defect. Today's corpus sits at 1.12 (153/137); a bound fitted to that would
// fire on the next enrolment wave for no reason, which is how a gate becomes a
// rubber stamp.
//
// So this is not a balance TARGET. It is the floor under "the partition still
// spreads at all" — a constant hash, a modulus over the wrong value, or a name
// set FNV clusters would land far outside it, and each of those turns the
// fan-out's wall-clock back into one big leg while every leg reports green.
const maxShardImbalance = 2.0

// pinnedAssignments locks the partition FUNCTION, not just its shape. Every
// property below (total cover, disjointness, balance) is equally satisfied by a
// different hash — so swapping FNV-1a for anything else would keep them all
// green while re-shuffling the entire corpus across legs. That re-shuffle is
// harmless to correctness and expensive in confusion: every leg's wall-clock
// moves at once and no diff explains it. These are goldens; changing them is the
// deliberate act of changing the partition.
//
// They are also the cross-check on the perf lane's own pin
// (test/perf/profile/shard_test.go), which hashes head-QUALIFIED ids through
// this same function: two id shapes, one arithmetic.
//
// One fixture per leg, deliberately: a set that happened to cluster on one leg
// would be satisfied by a partition that had DEGENERATED to that leg — the
// constant-hash case, which is exactly what the balance floor below and these
// goldens exist to catch between them.
var pinnedAssignments = map[string]int{
	"abs_metric":              1,
	"agg_by_dotted_source":    2,
	"absent_binary_no_series": 3,
	"absent_with_series":      4,
}

func TestShardOfIsPinnedToFNV1a(t *testing.T) {
	t.Parallel()

	for name, want := range pinnedAssignments {
		if got := ShardOf(name, goldenUpdateShardCount); got != want {
			t.Errorf("ShardOf(%q, %d) = %d, want %d — the partition function changed. Every fixture in "+
				"the corpus just moved to a different leg; per-leg wall-clock will move with it and "+
				"nothing in the diff will say why. If the change is deliberate, re-pin these goldens.",
				name, goldenUpdateShardCount, got, want)
		}
	}
}

func TestShardOfIsInRangeAndDeterministic(t *testing.T) {
	t.Parallel()

	for _, count := range []int{1, 2, 3, 8, 17} {
		for i := range 500 {
			name := fmt.Sprintf("synthetic_fixture_%d", i)
			got := ShardOf(name, count)
			if got < 1 || got > count {
				t.Fatalf("ShardOf(%q, %d) = %d, outside [1, %d]", name, count, got, count)
			}
			if again := ShardOf(name, count); again != got {
				t.Fatalf("ShardOf(%q, %d) is not deterministic: %d then %d", name, count, got, again)
			}
		}
	}
}

// TestWalkShardIsATotalDisjointCover is THE correctness assertion for the
// fan-out: the union of what the legs walk must reproduce, exactly, what the
// unsharded walk walks.
//
// Both halves matter and both are silent. A fixture no leg holds never has its
// `-- expected_rows --` regenerated, so `just update-golden promql` returns a
// diff that looks complete and is not — the #1573 staleness trap re-entered
// through the parallelism. A fixture two legs hold is regenerated twice, by two
// processes, into the same file.
//
// This walks the corpus for real (through WalkShard itself, not through a
// re-derivation of its filter) so the property is asserted about the code path
// the fan-out runs.
func TestWalkShardIsATotalDisjointCover(t *testing.T) {
	t.Parallel()

	whole := walkedNames(t, "whole", WholeCorpus)
	if len(whole) == 0 {
		t.Fatalf("the unsharded walk over %s visited no fixtures; every assertion below would hold "+
			"vacuously", shardedCorpusDir)
	}

	seen := make(map[string]int, len(whole))
	for index := 1; index <= goldenUpdateShardCount; index++ {
		shard := Shard{Index: index, Count: goldenUpdateShardCount}
		for _, name := range walkedNames(t, fmt.Sprintf("leg%d", index), shard) {
			seen[name]++
		}
	}

	var missing, duplicated []string
	for _, name := range whole {
		switch seen[name] {
		case 1:
		case 0:
			missing = append(missing, name)
		default:
			duplicated = append(duplicated, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d fixture(s) belong to NO leg of a %d-way split, so no process regenerates their "+
			"goldens and `update-golden` reports a diff that only looks complete: %v",
			len(missing), goldenUpdateShardCount, truncate(missing))
	}
	if len(duplicated) > 0 {
		t.Errorf("%d fixture(s) belong to MORE THAN ONE leg, so two concurrent processes rewrite the "+
			"same fixture file: %v", len(duplicated), truncate(duplicated))
	}
	for name := range seen {
		if !contains(whole, name) {
			t.Errorf("a leg walked %q, which the unsharded walk does not visit at all", name)
		}
	}
}

// TestWalkShardBalancesTheRealCorpus asserts the split actually splits. A
// partition that put 500 of 569 fixtures on one leg satisfies "total disjoint
// cover" perfectly and delivers none of the wall-clock win the fan-out exists
// for — the run still takes as long as its biggest leg.
func TestWalkShardBalancesTheRealCorpus(t *testing.T) {
	t.Parallel()

	names := corpusNames(t)
	sizes := make([]int, goldenUpdateShardCount+1)
	for _, name := range names {
		sizes[ShardOf(name, goldenUpdateShardCount)]++
	}

	minSize, maxSize := len(names), 0
	for index := 1; index <= goldenUpdateShardCount; index++ {
		minSize = min(minSize, sizes[index])
		maxSize = max(maxSize, sizes[index])
	}
	if minSize == 0 {
		t.Fatalf("a leg holds ZERO of the %d corpus fixtures (sizes %v) — that process boots chDB, "+
			"regenerates nothing and exits 0", len(names), sizes[1:])
	}
	if ratio := float64(maxSize) / float64(minSize); ratio > maxShardImbalance {
		t.Errorf("leg sizes %v over %d fixtures are imbalanced %.2fx (max %d / min %d), above the %.2fx "+
			"floor — the partition has stopped spreading, so the fan-out's wall-clock is the biggest "+
			"leg rather than the average one", sizes[1:], len(names), ratio, maxSize, minSize, maxShardImbalance)
	}
}

// TestWalkShardDefaultsToTheWholeCorpus pins the fallback that keeps a
// hand-typed `go test -tags chdb ./test/spec/promql/` — and every other
// spec.Walk caller, none of which declare a partition — walking everything.
func TestWalkShardDefaultsToTheWholeCorpus(t *testing.T) {
	t.Parallel()

	if !WholeCorpus.IsWhole() {
		t.Fatal("WholeCorpus.IsWhole() is false")
	}
	walked := walkedNames(t, "default", WholeCorpus)
	corpus := corpusNames(t)
	if len(walked) != len(corpus) {
		t.Fatalf("the whole-corpus walk visited %d of %d fixtures in %s",
			len(walked), len(corpus), shardedCorpusDir)
	}
}

// TestWalkShardRefusesASliceThatHoldsNothing pins the loud end of the failure
// mode above: a count wider than the corpus leaves legs holding nothing, and a
// leg that walks nothing finishes fast and green. FilterShard is the decision
// WalkShard fatals on, asserted here directly because a t.Fatalf inside a
// subtest cannot be observed from the parent without a subprocess.
func TestWalkShardRefusesASliceThatHoldsNothing(t *testing.T) {
	t.Parallel()

	only := []string{"the_only_fixture"}
	empty := 0
	const impossiblyWideSplit = 8
	for index := 1; index <= impossiblyWideSplit; index++ {
		shard := Shard{Index: index, Count: impossiblyWideSplit}
		if len(FilterShard(shard, only, func(s string) string { return s })) == 0 {
			empty++
		}
	}
	if empty != impossiblyWideSplit-1 {
		t.Fatalf("splitting a 1-fixture corpus %d ways left %d empty slices, want %d — the guard in "+
			"WalkShard fires on exactly this condition", impossiblyWideSplit, empty, impossiblyWideSplit-1)
	}
}

func TestShardFromEnvNames(t *testing.T) {
	const (
		indexEnv = "CERBERUS_TEST_SHARD_INDEX"
		countEnv = "CERBERUS_TEST_SHARD_COUNT"
	)

	cases := []struct {
		name        string
		index       *string
		count       *string
		want        Shard
		wantErrLike string
	}{
		{name: "unset defaults to the whole corpus", want: WholeCorpus},
		{name: "a declared leg", index: ptr("3"), count: ptr("8"), want: Shard{Index: 3, Count: 8}},
		{name: "single-leg split is the whole corpus", index: ptr("1"), count: ptr("1"), want: Shard{Index: 1, Count: 1}},
		{name: "last leg", index: ptr("8"), count: ptr("8"), want: Shard{Index: 8, Count: 8}},

		{name: "index without count", index: ptr("3"), wantErrLike: "must be set together"},
		{name: "count without index", count: ptr("8"), wantErrLike: "must be set together"},
		{name: "non-integer count", index: ptr("1"), count: ptr("eight"), wantErrLike: "is not an integer"},
		{name: "non-integer index", index: ptr("first"), count: ptr("8"), wantErrLike: "is not an integer"},
		{name: "zero count", index: ptr("1"), count: ptr("0"), wantErrLike: "must be >= 1"},
		{name: "index above count", index: ptr("9"), count: ptr("8"), wantErrLike: "outside"},
		{name: "zero index", index: ptr("0"), count: ptr("8"), wantErrLike: "outside"},
		{name: "negative index", index: ptr("-1"), count: ptr("8"), wantErrLike: "outside"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No t.Parallel(): t.Setenv and the process environment are
			// global, so these subtests must not overlap.
			if tc.index != nil {
				t.Setenv(indexEnv, *tc.index)
			} else {
				os.Unsetenv(indexEnv)
			}
			if tc.count != nil {
				t.Setenv(countEnv, *tc.count)
			} else {
				os.Unsetenv(countEnv)
			}

			got, err := ShardFromEnvNames(indexEnv, countEnv)
			if tc.wantErrLike != "" {
				if err == nil {
					t.Fatalf("ShardFromEnvNames() = %v, want an error containing %q", got, tc.wantErrLike)
				}
				if !strings.Contains(err.Error(), tc.wantErrLike) {
					t.Fatalf("ShardFromEnvNames() error = %q, want it to contain %q", err, tc.wantErrLike)
				}
				return
			}
			if err != nil {
				t.Fatalf("ShardFromEnvNames() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ShardFromEnvNames() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestShardFromEnvReadsTheSpecContract pins which variables the spec corpus is
// partitioned by. Sharing PERF_SHARD_* with the perf lane would let a
// `perf-guards-shard (3)` leg silently narrow an unrelated spec walk.
func TestShardFromEnvReadsTheSpecContract(t *testing.T) {
	t.Setenv(ShardIndexEnv, "3")
	t.Setenv(ShardCountEnv, "8")

	got, err := ShardFromEnv()
	if err != nil {
		t.Fatalf("ShardFromEnv() unexpected error: %v", err)
	}
	if want := (Shard{Index: 3, Count: 8}); got != want {
		t.Fatalf("ShardFromEnv() = %+v, want %+v — it does not read %s / %s",
			got, want, ShardIndexEnv, ShardCountEnv)
	}
	if ShardIndexEnv == "PERF_SHARD_INDEX" || ShardCountEnv == "PERF_SHARD_COUNT" {
		t.Errorf("the spec partition reads the perf lane's variables (%s / %s); a perf shard leg would "+
			"then narrow every spec walk in the same process", ShardIndexEnv, ShardCountEnv)
	}
}

// walkedNames runs WalkShard over the sharded corpus and returns the fixture
// names it visited, in visit order.
func walkedNames(t *testing.T, label string, shard Shard) []string {
	t.Helper()

	var names []string
	t.Run(label, func(t *testing.T) {
		WalkShard(t, shardedCorpusDir, shard, func(_ *testing.T, c *Case) {
			names = append(names, c.Name)
		})
	})
	return names
}

// corpusNames is the fixture roster read straight off disk, independent of
// WalkShard, so the cover assertion has something to compare against that
// WalkShard did not produce.
func corpusNames(t *testing.T) []string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(shardedCorpusDir, "*"+fixtureExt))
	if err != nil {
		t.Fatalf("glob %s: %v", shardedCorpusDir, err)
	}
	if len(matches) == 0 {
		t.Fatalf("no fixtures in %s; the partition assertions would hold vacuously", shardedCorpusDir)
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, trimFixtureExt(filepath.Base(m)))
	}
	sort.Strings(names)
	return names
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func truncate(names []string) []string {
	const maxListed = 10
	if len(names) <= maxListed {
		return names
	}
	return append(names[:maxListed:maxListed], "…")
}

func ptr(s string) *string { return &s }

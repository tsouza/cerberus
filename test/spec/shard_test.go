// Untagged on purpose — see shard.go's header. The partition is the one part of
// the sharded walk that decides WHAT gets walked, so it is also the one part
// whose bug is invisible: a shard that silently holds the wrong slice rewrites
// some fixtures' goldens twice and others' never, and every leg exits 0.

package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

	legs := make([][]string, 0, goldenUpdateShardCount)
	for index := 1; index <= goldenUpdateShardCount; index++ {
		shard := Shard{Index: index, Count: goldenUpdateShardCount}
		legs = append(legs, walkedNames(t, fmt.Sprintf("leg%d", index), shard))
	}
	assertTotalDisjointCover(t, fmt.Sprintf("fixture(s) of a %d-way split", goldenUpdateShardCount), whole, legs)
}

// assertTotalDisjointCover fails t unless every name in whole appears in
// exactly one of legs and no leg holds a name outside whole. what names the
// members in the failure messages.
func assertTotalDisjointCover(t *testing.T, what string, whole []string, legs [][]string) {
	t.Helper()

	seen := make(map[string]int, len(whole))
	for _, leg := range legs {
		for _, name := range leg {
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
		t.Errorf("%d %s belong to NO leg, so nothing runs them and every leg still reports green: %v",
			len(missing), what, truncate(missing))
	}
	if len(duplicated) > 0 {
		t.Errorf("%d %s belong to MORE THAN ONE leg, so they run twice (and a golden writer rewrites "+
			"the same file from two processes): %v", len(duplicated), what, truncate(duplicated))
	}
	for name := range seen {
		if !contains(whole, name) {
			t.Errorf("a leg ran %q, which the unsharded run does not hold at all", name)
		}
	}
}

// subtestShardCaseCount is the size of the synthetic case table the
// SubtestShard partition is asserted over: as large as the largest case table
// a self-sharding internal/promql test loops over, so the cover holds for
// tables of that size and not only for small ones.
const subtestShardCaseCount = 120

// subtestShardMaxLegs is the widest split the SubtestShard cover is asserted
// at. Every count from one leg up to it is checked, which spans the
// roundtrip-promql matrix's leg count with room for it to grow, and includes
// splits wider than some case tables, where a leg legitimately creates none
// of a table's subtests.
const subtestShardMaxLegs = 32

// TestSubtestShardIsATotalDisjointCover is the correctness assertion for the
// self-sharding tests built on SubtestShard: across all legs of a split, each
// subtest of a case table is created on exactly one leg. It drives the real
// code path — NewSubtestShard reading the shard environment, and Run creating
// subtests through t.Run — and records which subtest bodies actually ran.
func TestSubtestShardIsATotalDisjointCover(t *testing.T) {
	whole := make([]string, subtestShardCaseCount)
	for i := range whole {
		whole[i] = fmt.Sprintf("op%d/grouping%d", i%7, i)
	}
	for count := 1; count <= subtestShardMaxLegs; count++ {
		legs := make([][]string, 0, count)
		for index := 1; index <= count; index++ {
			legs = append(legs, createdSubtests(t, Shard{Index: index, Count: count}, whole))
		}
		assertTotalDisjointCover(t, fmt.Sprintf("subtest(s) of a %d-way split", count), whole, legs)
	}
}

// TestSubtestShardRefusesADuplicateName pins the guard that keeps the
// partition well defined: two cases with one name would share one id.
func TestSubtestShardRefusesADuplicateName(t *testing.T) {
	s := &SubtestShard{t: t, shard: WholeCorpus, seen: map[string]bool{}}
	if _, err := s.claim("sum/by(bucket)"); err != nil {
		t.Fatalf("first claim of a name: %v", err)
	}
	if _, err := s.claim("sum/by(bucket)"); err == nil {
		t.Fatal("claiming the same subtest name twice succeeded; two cases would then share one partition id")
	}
}

// createdSubtests runs every name of table through a SubtestShard under the
// shard environment of leg shard, inside one parent subtest, and returns the
// names whose bodies ran.
func createdSubtests(t *testing.T, shard Shard, table []string) []string {
	t.Helper()

	var created []string
	t.Run(fmt.Sprintf("leg%d_of_%d", shard.Index, shard.Count), func(t *testing.T) {
		t.Setenv(ShardIndexEnv, fmt.Sprint(shard.Index))
		t.Setenv(ShardCountEnv, fmt.Sprint(shard.Count))
		s := NewSubtestShard(t)
		for _, name := range table {
			s.Run(name, func(sub *testing.T) {
				if want := t.Name() + "/" + name; sub.Name() != want {
					sub.Errorf("subtest ran as %q, want %q", sub.Name(), want)
				}
				created = append(created, name)
			})
		}
	})
	return created
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
		{name: "a declared leg", index: new("3"), count: new("8"), want: Shard{Index: 3, Count: 8}},
		{name: "single-leg split is the whole corpus", index: new("1"), count: new("1"), want: Shard{Index: 1, Count: 1}},
		{name: "last leg", index: new("8"), count: new("8"), want: Shard{Index: 8, Count: 8}},

		{name: "index without count", index: new("3"), wantErrLike: "must be set together"},
		{name: "count without index", count: new("8"), wantErrLike: "must be set together"},
		{name: "non-integer count", index: new("1"), count: new("eight"), wantErrLike: "is not an integer"},
		{name: "non-integer index", index: new("first"), count: new("8"), wantErrLike: "is not an integer"},
		{name: "zero count", index: new("1"), count: new("0"), wantErrLike: "must be >= 1"},
		{name: "index above count", index: new("9"), count: new("8"), wantErrLike: "outside"},
		{name: "zero index", index: new("0"), count: new("8"), wantErrLike: "outside"},
		{name: "negative index", index: new("-1"), count: new("8"), wantErrLike: "outside"},
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
	return slices.Contains(haystack, needle)
}

func truncate(names []string) []string {
	const maxListed = 10
	if len(names) <= maxListed {
		return names
	}
	return append(names[:maxListed:maxListed], "…")
}

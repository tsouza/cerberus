// Untagged on purpose — see shard.go's header. The partition is the one part
// of the profiler that decides WHAT gets profiled, so it is also the one part
// whose bug is invisible: a shard that silently holds nothing reports a green
// `perf-guards-shard (n)` over a corpus slice no ratchet ever looked at.

package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corpusRosterDir is the committed cardinality baseline: one JSON shard file
// per profiled fixture, keyed by fixture id. It is the corpus roster in the
// only form reachable without libchdb.so, which is what lets this untagged test
// assert the partition's balance against the REAL ~950-fixture id set rather
// than against synthetic ids that would balance by construction.
const corpusRosterDir = "../cardinality-baseline"

// productionShardCount is the number of legs BOTH sharded CI consumers of this
// package's partition fan out to: `perf-guards-shard` in
// .github/workflows/chdb.yml (the pass/fail cardinality ratchet), and
// `profile-shard` in .github/workflows/perf-profile.yml (the breadth report,
// tsouza/cerberus#2375). They share one partition function (ShardOf) and one
// corpus, so one count serves both. The balance assertion below is only
// meaningful at the count CI actually runs, and
// test/regression/perf_guards_gate_test.go + perf_profile_gate_test.go each
// pin their own workflow to this same number from the other side.
const productionShardCount = 8

// maxShardImbalance is how far the biggest shard may exceed the smallest, as a
// ratio, over the real corpus at productionShardCount. A hash partition
// balances STATISTICALLY, not exactly, and the spread is larger than intuition
// suggests: dropping ~950 ids into 8 buckets is a binomial with mean ≈119 and
// σ = √(949·⅛·⅞) ≈ 10.2, so a ±3σ swing spans ≈88–149 and a max/min ratio near
// 1.7 is ordinary sampling noise, not a defect. Today's corpus sits at 1.36
// (137/101); a bound fitted to that would fire on the next enrolment wave for
// no reason, which is how a gate becomes a rubber stamp.
//
// So this is not a balance TARGET. It is the floor under "the partition still
// spreads at all" — a constant hash, a modulus taken over the wrong value, or a
// fixture-id set FNV clusters would land far outside it, and each of those turns
// the lane's wall-clock back into one big shard while every leg reports green.
const maxShardImbalance = 2.0

// pinnedAssignments locks the partition FUNCTION, not just its shape. Every
// property below (total cover, disjointness, balance) is equally satisfied by a
// different hash — so swapping FNV-1a for anything else would keep them all
// green while re-shuffling the entire corpus across shards. That re-shuffle is
// harmless to correctness and expensive in confusion: every shard's wall-clock
// moves at once and no diff explains it. These are goldens; changing them is
// the deliberate act of changing the partition.
var pinnedAssignments = map[string]int{
	"promql/absent_aggregate_no_series": 5,
	"logql/agg_by_severity":             5,
	"traceql/and_two_attrs":             8,
	"metadata/series_no_bounds":         3,
}

func TestShardOfIsPinnedToFNV1a(t *testing.T) {
	t.Parallel()

	for id, want := range pinnedAssignments {
		if got := ShardOf(id, productionShardCount); got != want {
			t.Errorf("ShardOf(%q, %d) = %d, want %d — the partition function changed. Every fixture in "+
				"the corpus just moved to a different shard; per-shard wall-clock will move with it and "+
				"nothing in the diff will say why. If the change is deliberate, re-pin these goldens.",
				id, productionShardCount, got, want)
		}
	}
}

func TestShardOfIsInRangeAndDeterministic(t *testing.T) {
	t.Parallel()

	for _, count := range []int{1, 2, 3, 8, 17} {
		for i := range 500 {
			id := fmt.Sprintf("promql/synthetic_fixture_%d", i)
			got := ShardOf(id, count)
			if got < 1 || got > count {
				t.Fatalf("ShardOf(%q, %d) = %d, outside [1, %d]", id, count, got, count)
			}
			if again := ShardOf(id, count); again != got {
				t.Fatalf("ShardOf(%q, %d) is not deterministic: %d then %d", id, count, got, again)
			}
		}
	}
}

// TestPartitionIsATotalDisjointCover is the coverage assertion. Union over
// every shard must reproduce the corpus EXACTLY: a fixture in no shard is never
// ratcheted (a silent hole, the failure this whole split could most easily
// introduce), and a fixture in two shards is profiled twice and, worse, makes
// the added/removed diff disagree between two legs.
func TestPartitionIsATotalDisjointCover(t *testing.T) {
	t.Parallel()

	ids := corpusRoster(t)
	seen := make(map[string]int, len(ids))
	for index := 1; index <= productionShardCount; index++ {
		s := Shard{Index: index, Count: productionShardCount}
		for _, id := range ids {
			if s.Holds(id) {
				seen[id]++
			}
		}
	}

	var missing, duplicated []string
	for _, id := range ids {
		switch seen[id] {
		case 1:
		case 0:
			missing = append(missing, id)
		default:
			duplicated = append(duplicated, id)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d fixture(s) belong to NO shard, so no `perf-guards-shard` leg profiles them and the "+
			"ratchet passes over them in silence: %v", len(missing), truncate(missing))
	}
	if len(duplicated) > 0 {
		t.Errorf("%d fixture(s) belong to MORE THAN ONE shard: %v", len(duplicated), truncate(duplicated))
	}
}

// TestPartitionBalancesTheRealCorpus asserts the split actually splits. A
// partition that put 900 of 950 fixtures on one shard satisfies "total disjoint
// cover" perfectly and delivers none of the wall-clock win the split exists for.
func TestPartitionBalancesTheRealCorpus(t *testing.T) {
	t.Parallel()

	ids := corpusRoster(t)
	sizes := make([]int, productionShardCount+1)
	for _, id := range ids {
		sizes[ShardOf(id, productionShardCount)]++
	}

	minSize, maxSize := len(ids), 0
	for index := 1; index <= productionShardCount; index++ {
		minSize = min(minSize, sizes[index])
		maxSize = max(maxSize, sizes[index])
	}
	if minSize == 0 {
		t.Fatalf("a shard holds ZERO of the %d corpus fixtures (sizes %v) — that leg boots chDB, profiles "+
			"nothing and reports green", len(ids), sizes[1:])
	}
	if ratio := float64(maxSize) / float64(minSize); ratio > maxShardImbalance {
		t.Errorf("shard sizes %v over %d fixtures are imbalanced %.2fx (max %d / min %d), above the %.2fx "+
			"floor — the partition has stopped spreading, so the lane's wall-clock is the biggest shard "+
			"rather than the average one", sizes[1:], len(ids), ratio, maxSize, minSize, maxShardImbalance)
	}
}

// TestWholeCorpusHoldsEverything pins the default: with no partition declared,
// nothing is filtered out. This is what keeps `just update-cardinality-baseline`
// and the nightly perf-profile lane whole-corpus operations.
func TestWholeCorpusHoldsEverything(t *testing.T) {
	t.Parallel()

	if !WholeCorpus.IsWhole() {
		t.Fatal("WholeCorpus.IsWhole() is false")
	}
	for _, id := range corpusRoster(t) {
		if !WholeCorpus.Holds(id) {
			t.Fatalf("WholeCorpus does not hold %q", id)
		}
	}
}

func TestShardFromEnv(t *testing.T) {
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
				t.Setenv(ShardIndexEnv, *tc.index)
			} else {
				os.Unsetenv(ShardIndexEnv)
			}
			if tc.count != nil {
				t.Setenv(ShardCountEnv, *tc.count)
			} else {
				os.Unsetenv(ShardCountEnv)
			}

			got, err := ShardFromEnv()
			if tc.wantErrLike != "" {
				if err == nil {
					t.Fatalf("ShardFromEnv() = %v, want an error containing %q", got, tc.wantErrLike)
				}
				if !strings.Contains(err.Error(), tc.wantErrLike) {
					t.Fatalf("ShardFromEnv() error = %q, want it to contain %q", err, tc.wantErrLike)
				}
				return
			}
			if err != nil {
				t.Fatalf("ShardFromEnv() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ShardFromEnv() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFilterShardKeepsExactlyTheShardsFixtures(t *testing.T) {
	t.Parallel()

	ids := corpusRoster(t)
	total := 0
	for index := 1; index <= productionShardCount; index++ {
		s := Shard{Index: index, Count: productionShardCount}
		kept := FilterShard(s, ids, func(id string) string { return id })
		for _, id := range kept {
			if !s.Holds(id) {
				t.Fatalf("FilterShard kept %q which %v does not hold", id, s)
			}
		}
		total += len(kept)
	}
	if total != len(ids) {
		t.Errorf("FilterShard over all %d shards kept %d of %d fixtures", productionShardCount, total, len(ids))
	}
	if got := FilterShard(WholeCorpus, ids, func(id string) string { return id }); len(got) != len(ids) {
		t.Errorf("FilterShard(WholeCorpus) kept %d of %d fixtures", len(got), len(ids))
	}
}

// corpusRoster reads the fixture ids out of the committed baseline tree.
func corpusRoster(t *testing.T) []string {
	t.Helper()

	var ids []string
	err := filepath.WalkDir(corpusRosterDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		var entry struct {
			Fixture string `json:"fixture"`
		}
		if jerr := json.Unmarshal(raw, &entry); jerr != nil {
			return fmt.Errorf("%s: %w", path, jerr)
		}
		if entry.Fixture == "" {
			return fmt.Errorf("%s: baseline shard carries no `fixture` key", path)
		}
		ids = append(ids, entry.Fixture)
		return nil
	})
	if err != nil {
		t.Fatalf("read corpus roster from %s: %v", corpusRosterDir, err)
	}
	if len(ids) == 0 {
		t.Fatalf("corpus roster at %s is empty — the balance and coverage assertions below would pass "+
			"vacuously over zero fixtures", corpusRosterDir)
	}
	return ids
}

func truncate(ids []string) []string {
	const maxListed = 10
	if len(ids) <= maxListed {
		return ids
	}
	return append(ids[:maxListed:maxListed], "…")
}

func ptr(s string) *string { return &s }

// Deliberately NOT `//go:build chdb`. The corpus PARTITION is pure arithmetic
// over a fixture name — no chDB, no seeding, no SQL — so keeping it untagged
// means its unit test (shard_test.go) runs in the ordinary `check` lane, where a
// partition bug is caught in seconds, instead of only inside the chdb-tagged
// lane the partition exists to speed up.

package spec

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The shard contract between a corpus-walking test and whatever launched it.
//
// The names are SPEC_-prefixed rather than shared with the perf lane's
// PERF_SHARD_INDEX / PERF_SHARD_COUNT (test/perf/profile/shard.go) on purpose:
// the two partitions cover different corpora at different counts, and a single
// `go test ./...` can link both. One shared pair of variables would let a
// `perf-guards-shard (3)` leg silently narrow an unrelated spec walk to a third
// of its fixtures — a green run over a slice nobody asked to slice.
const (
	ShardIndexEnv = "SPEC_SHARD_INDEX"
	ShardCountEnv = "SPEC_SHARD_COUNT"
)

// shardHashMask clears the top bit of the 32-bit FNV digest, bounding it below
// 2^31 so it converts to a non-negative int on every platform. See [ShardOf]
// for why the fold is done in signed arithmetic rather than unsigned.
const shardHashMask = 1<<31 - 1

// Shard is a deterministic partition of a fixture corpus into Count disjoint
// pieces, of which this process owns piece Index. Index is 1-BASED so it reads
// the same as the leg's own name (`promql[3/8]` owns Shard{Index: 3, Count: 8})
// — an off-by-one between the two would silently walk one slice twice and never
// walk another.
type Shard struct {
	Index int
	Count int
}

// WholeCorpus is the unpartitioned corpus: one shard holding everything. It is
// the default whenever the environment declares no partition, so a hand-run
// `go test -tags chdb ./test/spec/promql/` still walks every fixture exactly as
// it did before sharding existed.
var WholeCorpus = Shard{Index: 1, Count: 1}

// IsWhole reports whether this shard is the entire corpus.
func (s Shard) IsWhole() bool { return s.Count <= 1 }

// Holds reports whether fixtureID belongs to this shard.
func (s Shard) Holds(fixtureID string) bool {
	if s.IsWhole() {
		return true
	}
	return ShardOf(fixtureID, s.Count) == s.Index
}

func (s Shard) String() string {
	if s.IsWhole() {
		return "whole corpus"
	}
	return fmt.Sprintf("shard %d/%d", s.Index, s.Count)
}

// ShardOf is the partition function: the 1-based shard a fixture id belongs to
// when the corpus is split Count ways.
//
// It hashes the fixture id rather than slicing the SORTED corpus into Count
// contiguous runs, and that choice is the whole point. A positional partition
// re-assigns roughly half the corpus every time a fixture is added or removed
// anywhere but the end — so a shard's membership, and therefore its wall-clock,
// jitters run to run and a slow shard can never be attributed to anything. A
// hash of the id is a pure function of that ONE fixture: enrolling 150 new
// promql fixtures leaves every existing fixture on the shard it was already on,
// and the new ones spread evenly across all of them.
//
// FNV-1a is chosen for being stable ACROSS Go releases (it is a specified
// algorithm with fixed constants, unlike maphash, whose seed is deliberately
// per-process) — a partition that changed under a toolchain bump would silently
// re-shuffle the corpus and move every leg's wall-clock at once, with no diff to
// explain it.
func ShardOf(fixtureID string, count int) int {
	if count <= 1 {
		return 1
	}
	h := fnv.New32a()
	// hash.Hash's Write never returns an error (documented on the interface).
	_, _ = h.Write([]byte(fixtureID))
	// Fold the digest into the shard range with the modulo taken in SIGNED
	// arithmetic, so no int -> uint32 conversion happens at all.
	//
	// The natural spelling, `h.Sum32() % uint32(count)`, converts count — a
	// signed int — to unsigned, and gosec rejects it as G115. That rejection
	// is right rather than pedantic: the conversion silently WRAPS a negative
	// count into a huge modulus instead of failing, so a caller that got its
	// count from somewhere unvalidated would get a plausible-looking shard
	// number out of a nonsensical partition.
	//
	// Masking off the digest's sign bit first is what removes the question.
	// The result is below 2^31, which fits in an int on every platform Go
	// supports (int is at least 32 bits), so the remaining `int(...)` widens
	// rather than truncates and the modulo is plain int arithmetic against
	// count itself. The cost is one bit of hash width — irrelevant for a
	// partition into single digits of shards.
	return int(h.Sum32()&shardHashMask)%count + 1
}

// ShardFromEnv reads the partition this process owns from [ShardIndexEnv] /
// [ShardCountEnv]. See [ShardFromEnvNames] for the validation contract.
func ShardFromEnv() (Shard, error) {
	return ShardFromEnvNames(ShardIndexEnv, ShardCountEnv)
}

// ShardFromEnvNames reads the partition this process owns from the named pair of
// environment variables. The pair is a parameter because two corpora are
// partitioned by this function under two different contracts — the spec corpus
// under SPEC_SHARD_*, the profiled perf corpus under PERF_SHARD_* — and each
// must be able to declare its own without inheriting the other's.
//
// Both variables unset means [WholeCorpus] — the hand-run default. Anything else
// is validated strictly and returns an error rather than falling back, because
// every failure mode here is silent in the direction that matters: a shard that
// quietly widens to the whole corpus turns an 8-way split back into eight serial
// full runs, and a shard that quietly narrows to nothing reports a green run
// over a corpus slice nothing walked.
func ShardFromEnvNames(indexEnv, countEnv string) (Shard, error) {
	rawIndex, hasIndex := os.LookupEnv(indexEnv)
	rawCount, hasCount := os.LookupEnv(countEnv)

	switch {
	case !hasIndex && !hasCount:
		return WholeCorpus, nil
	case hasIndex != hasCount:
		// Half a contract is worse than none: with only COUNT set the index
		// is unknowable, and with only INDEX set the natural fallback
		// (count=1) walks the whole corpus on every leg — the exact
		// serial-run-times-N outcome the split exists to remove, reported as
		// N green legs.
		return Shard{}, fmt.Errorf(
			"%s and %s must be set together (got %s=%q, %s=%q); a half-declared shard either walks "+
				"the whole corpus on every leg or walks none of it, and both report green",
			indexEnv, countEnv, indexEnv, rawIndex, countEnv, rawCount,
		)
	}

	count, err := strconv.Atoi(rawCount)
	if err != nil {
		return Shard{}, fmt.Errorf("%s=%q is not an integer: %w", countEnv, rawCount, err)
	}
	index, err := strconv.Atoi(rawIndex)
	if err != nil {
		return Shard{}, fmt.Errorf("%s=%q is not an integer: %w", indexEnv, rawIndex, err)
	}
	if count < 1 {
		return Shard{}, fmt.Errorf("%s=%d must be >= 1", countEnv, count)
	}
	if index < 1 || index > count {
		return Shard{}, fmt.Errorf("%s=%d is outside [1, %s=%d] — the corpus slice it names does not exist, "+
			"so this leg would walk nothing and pass",
			indexEnv, index, countEnv, count)
	}
	return Shard{Index: index, Count: count}, nil
}

// FilterShard returns the subset of items this shard holds, preserving order.
func FilterShard[T any](s Shard, items []T, idOf func(T) string) []T {
	if s.IsWhole() {
		return items
	}
	out := make([]T, 0, len(items)/s.Count+1)
	for _, it := range items {
		if s.Holds(idOf(it)) {
			out = append(out, it)
		}
	}
	return out
}

// FilterShardMap returns the entries of a fixture-id-keyed map this shard holds.
func FilterShardMap[V any](s Shard, m map[string]V) map[string]V {
	if s.IsWhole() {
		return m
	}
	out := make(map[string]V, len(m)/s.Count+1)
	for id, v := range m {
		if s.Holds(id) {
			out[id] = v
		}
	}
	return out
}

// WalkShard is [Walk] over one slice of a fixture directory: it loads every
// *.txtar under dir (non-recursive), keeps the ones this shard holds, and calls
// fn inside a t.Run subtest named after each. Fixtures are visited in sorted
// order for stable output, exactly as Walk visits them.
//
// This is the ONLY safe way to spread a chdb-tagged corpus walk over more CPUs.
// The in-process route — `t.Parallel()` on the subtests — races two goroutines
// against chdb-go's process-wide `globalSession` singleton (chdb/session.go),
// which runner_chdb.go's OpenChDB isolates fixtures against by switching
// DATABASE on that one shared session (#1987), not by tearing it down — two
// concurrent USE/CREATE DATABASE/DROP DATABASE calls against the SAME session
// would still race each other exactly like two concurrent chdb_connect() calls
// would have. Sharding at the PROCESS level sidesteps that entirely: each leg
// is a separate address space and so gets its own singleton.
//
// The partition is over the fixture NAME (the basename without `.txtar`), which
// is unique within one corpus directory, so a leg's membership does not move
// when a sibling fixture is added or removed.
func WalkShard(t *testing.T, dir string, shard Shard, fn func(t *testing.T, c *Case)) {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, "*"+fixtureExt))
	if err != nil {
		t.Fatalf("spec.WalkShard: glob %q: %v", dir, err)
	}
	if len(matches) == 0 {
		t.Fatalf("spec.WalkShard: no *.txtar fixtures in %s", dir)
	}
	sort.Strings(matches)

	mine := FilterShard(shard, matches, fixtureIDOfPath)
	if len(mine) == 0 {
		// A leg that walks nothing passes in the time it takes to boot, and
		// reads as a fast green rather than as a slice nobody covered. With a
		// count above the corpus size that is arithmetic, not a bug in the
		// partition — but it is still a partition nobody should be running.
		t.Fatalf("spec.WalkShard: %v holds none of the %d fixtures in %s — this leg would assert "+
			"nothing and report green; the split is wider than the corpus", shard, len(matches), dir)
	}

	for _, m := range mine {
		c, err := Load(m)
		if err != nil {
			t.Fatalf("spec.WalkShard: load %s: %v", m, err)
		}
		t.Run(c.Name, func(t *testing.T) {
			fn(t, c)
		})
	}
}

// fixtureIDOfPath is the id [ShardOf] hashes: the fixture's basename without the
// `.txtar` suffix, matching the name Walk gives its subtest.
func fixtureIDOfPath(path string) string {
	return trimFixtureExt(filepath.Base(path))
}

// SubtestShard creates the subtests of one top-level test that this process's
// shard holds, so a test that loops over a large case table splits its cost
// across the CI legs instead of landing whole on one of them.
//
// It decides which subtests to CREATE: a subtest another leg holds is never
// started on this one, so nothing is skipped and every leg's report lists only
// the work it did. A test built on it must run on every leg — it is a
// self-sharding test, registered in SELF_SHARDING_TESTS in
// .github/scripts/chdb-roundtrip.mjs, which test/regression pins against every
// test that reads the shard environment.
//
// The id a subtest is hashed by is the top-level test's name, a slash, and the
// subtest's name, so two tests whose case tables share subtest names still
// spread independently. Only the top-level segment of the parent's name enters
// the id: a SubtestShard opened inside a subtest whose own name differed per
// leg would otherwise hash each case differently on each leg.
//
// Subtests run sequentially in the parent's goroutine, exactly as plain t.Run
// would; see [WalkShard] for why a chdb-tagged corpus must not use t.Parallel.
type SubtestShard struct {
	t       *testing.T
	shard   Shard
	seen    map[string]bool
	created int
}

// NewSubtestShard reads the shard this process owns from [ShardIndexEnv] /
// [ShardCountEnv] (the whole case table when neither is set) and returns the
// creator of t's subtests. On cleanup it logs how many of the table's
// subtests this leg created.
func NewSubtestShard(t *testing.T) *SubtestShard {
	t.Helper()

	shard, err := ShardFromEnv()
	if err != nil {
		t.Fatalf("spec.NewSubtestShard: %v", err)
	}
	s := &SubtestShard{t: t, shard: shard, seen: map[string]bool{}}
	t.Cleanup(func() {
		t.Logf("%v: created %d of %d subtests", s.shard, s.created, len(s.seen))
	})
	return s
}

// Run creates subtest name with body fn when this shard holds it. Every
// subtest name of the table is passed through Run on every leg, and a name
// passed twice fails the test: two cases sharing a name would share an id, so
// the partition could not tell them apart.
func (s *SubtestShard) Run(name string, fn func(t *testing.T)) {
	s.t.Helper()

	held, err := s.claim(name)
	if err != nil {
		s.t.Fatalf("spec.SubtestShard: %v", err)
	}
	if !held {
		return
	}
	s.created++
	s.t.Run(name, fn)
}

// claim records name as a member of the case table and reports whether this
// shard holds it.
func (s *SubtestShard) claim(name string) (bool, error) {
	if s.seen[name] {
		return false, fmt.Errorf("%s declares subtest %q twice; each case of a self-sharding test needs its own name", s.t.Name(), name)
	}
	s.seen[name] = true
	topLevel, _, _ := strings.Cut(s.t.Name(), "/")
	return s.shard.Holds(topLevel + "/" + name), nil
}

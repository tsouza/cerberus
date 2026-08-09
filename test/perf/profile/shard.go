// Deliberately NOT `//go:build chdb`. Every other file in this package needs
// libchdb.so to compile, but the corpus PARTITION is pure arithmetic over a
// fixture id — no chDB, no seeding, no SQL. Keeping it untagged means its unit
// test (shard_test.go) runs in the ordinary `check` lane, where a partition bug
// is caught in seconds, instead of only inside the chdb-tagged lane the
// partition exists to speed up.

package profile

import (
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
)

// The shard contract between CI and this package. `perf-guards-shard` in
// .github/workflows/chdb.yml sets both on every matrix leg — INDEX from
// `matrix.shard`, COUNT from `strategy.job-total`, so the count is derived from
// the matrix itself and cannot drift from the number of legs that actually run.
//
// Naming mirrors the CRAWL_SHARD_INDEX / CRAWL_SHARD_COUNT contract
// .github/scripts/dashboard-matrix.mjs documents for the e2e crawl frontier.
const (
	ShardIndexEnv = "PERF_SHARD_INDEX"
	ShardCountEnv = "PERF_SHARD_COUNT"
)

// shardHashMask clears the top bit of the 32-bit FNV digest, bounding it below
// 2^31 so it converts to a non-negative int on every platform. See [ShardOf]
// for why the fold is done in signed arithmetic rather than unsigned.
const shardHashMask = 1<<31 - 1

// Shard is a deterministic partition of the profiled corpus into Count disjoint
// pieces, of which this process owns piece Index. Index is 1-BASED so it reads
// the same as the shard's own CI check name (`perf-guards-shard (3)` owns
// Shard{Index: 3}) — an off-by-one between the two would silently double-profile
// one slice and never profile another.
type Shard struct {
	Index int
	Count int
}

// WholeCorpus is the unpartitioned corpus: one shard holding everything. It is
// the default whenever the environment declares no partition, so a local
// `just perf-chdb`, `just update-cardinality-baseline` and the nightly
// perf-profile lane all behave exactly as they did before sharding existed.
var WholeCorpus = Shard{Index: 1, Count: 1}

// IsWhole reports whether this shard is the entire corpus. The baseline WRITER
// must refuse to run on anything else: `mustWrite` prunes every shard file it
// was not handed, so regenerating from a partial profile would silently delete
// the rows belonging to the other shards.
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
// It hashes the fixture ID rather than slicing the SORTED corpus into Count
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
// re-shuffle the corpus and make the ratchet's added/removed diff scream on a
// PR that touched nothing.
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
	// number out of a nonsensical partition. Restating `count > 0` next to the
	// conversion does not help, because the value being converted is still of
	// a type that can hold negatives.
	//
	// Masking off the digest's sign bit first is what removes the question.
	// The result is below 2^31, which fits in an int on every platform Go
	// supports (int is at least 32 bits), so the remaining `int(...)` widens
	// rather than truncates and the modulo is plain int arithmetic against
	// count itself. The cost is one bit of hash width — irrelevant for a
	// partition into single digits of shards.
	return int(h.Sum32()&shardHashMask)%count + 1
}

// ShardFromEnv reads the partition this process owns from the environment.
//
// Both variables unset means [WholeCorpus] — the local and nightly default.
// Anything else is validated strictly and returns an error rather than falling
// back, because every failure mode here is silent in the direction that matters:
// a shard that quietly widens to the whole corpus turns an 8-way split back into
// eight serial full runs, and a shard that quietly narrows to nothing reports a
// green check over a corpus slice nothing profiled.
func ShardFromEnv() (Shard, error) {
	rawIndex, hasIndex := os.LookupEnv(ShardIndexEnv)
	rawCount, hasCount := os.LookupEnv(ShardCountEnv)

	switch {
	case !hasIndex && !hasCount:
		return WholeCorpus, nil
	case hasIndex != hasCount:
		// Half a contract is worse than none: with only COUNT set the index
		// is unknowable, and with only INDEX set the natural fallback
		// (count=1) profiles the whole corpus on every leg — the exact
		// serial-run-times-N outcome the split exists to remove, reported as
		// N green checks.
		return Shard{}, fmt.Errorf(
			"%s and %s must be set together (got %s=%q, %s=%q); a half-declared shard either profiles "+
				"the whole corpus on every leg or profiles none of it, and both report green",
			ShardIndexEnv, ShardCountEnv, ShardIndexEnv, rawIndex, ShardCountEnv, rawCount,
		)
	}

	count, err := strconv.Atoi(rawCount)
	if err != nil {
		return Shard{}, fmt.Errorf("%s=%q is not an integer: %w", ShardCountEnv, rawCount, err)
	}
	index, err := strconv.Atoi(rawIndex)
	if err != nil {
		return Shard{}, fmt.Errorf("%s=%q is not an integer: %w", ShardIndexEnv, rawIndex, err)
	}
	if count < 1 {
		return Shard{}, fmt.Errorf("%s=%d must be >= 1", ShardCountEnv, count)
	}
	if index < 1 || index > count {
		return Shard{}, fmt.Errorf("%s=%d is outside [1, %s=%d] — the corpus slice it names does not exist, "+
			"so this leg would profile nothing and pass",
			ShardIndexEnv, index, ShardCountEnv, count)
	}
	return Shard{Index: index, Count: count}, nil
}

// FilterShardMap returns the entries of a fixture-id-keyed map this shard
// holds. It is the baseline side of the partition: a ratchet leg must compare
// its slice of the corpus against the SAME slice of the committed baseline, or
// every fixture the leg does not own reads as removed.
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

// FilterShard returns the subset of ids this shard holds, preserving order.
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

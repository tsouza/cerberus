// Deliberately NOT `//go:build chdb`. Every other file in this package needs
// libchdb.so to compile, but the corpus PARTITION is pure arithmetic over a
// fixture id — no chDB, no seeding, no SQL. Keeping it untagged means its unit
// test (shard_test.go) runs in the ordinary `check` lane, where a partition bug
// is caught in seconds, instead of only inside the chdb-tagged lane the
// partition exists to speed up.

package profile

import "github.com/tsouza/cerberus/test/spec"

// This file is the PERF lane's binding of the shared corpus partition, not a
// second implementation of it. The partition function itself lives in
// test/spec/shard.go, next to the TXTAR corpus it splits, and is shared with the
// spec lane's own process-level split (`spec.WalkShard`).
//
// One implementation is load-bearing rather than tidy. The properties this
// package's shard_test.go asserts — total disjoint cover, balance, and the
// PINNED FNV-1a assignments — are the properties both lanes depend on, and a
// second copy of the arithmetic would satisfy them on the day it was written and
// then drift silently: the copies only disagree about which fixture lands where,
// which no test that treats each partition in isolation can see.
const (
	ShardIndexEnv = "PERF_SHARD_INDEX"
	ShardCountEnv = "PERF_SHARD_COUNT"
)

// Shard is a deterministic partition of the profiled corpus into Count disjoint
// pieces, of which this process owns piece Index. Index is 1-BASED so it reads
// the same as the shard's own CI check name (`perf-guards-shard (3)` owns
// Shard{Index: 3}) — an off-by-one between the two would silently double-profile
// one slice and never profile another.
type Shard = spec.Shard

// WholeCorpus is the unpartitioned corpus: one shard holding everything. It is
// the default whenever the environment declares no partition, so a local
// `just perf-chdb`, `just update-cardinality-baseline` and the nightly
// perf-profile lane all behave exactly as they did before sharding existed.
//
// The baseline WRITER must refuse to run on anything else (`IsWhole`):
// `mustWrite` prunes every shard file it was not handed, so regenerating from a
// partial profile would silently delete the rows belonging to the other shards.
var WholeCorpus = spec.WholeCorpus

// ShardOf is the partition function: the 1-based shard a fixture id belongs to
// when the corpus is split Count ways. See [spec.ShardOf] for why it hashes the
// id (FNV-1a) rather than slicing the sorted corpus positionally.
func ShardOf(fixtureID string, count int) int { return spec.ShardOf(fixtureID, count) }

// ShardFromEnv reads the partition this process owns from PERF_SHARD_INDEX /
// PERF_SHARD_COUNT. `perf-guards-shard` in .github/workflows/chdb.yml sets both
// on every matrix leg — INDEX from `matrix.shard`, COUNT from
// `strategy.job-total`, so the count is derived from the matrix itself and
// cannot drift from the number of legs that actually run.
//
// Both unset means [WholeCorpus]; anything else is validated strictly rather
// than falling back, because both silent failure modes report green. See
// [spec.ShardFromEnvNames].
func ShardFromEnv() (Shard, error) { return spec.ShardFromEnvNames(ShardIndexEnv, ShardCountEnv) }

// FilterShard returns the subset of ids this shard holds, preserving order.
func FilterShard[T any](s Shard, items []T, idOf func(T) string) []T {
	return spec.FilterShard(s, items, idOf)
}

// FilterShardMap returns the entries of a fixture-id-keyed map this shard
// holds. It is the baseline side of the partition: a ratchet leg must compare
// its slice of the corpus against the SAME slice of the committed baseline, or
// every fixture the leg does not own reads as removed.
func FilterShardMap[V any](s Shard, m map[string]V) map[string]V {
	return spec.FilterShardMap(s, m)
}

package chclient

// settingSkipUnavailableShards / settingFallbackToStaleReplicasForDistributedQueries
// name the two ClickHouse settings that decide what a Distributed-engine
// query does when it cannot reach every shard/replica it fans out to.
// Cerberus issue #3078 pins BOTH to the fail-loud value, and this file is
// that pin, cited inline exactly as the issue's own acceptance criteria
// require:
//
//   - skip_unavailable_shards=0 — ClickHouse's own default is already 0
//     (verified against docs/reference/settings/session-settings/
//     skip-unavailable-shards.mdx in github.com/ClickHouse/ClickHouse), so
//     this pin changes no server behavior today; it is stamped explicitly
//     so the decision survives a future ClickHouse default change rather
//     than being left to chance. With it, a Distributed data shard that
//     has NO reachable replica at all aborts the whole query with
//     ALL_CONNECTION_TRIES_FAILED (chclient's own *ShardUnavailableError,
//     see distributed_shard_error.go) instead of silently answering from
//     whichever shards happened to respond (skip_unavailable_shards=1's
//     documented "returns a result based on partial data" behavior) — a
//     silent partial answer is a worse failure mode for a correctness-
//     focused gateway than a loud one.
//   - fallback_to_stale_replicas_for_distributed_queries=0 — the OPPOSITE
//     of ClickHouse's own default, which is 1 (verified against
//     docs/reference/settings/session-settings/other.mdx: "Forces a query
//     to an out-of-date replica if updated data is not available ... By
//     default, 1 (enabled)"). Cerberus overrides it to 0 so a shard whose
//     reachable replicas are ALL stale aborts the query with
//     ALL_REPLICAS_ARE_STALE (chclient's own
//     *StaleReplicaFallbackDeniedError) instead of silently answering from
//     data that may be missing recent writes. This is the same "fail
//     loud, never silently wrong" posture cerberus already applies
//     elsewhere (skip_unavailable_shards above, timeout_overflow_mode=
//     throw in this same package) — an operator who genuinely wants
//     eventual-consistency-tolerant reads over a lagging replica can still
//     override this via a server-side settings profile; cerberus's own
//     default never chooses that trade-off silently.
//
// Both settings are stamped UNCONDITIONALLY on every data-plane read-path
// query (see Client.querySettings), mirroring how settingTimeoutOverflowMode
// is pinned outright rather than exposed as an operator knob: they are
// harmless, version-safe no-ops against a single-shard/non-Distributed
// deployment (cerberus's default, and every deployment before epic #3074) —
// ClickHouse accepts and simply never consults either setting on a query
// that touches no Distributed-engine table — so pinning them unconditionally
// costs nothing on the common case and only changes behavior once
// internal/chopt.ClusterTopology.DataShardCount > 1 (cerberus issue #3077).
const (
	settingSkipUnavailableShards                        = "skip_unavailable_shards"
	settingFallbackToStaleReplicasForDistributedQueries = "fallback_to_stale_replicas_for_distributed_queries"
)

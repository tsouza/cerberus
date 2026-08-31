package chclient

import (
	"sync/atomic"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// ClickHouse ProfileEvent names the query result cache reports. Pinned as
// named constants — rather than inlined string literals in
// observeResultCacheProfileEvents — so a single place documents the exact
// server-reported spelling these counters key on.
const (
	profileEventQueryCacheHits   = "QueryCacheHits"
	profileEventQueryCacheMisses = "QueryCacheMisses"
)

// resultCacheHits / resultCacheMisses are process-wide counters of the
// ClickHouse-reported QueryCacheHits / QueryCacheMisses ProfileEvents,
// summed across every dispatch whose per-query settings carried
// use_query_cache=1 (see queryContext / resultCacheStamped). This is the
// REAL server-side verdict — did ClickHouse actually serve a cached result,
// not merely "cerberus considered this query eligible" — surfaced on /info
// as the result-cache hit-rate signal (cerberus issue #2781).
//
// Process-wide rather than per-request because the /info endpoint reports a
// process fingerprint, exactly like every other counter it surfaces
// (uptime, the resolved optimization set); a per-query breakdown belongs to
// tracing (the ch.profile_event.* span attributes columnar.go already
// stamps for the columnar-decode path) or the optcorpus reconciler, not
// this lightweight boot-to-now tally.
var (
	resultCacheHits   atomic.Uint64
	resultCacheMisses atomic.Uint64
)

// observeResultCacheProfileEvents is the clickhouse.WithProfileEvents
// handler wired onto every dispatch whose per-query settings map carries
// use_query_cache=1 (see queryContext). A single query's ProfileEvents batch
// may report zero, one, or both counters depending on server version, so
// both are summed independently rather than assuming exactly one fires.
func observeResultCacheProfileEvents(events []clickhouse.ProfileEvent) {
	for i := range events {
		switch events[i].Name {
		case profileEventQueryCacheHits:
			resultCacheHits.Add(clampU64(events[i].Value))
		case profileEventQueryCacheMisses:
			resultCacheMisses.Add(clampU64(events[i].Value))
		}
	}
}

// clampU64 narrows a non-negative int64 ProfileEvent count to uint64,
// clamping a negative value to 0 so the conversion is provably overflow-free
// (gosec G115), mirroring internal/engine's clampU32 /
// internal/optcorpus's clampShardCount. A ClickHouse counter ProfileEvent is
// always non-negative; the clamp documents that invariant rather than
// trusting it silently.
func clampU64(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// ResultCacheStats returns the process-wide result-cache hit/miss totals
// accumulated since boot. The /info handler (internal/api/info) reads this
// on every request; it is the honest "did the result cache actually pay
// off" answer — a query cerberus stamped eligible but the server still
// recomputed (a cold entry, an evicted one, or a byte-differing settings
// fingerprint) counts as a miss here, never silently dropped.
func ResultCacheStats() (hits, misses uint64) {
	return resultCacheHits.Load(), resultCacheMisses.Load()
}

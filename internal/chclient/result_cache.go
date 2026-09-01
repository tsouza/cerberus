package chclient

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// SettingUseQueryCache is the ClickHouse setting that turns on the query
// result cache for a statement: the server caches the RESULT ROWS of the
// executed query (keyed by the query AST plus the settings fingerprint that
// rode it), and a later query with byte-identical text and settings returns
// the cached result set without re-executing. Long-stable — ClickHouse
// shipped the query cache in 23.2, well before cerberus's own 24.8 floor —
// so unlike the experimental ts-grid setting there is no version-gated
// registry floor above the supported baseline to check here; the chopt
// result_cache feature instead gates on a boot capability probe (see
// ProbeResultCacheCapability) because a hardened/constrained deployment
// profile can still forbid the setting, or the server can run with its query
// cache disabled entirely (query_cache_max_size_in_bytes=0).
const SettingUseQueryCache = "use_query_cache"

// SettingQueryCacheTTL is query_cache_ttl: how many seconds ClickHouse keeps
// a cached result before a later identical query recomputes it rather than
// serving the cached rows.
const SettingQueryCacheTTL = "query_cache_ttl"

// SettingQueryCacheNondeterministicFunctionHandling is
// query_cache_nondeterministic_function_handling: what ClickHouse does when a
// query it was asked to cache contains a function its query cache classifies
// as non-deterministic.
//
// Its server default is `throw`, and `throw` means the QUERY FAILS — error
// 704 QUERY_CACHE_USED_WITH_NONDETERMINISTIC_FUNCTIONS, "The query result was
// not cached because the query contains a non-deterministic function" — not
// "the result was returned uncached". That distinction is the whole reason
// this constant exists: stamping use_query_cache=1 on a statement ClickHouse
// declines to cache turns a query that would have SUCCEEDED into one that
// errors out.
//
// ClickHouse's non-determinism test is `IFunction::isDeterministic()`, which
// is far broader than the now()/now64() family cerberus's own eligibility
// gate reasons about. `arrayJoin` is classified non-deterministic (it changes
// the row count), and arrayJoin is exactly how cerberus fans a range query's
// samples out across its step grid — the `arrayJoin(arrayMap(i -> ...,
// range(...)))` shape every non-native range lowering emits. So on the
// DEFAULT server posture the result cache did not merely fail to help those
// queries, it failed them: cerberus issue #2895. The rejections then
// amplified, because the resulting error rate tripped the chclient circuit
// breaker and everything dispatched behind it came back "circuit breaker
// open" — in the failing CI run, 16 rejected queries became 144 diverged
// LogQL cases and 871 diverged PromQL cases against the reference backends,
// holding the compatibility lane red on main for 31 consecutive runs.
//
// Verified directly against clickhouse-server 24.8.14.39 (cerberus's supported
// floor) and 26.5.7.64 (the compatibility harness's server): on both, the
// setting exists, defaults to `throw`, `SELECT arrayJoin([1,2,3])` under
// use_query_cache=1 raises 704, and the same statement under
// queryCacheHandlingIgnore returns its rows normally. In a local reproduction
// of the incident, the harness's own ClickHouse recorded 21 exceptions and
// system.query_log agreed on every one: all 21 were code 704, all 21 carried
// use_query_cache=1, and all 21 contained arrayJoin.
const SettingQueryCacheNondeterministicFunctionHandling = "query_cache_nondeterministic_function_handling"

// queryCacheHandlingIgnore is the value cerberus stamps for
// SettingQueryCacheNondeterministicFunctionHandling: ClickHouse SKIPS caching
// the statement and returns its freshly computed rows, instead of refusing to
// run it (`throw`, the server default) or caching it anyway (`save`).
//
// `ignore` is the only value that matches what cerberus is actually asking
// for. A result cache is a performance optimization, and a performance
// optimization must never be able to turn a successful query into a failed
// one — so `throw` is wrong. `save` is wrong in the other direction: it would
// override ClickHouse's own judgement and cache a result the server just said
// it cannot vouch for, which is precisely the staleness risk
// engine.eligibleForResultCache's closed-window gate exists to avoid. Under
// `ignore` the two guards compose the way the feature always intended: cerberus
// decides which windows are CLOSED enough to be cacheable at all, and
// ClickHouse retains its veto over statements it cannot cache safely, with the
// veto costing a cache miss rather than the query.
const queryCacheHandlingIgnore = "ignore"

// WithResultCacheSetting returns a ctx that signals the data-plane query
// methods to add SettingUseQueryCache=1, SettingQueryCacheTTL=ttlSeconds and
// SettingQueryCacheNondeterministicFunctionHandling=queryCacheHandlingIgnore
// to the per-request ClickHouse settings map. It mirrors WithTSGridSetting:
// one writer into the generalised WithQuerySetting carrier, so a query that
// is BOTH result-cache-eligible and, say, condition-cache-eligible carries
// both settings on the one map rather than one wrap clobbering the other.
//
// The three settings ride TOGETHER, always, and that coupling is load-bearing
// rather than incidental: use_query_cache=1 without the handling stamp is the
// posture that fails a query ClickHouse declines to cache (see
// SettingQueryCacheNondeterministicFunctionHandling for the mechanism and the
// incident). Every caller therefore goes through this one wrapper — there is
// no path that stamps use_query_cache on its own.
func WithResultCacheSetting(ctx context.Context, ttlSeconds int64) context.Context {
	ctx = WithQuerySetting(ctx, SettingUseQueryCache, 1)
	ctx = WithQuerySetting(ctx, SettingQueryCacheNondeterministicFunctionHandling, queryCacheHandlingIgnore)
	return WithQuerySetting(ctx, SettingQueryCacheTTL, ttlSeconds)
}

// resultCacheStamped reports whether s (the per-query ClickHouse settings map
// querySettings assembles) carries SettingUseQueryCache=1 — the signal that
// THIS dispatch is one the engine's result-cache rule stamped, so
// queryContext should also wire the hit/miss ProfileEvents observer (see
// result_cache_metrics.go). A nil map or a settings map without the setting
// (or with it set to something other than 1) reports false.
func resultCacheStamped(s clickhouse.Settings) bool {
	v, ok := s[SettingUseQueryCache]
	return ok && v == 1
}

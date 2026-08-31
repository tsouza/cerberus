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

// WithResultCacheSetting returns a ctx that signals the data-plane query
// methods to add SettingUseQueryCache=1 and SettingQueryCacheTTL=ttlSeconds
// to the per-request ClickHouse settings map. It mirrors WithTSGridSetting:
// one writer into the generalised WithQuerySetting carrier, so a query that
// is BOTH result-cache-eligible and, say, condition-cache-eligible carries
// both settings on the one map rather than one wrap clobbering the other.
func WithResultCacheSetting(ctx context.Context, ttlSeconds int64) context.Context {
	ctx = WithQuerySetting(ctx, SettingUseQueryCache, 1)
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

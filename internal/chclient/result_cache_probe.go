package chclient

import (
	"context"

	"github.com/tsouza/cerberus/internal/chopt"
)

// resultCacheCapabilityProbeSQL is the canary body — see
// tsGridCapabilityProbeSQL's own doc for why it is a trivial constant SELECT
// rendered as a String column rather than a real query-cache round trip: the
// canary only needs to learn whether the server ACCEPTS the
// use_query_cache / query_cache_ttl settings cerberus stamps on an eligible
// query, which ClickHouse decides when it applies the per-query settings map
// — BEFORE the query body is analysed — so a trivial body isolates the
// FORBIDDEN signal with zero dependence on any real query shape. Reusing the
// SAME constant keeps the two canaries byte-identical on the one property
// that actually matters here (a String-typed projection Client.QueryStrings
// can decode); it is not a case of two probes needing the same setting.
const resultCacheCapabilityProbeSQL = tsGridCapabilityProbeSQL

// resultCacheProbeTTLSeconds is the query_cache_ttl the boot canary stamps
// alongside use_query_cache=1. Its value is otherwise irrelevant — the
// canary body never runs long enough to populate or expire a cache entry —
// but ClickHouse validates the setting's TYPE (a non-negative integer) when
// it applies the per-query settings map, so the probe must send something
// well-formed rather than an arbitrary sentinel that could itself trip a
// spurious rejection unrelated to use_query_cache.
const resultCacheProbeTTLSeconds = 1

// ProbeResultCacheCapability runs the boot-time capability canary once and
// classifies whether the connected ClickHouse server will accept the
// query-result-cache settings (use_query_cache, query_cache_ttl) cerberus
// stamps on an eligible read path. It mirrors ProbeTSGridCapability exactly
// — the same tri-state classification (classifyCapabilityFromProbeErr), the
// same "never returns an error" contract — but probes a DIFFERENT setting:
// the result-cache family is long-stable (unlike the ts-grid experimental
// gate, no ClickHouse version floor above cerberus's own 24.8 floor is at
// stake), so what this canary catches is not "too old" but a
// hardened/constrained deployment profile that pins or forbids
// use_query_cache, or a server whose query cache is disabled entirely
// (query_cache_max_size_in_bytes=0) — either of which answers with a typed
// rejection rather than silently ignoring the stamped settings. cmd/cerberus
// calls this over the bootstrap connection alongside ProbeTSGridCapability
// and hands the verdict to chopt.Resolve as Config.ResultCacheCapability.
//
// Because the canary stamps the SAME use_query_cache=1 setting a real
// eligible query does, queryContext's generic resultCacheStamped detection
// also wires the ProfileEvents observer onto this probe's own trivial
// SELECT, so ResultCacheStats picks up a near-zero amount of probe traffic
// (one dispatch at boot, then one per chOptReprobeInterval tick) alongside
// real dashboard queries. Negligible next to real query volume and not worth
// a second, probe-suppressing code path.
func (c *Client) ProbeResultCacheCapability(ctx context.Context) chopt.Capability {
	ctx = WithResultCacheSetting(ctx, resultCacheProbeTTLSeconds)
	_, err := c.QueryStrings(ctx, resultCacheCapabilityProbeSQL)
	return classifyCapabilityFromProbeErr(err)
}

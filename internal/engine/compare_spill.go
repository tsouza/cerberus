package engine

import (
	"context"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// settingMaxBytesBeforeExternalGroupBy is the ClickHouse setting that makes the
// aggregator spill its hash table to disk once it grows past the configured
// byte threshold, instead of holding the whole GROUP BY state in RAM. It is
// RESULT-EQUIVALENT — only the execution strategy changes, never the rows — and
// has existed since long before cerberus's CH 24.8 floor, so stamping it is
// version-safe.
const settingMaxBytesBeforeExternalGroupBy = "max_bytes_before_external_group_by"

// compareGroupBySpillBytes is the byte threshold at which the compare() GROUP BY
// begins spilling to disk. TraceQL's `| compare(...)` explodes every span into
// an arrayJoin of (attribute, value) pairs and then GROUP BYs the full
// (cohort, attr, val) cross-product with NO top-N pushdown (the totals series
// must count every value), so a broad selection over high-cardinality
// attributes builds an unbounded in-memory hash table that aborts the query
// with MEMORY_LIMIT_EXCEEDED (code 241) at the 2 GiB per-query cap. Spilling at
// 512 MiB keeps the aggregation well under the cap, trading a slower disk-backed
// merge for a query that COMPLETES instead of 422-ing. The threshold sits
// comfortably below the 1 GiB default max_memory_usage so the spill triggers
// before the cap is reached.
const compareGroupBySpillBytes int64 = 512 << 20 // 536870912 bytes

// applyCompareSpill stamps the external-group-by spill threshold on ctx when
// plan contains a chplan.MetricsCompare node. Unlike the DARK, flag-gated
// SettingsRules, this rule is ALWAYS ON for the compare shape: an OOM abort is
// an availability bug, not an optimization opportunity, and the setting is
// result-equivalent and version-safe, so there is no downside to always letting
// the heavy aggregation spill rather than blow the per-query memory cap.
//
// Plans without a MetricsCompare node return ctx unchanged, so the setting
// never rides an unrelated query. Written through chclient.WithQuerySetting so
// it merges onto the one per-request settings map alongside max_memory_usage
// and any plan-shape-gated knobs.
func applyCompareSpill(ctx context.Context, plan chplan.Node) context.Context {
	if !planHasMetricsCompare(plan) {
		return ctx
	}
	return chclient.WithQuerySetting(ctx, settingMaxBytesBeforeExternalGroupBy, compareGroupBySpillBytes)
}

// planHasMetricsCompare reports whether plan contains a chplan.MetricsCompare
// node anywhere in the tree (it is typically the emit root, optionally wrapped
// by a RangeWindow for the matrix path).
func planHasMetricsCompare(plan chplan.Node) bool {
	found := false
	chplan.Walk(plan, func(n chplan.Node) bool {
		if _, ok := n.(*chplan.MetricsCompare); ok {
			found = true
			return false // stop descending this branch
		}
		return true
	})
	return found
}

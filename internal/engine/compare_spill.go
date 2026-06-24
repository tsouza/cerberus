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

// compareSpillCapDenominator divides the live per-query memory cap to derive a
// cap-relative spill threshold: the spill must begin at a fraction of the cap
// so the disk-backed merge still has headroom under max_memory_usage.
// ClickHouse's own guidance for max_bytes_before_external_group_by is ~50% of
// max_memory_usage, hence 2 — half the cap.
const compareSpillCapDenominator int64 = 2

// compareSpillThreshold returns the byte threshold to stamp for
// max_bytes_before_external_group_by, given the live per-query memory cap
// (max_memory_usage, in bytes; 0 = no cap configured).
//
// The fixed compareGroupBySpillBytes (512 MiB) is only safe when the cap sits
// comfortably above it. When an operator lowers CERBERUS_CH_QUERY_MAX_MEMORY to
// at or below 512 MiB (the envdocs example cites 512Mi), a fixed 512 MiB spill
// threshold lands AT or ABOVE the cap, so the GROUP BY never spills and the
// query OOMs before the threshold is reached — the very bug this rule exists to
// prevent. So when a cap is set, take the smaller of the fixed threshold and a
// cap-relative fraction (~50% of the cap), keeping the spill strictly below the
// cap for every config.
//
// When no cap is configured (cap <= 0) the threshold is the plain fixed value:
// max_bytes_before_external_group_by=0 means the spill is DISABLED, so min'ing
// against a non-positive cap would re-introduce the unbounded-RAM bug.
func compareSpillThreshold(maxMemory int64) int64 {
	if maxMemory <= 0 {
		return compareGroupBySpillBytes
	}
	if capRelative := maxMemory / compareSpillCapDenominator; capRelative < compareGroupBySpillBytes {
		return capRelative
	}
	return compareGroupBySpillBytes
}

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
//
// maxMemory is the live per-query memory cap — the SAME value the chclient
// query path stamps as max_memory_usage — so the threshold is sized relative to
// it and the spill always triggers strictly below the cap (see
// compareSpillThreshold).
func applyCompareSpill(ctx context.Context, plan chplan.Node, maxMemory int64) context.Context {
	if !planHasMetricsCompare(plan) {
		return ctx
	}
	return chclient.WithQuerySetting(ctx, settingMaxBytesBeforeExternalGroupBy, compareSpillThreshold(maxMemory))
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

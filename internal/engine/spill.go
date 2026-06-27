package engine

import (
	"context"

	"github.com/tsouza/cerberus/internal/chclient"
)

// settingMaxBytesBeforeExternalGroupBy / settingMaxBytesBeforeExternalSort are
// the ClickHouse settings that make the aggregator / sorter spill its state to
// disk once it grows past the configured byte threshold, instead of holding the
// whole GROUP BY hash table / sort buffer in RAM. Both are RESULT-EQUIVALENT —
// only the execution strategy changes, never the rows — and have existed since
// long before cerberus's CH floor, so stamping them is version-safe.
const (
	settingMaxBytesBeforeExternalGroupBy = "max_bytes_before_external_group_by"
	settingMaxBytesBeforeExternalSort    = "max_bytes_before_external_sort"
)

// spillThresholdBytes is the default byte threshold at which a GROUP BY / sort
// begins spilling to disk. A high-cardinality aggregation (TraceQL `compare()`
// arrayJoin, PromQL `sum by(user_id)`, LogQL `by(request_id)`, …) or a large
// sort otherwise builds an unbounded in-memory state and aborts the query with
// MEMORY_LIMIT_EXCEEDED (code 241) at the per-query cap. Spilling at 512 MiB
// keeps the operation well under the 1 GiB default cap, trading a slower
// disk-backed merge for a query that COMPLETES instead of 422-ing.
const spillThresholdBytes int64 = 512 << 20 // 536870912 bytes

// spillCapDenominator divides the live per-query memory cap to derive a
// cap-relative spill threshold: the spill must begin at a fraction of the cap
// so the disk-backed merge still has headroom under max_memory_usage.
// ClickHouse's own guidance for max_bytes_before_external_group_by is ~50% of
// max_memory_usage, hence 2 — half the cap.
const spillCapDenominator int64 = 2

// spillThreshold returns the byte threshold to stamp, given the live per-query
// memory cap (max_memory_usage, in bytes; 0 = no cap configured).
//
// The fixed spillThresholdBytes (512 MiB) is only safe when the cap sits
// comfortably above it. When an operator lowers CERBERUS_CH_QUERY_MAX_MEMORY to
// at or below 512 MiB, a fixed 512 MiB threshold lands AT or ABOVE the cap, so
// the operation never spills and OOMs before the threshold is reached — the
// very bug this exists to prevent. So when a cap is set, take the smaller of
// the fixed threshold and a cap-relative fraction (~50% of the cap), keeping
// the spill strictly below the cap for every config.
//
// When no cap is configured (cap <= 0) the threshold is the plain fixed value:
// max_bytes_before_external_*=0 means the spill is DISABLED, so min'ing against
// a non-positive cap would re-introduce the unbounded-RAM bug.
func spillThreshold(maxMemory int64) int64 {
	if maxMemory <= 0 {
		return spillThresholdBytes
	}
	if capRelative := maxMemory / spillCapDenominator; capRelative < spillThresholdBytes {
		return capRelative
	}
	return spillThresholdBytes
}

// applySpillSettings stamps the external-group-by AND external-sort spill
// thresholds on ctx for EVERY data-plane query.
//
// It is UNCONDITIONAL (it replaces the old MetricsCompare-only applyCompareSpill)
// because the OOM-prone GROUP BY / sort is not unique to compare(): any head can
// lower a high-cardinality aggregation (`sum by(user_id)`, LogQL `by(...)`,
// TraceQL structural DISTINCT / nested-set window passes) or a large sort
// (`topk`, `ORDER BY`) that would otherwise abort at the cap. Both settings are
// result-equivalent and THRESHOLD-GATED: an operation whose state stays under
// spillThreshold(cap) never spills, so a normal query is byte-for-byte
// unaffected (same rows, same plan, no extra disk I/O); only an operation
// approaching the cap spills-and-completes instead of 422-ing. An OOM abort is
// an availability bug, not an optimization opportunity, so there is no downside
// to always letting a heavy aggregation/sort spill rather than blow the cap.
//
// Written through chclient.WithQuerySetting so the thresholds merge onto the one
// per-request settings map alongside max_memory_usage and any plan-shape-gated
// knobs. (See the resource-bound audit, axis 4: this upgrades the largest
// runtime-net surface from OOM-abort to spill-and-complete across all heads.)
func applySpillSettings(ctx context.Context, maxMemory int64) context.Context {
	threshold := spillThreshold(maxMemory)
	ctx = chclient.WithQuerySetting(ctx, settingMaxBytesBeforeExternalGroupBy, threshold)
	ctx = chclient.WithQuerySetting(ctx, settingMaxBytesBeforeExternalSort, threshold)
	return ctx
}

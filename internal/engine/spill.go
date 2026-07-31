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

// spillThresholdBytes is the byte threshold at which a GROUP BY / sort begins
// spilling to disk when NO per-query memory cap is configured. A
// high-cardinality aggregation (TraceQL `compare()` arrayJoin, PromQL
// `sum by(user_id)`, LogQL `by(request_id)`, …) or a large sort otherwise
// builds an unbounded in-memory state and aborts the query with
// MEMORY_LIMIT_EXCEEDED (code 241) at whatever server-side limit it eventually
// hits. With no cap there is nothing to take a fraction of, so the fallback is
// the threshold a DEFAULT-capped deployment gets: config's 1 GiB default
// max_memory_usage divided by spillCapDenominator. An uncapped deployment
// therefore spills exactly where a default-capped one does, trading a slower
// disk-backed merge for a query that COMPLETES instead of 422-ing.
// TestSpillThresholdBytes_MatchesDefaultCapThreshold pins that relationship, so
// raising the config default without re-deriving this value fails loudly.
const spillThresholdBytes int64 = 512 << 20 // 536870912 bytes — (1 GiB default cap) / 2

// spillCapDenominator divides the live per-query memory cap to derive the
// cap-relative spill threshold: the spill must begin at a fraction of the cap
// so the disk-backed merge still has headroom under max_memory_usage.
// ClickHouse's own guidance for max_bytes_before_external_group_by is ~50% of
// max_memory_usage, hence 2 — half the cap. Any denominator >= 2 puts the
// threshold strictly below the cap, which is the property the spill depends on.
const spillCapDenominator int64 = 2

// spillThreshold returns the byte threshold to stamp, given the live per-query
// memory cap (max_memory_usage, in bytes; 0 = no cap configured).
//
// A configured cap is the only honest scale for the threshold, so the threshold
// is derived from it and from nothing else: half the cap, per ClickHouse's own
// guidance for max_bytes_before_external_group_by. That is safe by construction
// at every cap a query can actually run under: for a cap of at least
// spillCapDenominator bytes the quotient is positive AND strictly below the cap,
// so the operation always reaches the spill threshold before it reaches
// MEMORY_LIMIT_EXCEEDED, whether the operator raises
// CERBERUS_CH_QUERY_MAX_MEMORY to many GiB or lowers it below 512 MiB. (A
// single-byte cap admits no threshold at all — no positive byte count sits below
// one byte — and no query runs under such a cap either way.) Sizing
// against the cap is also what keeps the threshold HONEST in the other
// direction: a threshold far below the cap spills state the query had the
// budget to hold in RAM, paying a disk-backed merge (and the read amplification
// that comes with it) for no memory gain.
//
// When no cap is configured (cap <= 0) there is no cap to take a fraction of,
// so the threshold is the absolute spillThresholdBytes. It must not fall out of
// the cap-relative arithmetic: max_bytes_before_external_*=0 means the spill is
// DISABLED, which would re-introduce the unbounded-RAM abort this exists to
// prevent.
func spillThreshold(maxMemory int64) int64 {
	if maxMemory <= 0 {
		return spillThresholdBytes
	}
	return maxMemory / spillCapDenominator
}

// applySpillSettings stamps the external-group-by AND external-sort spill
// thresholds on ctx for EVERY data-plane query.
//
// It is UNCONDITIONAL rather than scoped to one plan shape because the OOM-prone
// GROUP BY / sort is not unique to TraceQL compare(): any head can
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

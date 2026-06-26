package chplan

// This file makes "bound the raw scan to the eval window" an IR-level
// property of the plan rather than an emit-time afterthought.
//
// Background. An instant windowed range aggregation (rate / increase /
// *_over_time / …) over a metrics/traces/logs table reads per-sample rows
// out of MergeTree, groupArray's them per series at the innermost level, then
// evaluates the function over the in-window array. If the innermost read
// carries NO time predicate, ClickHouse cannot prune granules: it groupArray's
// the full per-series retention before the post-groupArray window filter (an
// arrayFilter) discards out-of-window samples. On prod instant queries that
// means tens of millions of rows read, seconds of latency, GiB of memory —
// because the bound lived only in the emitter, it was repeatedly forgotten as
// new groupArray emitters landed (#1027 / #1048 / #1056 / #1059 / #1080 /
// #1088 / #1089, then the instant path in #1098).
//
// The fix is structural: the fact that an instant windowed-array leaf IS
// bounded is recorded ON the RangeWindow (RangeWindow.InstantScanBounded) so it
// is visible in the IR, established ONCE here rather than re-remembered per
// emitter, and ENFORCED at plan-build time by the fail-closed optimizer
// analyzer (internal/optimizer RequireScanTimeBound) and at emit time by each
// windowed-array emitter's fail-closed guard. The bound predicate text is
// still rendered by the emitters (instantWindowScanBoundsFrags and the
// emitRangeWindowOverTimeDirect WHERE) — byte-identical to #1098 — so this
// change adds the invariant without perturbing a single emitted SQL byte.
//
// Why a flag on RangeWindow, not the predicate on Scan. The bound is applied at
// the innermost groupArray, which sits over whatever the windowed Input renders
// to — a bare Scan, Filter(Scan), a UnionAll of per-table scans (the
// unsuffixed Gauge+Sum metric path), or another window's per-anchor output (a
// subquery's outer aggregation). A single Scan field could not express the
// UnionAll / subquery shapes; the RangeWindow can carry the contract
// Input-shape-independently.
//
// The INSTANT windowed-array LEAF shape is: a RangeWindow with OuterRange == 0
// whose Input is NOT a MetricsAggregate / MetricsHistogramOverTime /
// MetricsCompare (those route to the metrics emitters, which carry their own
// emit-time bound via maybePushInnerScanTimeBounds). That is exactly the set
// the instant windowed-array emitters (#1098) handle. The matrix (OuterRange >
// 0), native, LWR, and resample bucket-fanout shapes still bound at emit time
// via maybePushInnerScanTimeBounds, and the lowering `@` / offset / staleness
// paths carry the bound as a sibling Filter; the analyzer leaves those
// emit-time mechanisms in place rather than asserting an IR flag for them. See
// docs/engine.md ("Scan time-bound contract").

// IsInstantWindowedLeaf reports whether rw is the instant windowed-array leaf
// shape this file governs: an instant RangeWindow (OuterRange == 0) whose Input
// routes to a windowed-array emitter — i.e. the Input is NOT one of the
// metrics-aggregate node types that route to the emit-time-bounded metrics
// emitters. That is exactly the set of RangeWindows whose innermost groupArray
// reads a raw scan (or union scans, or a subquery's per-anchor output) and
// therefore must carry a scan time bound.
func IsInstantWindowedLeaf(rw *RangeWindow) bool {
	if rw.OuterRange != 0 {
		return false
	}
	switch rw.Input.(type) {
	case *MetricsAggregate, *MetricsHistogramOverTime, *MetricsCompare:
		return false
	}
	return true
}

// AttachInstantScanTimeBounds marks every instant windowed-array leaf
// RangeWindow in root that does not already carry the bound, returning the
// (possibly new) root. It is idempotent: a RangeWindow already marked is left
// untouched.
//
// It is the always-run establishment point on the emit path (chsql.Emit calls
// it) so the flag is present even when the optimizer is skipped (the test/spec
// lower→emit lane); the optimizer establishes the same flag via the
// NormalizeScanTimeBound analyzer rule so the fail-closed RequireScanTimeBound
// analyzer sees it. When nothing needs marking (the production path, where the
// optimizer already established it) root is returned unchanged with no clone,
// so the common case is a single read-only walk.
func AttachInstantScanTimeBounds(root Node) Node {
	if root == nil || !needsInstantScanTimeBound(root) {
		return root
	}
	out := CloneNode(root)
	attachInstantWalk(out)
	return out
}

// needsInstantScanTimeBound reports whether any instant windowed-array leaf
// RangeWindow reachable from n is still unmarked (a read-only pre-check so the
// already-established common case avoids a clone).
func needsInstantScanTimeBound(n Node) bool {
	if n == nil {
		return false
	}
	if rw, ok := n.(*RangeWindow); ok && !rw.InstantScanBounded && IsInstantWindowedLeaf(rw) {
		return true
	}
	for _, c := range n.Children() {
		if needsInstantScanTimeBound(c) {
			return true
		}
	}
	return false
}

// attachInstantWalk mutates n in place — the caller passes a freshly cloned,
// solely-owned tree — marking each instant windowed-array leaf RangeWindow that
// is not yet marked.
func attachInstantWalk(n Node) {
	if n == nil {
		return
	}
	if rw, ok := n.(*RangeWindow); ok && !rw.InstantScanBounded && IsInstantWindowedLeaf(rw) {
		rw.InstantScanBounded = true
	}
	for _, c := range n.Children() {
		attachInstantWalk(c)
	}
}

// WithInstantScanTimeBound returns rw with InstantScanBounded set, plus whether
// a change was made. When rw is not the instant windowed-array leaf shape, or
// is already marked, rw is returned unchanged. The returned RangeWindow (on
// change) is a shallow copy — the flag is set on the copy, so the original is
// never mutated — making it safe to use from the optimizer's immutable rewrite
// framework. Idempotent.
func WithInstantScanTimeBound(rw *RangeWindow) (*RangeWindow, bool) {
	if rw.InstantScanBounded || !IsInstantWindowedLeaf(rw) {
		return rw, false
	}
	clone := *rw
	clone.InstantScanBounded = true
	return &clone, true
}

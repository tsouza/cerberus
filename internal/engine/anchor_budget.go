package engine

import (
	"math"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// requireSubquerySampleBudget fail-closes a PromQL subquery whose anchor grid
// alone would exceed the per-query sample budget (Config.MaxQuerySamples).
//
// A subquery <reducer>_over_time(<inner>[OuterRange:Step]) materialises
// OuterRange/Step + 1 anchor rows PER SERIES as an intermediate (the
// GROUP BY (Attributes, anchor_ts) regroup) before collapsing them. cerberus's
// MaxQuerySamples budget is enforced on the Go-side RESULT drain
// (chclient.SampleBudget), which sees ~1 row/series for an instant reducer and
// so never trips — the millions of intermediate anchor rows OOM ClickHouse
// before any result is drained (resource-bound audit GAP-2; #1112's spill
// bounds the anchor axis, but the cardinality axis still busts a fixed memory
// cap at C>=10 series for a 90d:1s grid).
//
// This bounds the intermediate the way upstream Prometheus bounds a subquery:
// it REJECTS rather than streams-and-OOMs (Prometheus returns "too many
// samples" once a subquery would load more than query.max-samples into memory).
// We reject when a single series' anchor grid alone exceeds the budget,
// returning the same chclient.ErrTooManySamples that maps to the Prom-shaped
// 422 — so the worst case is a fast honest rejection, never a process OOM that
// takes down all three heads sharing the cerberus process.
//
// maxSamples <= 0 disables the budget (matching the cursor's per-query budget
// semantics), so the gate is inert by default in tests that don't wire it.
//
// Scope. NumAnchors is non-zero for ANY RangeWindow with OuterRange>0 && Step>0
// — a subquery [range:step] grid, OR a plain query_range matrix's outer step
// grid. A plain query_range alone is already capped at format.MaxResolutionPoints
// (11000) in the head handlers, far below any sane budget; SUBQUERY inner grids
// have no such cap, so this budget is the subquery counterpart to
// MaxResolutionPoints. The two compose: a query_range OVER a subquery stacks the
// 11000-point outer grid above the subquery grid, and subqueryAnchorLoad
// MULTIPLIES them — so the gate can be reached by a range query whose subquery
// inner grid pushes the product past the budget (the intermediate it materialises
// really is that product).
//
// The bound is per-series and conservative on the cardinality axis by design: it
// counts a single series' anchor grid, not anchors x series; the cardinality
// axis is bounded elsewhere (#1112 spill + the result-drain SampleBudget). So a
// sub-budget grid at high cardinality is backstopped at runtime, not here.
//
// Nesting IS counted (GAP-C): subqueryAnchorLoad takes the PRODUCT of stacked
// OuterRange>0 grids — each outer anchor re-evaluates the inner grid — not the
// max, which under-rejected nested subqueries.
func requireSubquerySampleBudget(plan chplan.Node, maxSamples int64) error {
	if maxSamples <= 0 || plan == nil {
		return nil
	}
	worst := subqueryAnchorLoad(plan)
	if worst > maxSamples {
		return &chclient.TooManySamplesError{Limit: maxSamples}
	}
	return nil
}

// subqueryAnchorLoad returns the largest per-series intermediate anchor-row
// count the plan will materialise. A single subquery RangeWindow contributes
// its NumAnchors; NESTED subqueries multiply, because each outer anchor
// re-evaluates the inner grid — `max_over_time(max_over_time(rate(m[1m])
// [5m:30s])[1h:5m])` materialises (1h/5m)·(5m/30s) anchor rows, not the larger
// of the two. (The earlier max-only count under-rejected this product shape —
// GAP-C.) Sibling subqueries (e.g. the two arms of a binary op) do NOT multiply;
// only a RangeWindow stacked over another OuterRange>0 RangeWindow in its own
// input subtree does. Saturates at math.MaxInt64 so a deeply nested product can
// never wrap negative and slip under the budget.
//
// [chplan.RangeBucketFanout], [chplan.RangeLWR] and
// [chplan.RangeBucketGridNative] contribute their own NumAnchors the
// same way (#2408): each materialises
// exactly the same "one row per Step across [Start, End]" per-series
// intermediate RangeWindow does, and the histogram range-function lowerings
// (internal/promql) build one of these DIRECTLY off a subquery's own
// [OuterRange:Step] grid — `histogram_quantile(...)[range:step]` nested
// inside an outer `<reducer>_over_time(...)` — without ever wrapping that
// grid in an OuterRange-bearing RangeWindow (the histogram lowering absorbs
// the subquery's materialisation into the fan-out node itself). Before this
// case existed, that grid was invisible to this walk: a bare-selector
// subquery `<reducer>_over_time(m[range:step])` was rejected once its inner
// grid crossed the budget, but the histogram-valued analogue
// `<reducer>_over_time(histogram_quantile(...)[range:step])` was not,
// despite materialising the identical per-series row count. Because these
// nodes are the SOLE representation of their own subquery grid — no
// RangeWindow duplicates it above or below them — adding them here cannot
// double-count an already-counted grid; it only closes the gap for the one
// grid shape nothing else on this walk could see.
func subqueryAnchorLoad(n chplan.Node) int64 {
	if n == nil {
		return 0
	}
	var self int64
	switch v := n.(type) {
	case *chplan.RangeWindow:
		self = v.NumAnchors()
	case *chplan.RangeBucketFanout:
		self = v.NumAnchors()
	case *chplan.RangeLWR:
		self = v.NumAnchors()
	case *chplan.RangeBucketGridNative:
		self = v.NumAnchors()
	}
	// The heaviest nested load among the input subtree(s).
	var childLoad int64
	for _, c := range n.Children() {
		if l := subqueryAnchorLoad(c); l > childLoad {
			childLoad = l
		}
	}
	// A subquery grid (self>0) stacked over a nested grid (childLoad>0)
	// multiplies; otherwise the load is whichever side carries it.
	stacked := childLoad
	switch {
	case self > 0 && childLoad > 0:
		stacked = satMulInt64(self, childLoad)
	case self > childLoad:
		stacked = self
	}
	if exprLoad := exprSlotAnchorLoad(n); exprLoad > stacked {
		return exprLoad
	}
	return stacked
}

// exprSlotAnchorLoad returns the heaviest subqueryAnchorLoad among the plan
// subtrees embedded in n's own Expr slots — the ones chplan.Walk and
// chplan.Node.Children cannot reach.
func exprSlotAnchorLoad(n chplan.Node) int64 {
	var worst int64
	chplan.InspectNodeExprs(n, func(e chplan.Expr) {
		chplan.InspectExprNodes(e, func(chplan.Expr) bool { return true }, func(sub chplan.Node) {
			if l := subqueryAnchorLoad(sub); l > worst {
				worst = l
			}
		})
	})
	return worst
}

// satMulInt64 multiplies two non-negative int64s, saturating at math.MaxInt64
// instead of overflowing (a nested anchor product can exceed 1e18).
func satMulInt64(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}

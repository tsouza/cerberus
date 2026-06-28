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
func subqueryAnchorLoad(n chplan.Node) int64 {
	if n == nil {
		return 0
	}
	var self int64
	if rw, ok := n.(*chplan.RangeWindow); ok {
		self = rw.NumAnchors()
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
	if self > 0 && childLoad > 0 {
		return satMulInt64(self, childLoad)
	}
	if self > childLoad {
		return self
	}
	return childLoad
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

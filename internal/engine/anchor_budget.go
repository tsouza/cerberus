package engine

import (
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
func requireSubquerySampleBudget(plan chplan.Node, maxSamples int64) error {
	if maxSamples <= 0 || plan == nil {
		return nil
	}
	worst := worstAnchorCount(plan)
	if worst > maxSamples {
		return &chclient.TooManySamplesError{Limit: maxSamples}
	}
	return nil
}

// worstAnchorCount returns the largest RangeWindow.NumAnchors anywhere in the
// plan (0 if none) — the per-series intermediate row count of the heaviest
// subquery grid the plan will materialise.
func worstAnchorCount(n chplan.Node) int64 {
	if n == nil {
		return 0
	}
	var worst int64
	if rw, ok := n.(*chplan.RangeWindow); ok {
		worst = rw.NumAnchors()
	}
	for _, c := range n.Children() {
		if a := worstAnchorCount(c); a > worst {
			worst = a
		}
	}
	return worst
}

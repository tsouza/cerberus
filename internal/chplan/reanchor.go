package chplan

import (
	"errors"
	"fmt"
	"time"
)

// ErrReanchorGridMismatch is returned by ReanchorRange when a windowed
// node on the spine does not sit on the grid the request predicts at that
// spine depth. The sharded-pushdown solver treats this as "abort the
// Decision, fall back to route A": the copy is only safe when every
// re-anchored window is grid-consistent, so a mismatch (an @-pinned anchor,
// or a future route-A fix that pins End != ctx.end) must not be silently
// re-anchored into a wrong-results shard plan.
var ErrReanchorGridMismatch = errors.New("chplan: windowed node bounds do not match the predicted request grid")

// ReanchorRange returns a deep copy of n whose windowed spine is re-anchored
// to evaluate one row per anchor across [start, end], with each matrix
// RangeWindow's own input spine widened by a further Range of lookback so
// every anchor finds the samples it needs.
//
// It is the head-agnostic, copy-not-mutate generalization of
// promql.widenSubquerySpine (internal/promql/subquery.go): where
// widenSubquerySpine mutates the spine in place, ReanchorRange leaves the
// input Node and every expr tree reachable from it byte-identical and
// returns a fresh tree the solver can run as one of K concurrent shards.
//
// Defensive grid-prediction check (the @-modifier guard, §Routing of
// docs/query-solver-design.md). A windowed matrix node is re-anchored only
// if its current (Start, End, Step, OuterRange) match the grid the request
// predicts at that spine depth — concretely either:
//
//   - the bounds are unpinned (Start and End both zero): the shape the
//     subquery lowerings emit, expecting the grid to be filled in by the
//     widen/re-anchor pass (this is what keeps ReanchorRange equivalent to
//     widenSubquerySpine, which overwrites these unconditionally); or
//   - the bounds already equal the predicted (start, end) with
//     OuterRange == end - start: an already-gridded node sitting exactly on
//     the predicted grid (e.g. a top-level range-mode `rate(m[5m])`).
//
// Any other shape — an @-pinned anchor whose End differs from the predicted
// End, or a future route-A fix that pins End != ctx.end — returns
// ErrReanchorGridMismatch so the caller aborts to route A rather than emit
// a shard plan that silently disagrees with the @ semantics. This makes the
// copy safe both before and after the known lowerRangeFn @-clobber bug is
// fixed: today's clobbered plans land exactly on the predicted grid (they
// pass, and route-A-as-oracle holds), and once the clobber is fixed an
// @-pinned node's End no longer matches the predicted grid and it routes A.
//
// Spine shape mirrors widenSubquerySpine exactly so the two stay
// equivalent on post-optimizer plans (pinned by the equivalence test in
// internal/promql): matrix RangeWindows (Step > 0) re-anchor and recurse
// into their input with start.Add(-Range); instant RangeWindows (Step == 0)
// terminate the walk; the wrapper nodes the subquery lowerings interpose
// (Project / Aggregate / TopK / Filter) pass the requirement through
// unchanged. Every other node type is copied verbatim — it is below the
// spine and does not move in time.
func ReanchorRange(n Node, start, end time.Time) (Node, error) {
	if n == nil {
		return nil, nil
	}
	return reanchor(n, start.UTC(), end.UTC())
}

func reanchor(n Node, start, end time.Time) (Node, error) {
	switch v := n.(type) {
	case *RangeWindow:
		// Instant-shape RangeWindows resolve a single anchor themselves and
		// terminate the walk (mirrors widenSubquerySpine's Step <= 0 guard).
		if v.Step <= 0 {
			return CloneNode(v), nil
		}
		if err := checkPredictedGrid(v, start, end); err != nil {
			return nil, err
		}
		c := *v
		c.GroupBy = cloneExprs(v.GroupBy)
		c.Scalars = cloneFloats(v.Scalars)
		c.ScalarExprs = cloneExprs(v.ScalarExprs)
		c.Start = start
		c.End = end
		c.OuterRange = end.Sub(start)
		// Each of this window's anchors looks back v.Range; widen the input
		// spine by that much so the inner grid covers every anchor's window.
		input, err := reanchor(v.Input, start.Add(-v.Range), end)
		if err != nil {
			return nil, err
		}
		c.Input = input
		return &c, nil
	case *Project:
		input, err := reanchor(v.Input, start, end)
		if err != nil {
			return nil, err
		}
		return &Project{Input: input, Projections: cloneProjections(v.Projections)}, nil
	case *Aggregate:
		input, err := reanchor(v.Input, start, end)
		if err != nil {
			return nil, err
		}
		return &Aggregate{
			Input:              input,
			GroupBy:            cloneExprs(v.GroupBy),
			GroupByAliases:     cloneStrings(v.GroupByAliases),
			AggFuncs:           cloneAggFuncs(v.AggFuncs),
			DropEmptyOnNoGroup: v.DropEmptyOnNoGroup,
		}, nil
	case *TopK:
		input, err := reanchor(v.Input, start, end)
		if err != nil {
			return nil, err
		}
		c := *v
		c.Input = input
		// KExpr is below the spine (a computed-K scalar plan): copy verbatim,
		// it does not participate in the anchor grid.
		c.KExpr = CloneNode(v.KExpr)
		c.By = cloneExprs(v.By)
		c.SortExpr = cloneExpr(v.SortExpr)
		c.Columns = cloneStrings(v.Columns)
		return &c, nil
	case *Filter:
		input, err := reanchor(v.Input, start, end)
		if err != nil {
			return nil, err
		}
		return &Filter{Input: input, Predicate: cloneExpr(v.Predicate)}, nil
	default:
		// Off the windowed spine: a verbatim deep copy. CloneNode is
		// exhaustive, so an unhandled node type panics rather than aliasing.
		return CloneNode(n), nil
	}
}

// checkPredictedGrid asserts a matrix RangeWindow's current bounds match
// the grid predicted at this spine depth. Either the bounds are unpinned
// (zero Start and End — the subquery-inner shape, filled by the re-anchor)
// or they already sit exactly on the predicted grid
// ([predStart, predEnd] with OuterRange == predEnd - predStart). Anything
// else — most importantly an @-pinned End that diverges from the predicted
// grid — is rejected so the solver routes A.
func checkPredictedGrid(r *RangeWindow, predStart, predEnd time.Time) error {
	if r.Start.IsZero() && r.End.IsZero() {
		// Unpinned: the subquery lowerings emit OuterRange + Step but leave
		// Start/End for the widen pass. Re-anchoring fills the grid.
		return nil
	}
	if r.Start.Equal(predStart) && r.End.Equal(predEnd) && r.OuterRange == predEnd.Sub(predStart) {
		// Already gridded exactly on the predicted grid.
		return nil
	}
	return fmt.Errorf("%w: node bounds (Start=%v End=%v OuterRange=%s) "+
		"do not match predicted grid (Start=%v End=%v OuterRange=%s) — an @-pinned or non-grid anchor",
		ErrReanchorGridMismatch,
		r.Start, r.End, r.OuterRange,
		predStart, predEnd, predEnd.Sub(predStart))
}

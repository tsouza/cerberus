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

// ReanchorRange returns a re-anchored view of n whose windowed spine is
// re-anchored to evaluate one row per anchor across [start, end], with each
// matrix RangeWindow's own input spine widened by a further Range of lookback
// so every anchor finds the samples it needs.
//
// It is the head-agnostic, no-mutate generalization of
// promql.widenSubquerySpine (internal/promql/subquery.go): where
// widenSubquerySpine mutates the spine in place, ReanchorRange leaves the
// input Node and every expr tree reachable from it byte-identical.
//
// Structural sharing (copy-on-write). ReanchorRange clones only the
// O(spine-depth) nodes it actually re-grids — the matrix RangeWindow /
// RangeLWR / Project / Aggregate / TopK / Filter chain down the windowed
// spine — and SHARES every immutable off-spine subtree, expr, projection,
// and agg-func pointer with the input verbatim. The off-spine subtree is
// byte-identical across all K shards (it does not move in time), so sharing
// it is exactly equal to the old per-shard CloneNode (`Equal` is preserved)
// while doing K+1 fewer full-subtree copies. The returned tree is therefore
// NOT independently mutable: the solver runs the K shards through emit only,
// which never mutates a plan node in place. That no-mutate-after-slice
// contract is enforced by the differential immutability guards in
// internal/solver (TestSlice_NoSharedMutation and siblings) and by the
// per-arm immutability tests below — a future pass that mutates a shared
// off-spine node in place must add its own clone or it will corrupt sibling
// shards.
//
// Defensive grid-prediction check (the @-modifier guard, §"Eligibility signals" of
// docs/solver.md). A windowed matrix node is re-anchored only
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
// unchanged. Every other node type is SHARED verbatim (the original pointer,
// not a copy) — it is below the spine and does not move in time.
//
// RangeLWR (the bare-selector last-with-respect-to leaf, the deriv / idelta /
// irate / instant-LWR / negative-offset families) re-anchors the same way:
// matrix-grid RangeLWRs (Step > 0) re-grid their (Start, End) and recurse into
// their input widened by the offset-aware membership lookback Offset+Lookback;
// an instant-shape RangeLWR (Step == 0) terminates the walk. The
// grid-prediction guard applies identically, so an @-pinned RangeLWR routes A.
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
			// Instant-shape window: not re-gridded, share verbatim.
			return v, nil
		}
		if err := checkPredictedGrid(v, start, end); err != nil {
			return nil, err
		}
		// Clone only this spine node; GroupBy / Scalars / ScalarExprs are
		// off-grid immutable, so share the original slice headers (the shard
		// re-grids Start/End/OuterRange only — it never mutates these).
		c := *v
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
	case *RangeLWR:
		// The bare-selector last-with-respect-to leaf. Its eval grid is
		// [Start, End] spaced by Step; each anchor reduces the most-recent
		// sample in its offset-aware staleness window
		// `(anchor - Offset - Lookback, anchor - Offset]`. The per-(series,
		// anchor) value depends only on that window's membership, not on the
		// scan lower bound — it is registered slice-invariant — so re-anchoring
		// to a sub-grid yields exactly the rows route A would have produced for
		// those anchors. Same no-mutate + grid-prediction discipline as
		// the RangeWindow arm: the grid is filled only when the node is either
		// unpinned (the slicer's unpinSpine shape) or already sits exactly on
		// the predicted grid; an @-pinned divergence routes A via
		// ErrReanchorGridMismatch.
		if v.Step <= 0 {
			// No anchor grid to re-grid (an instant-shape LWR); share verbatim.
			return v, nil
		}
		if err := checkPredictedGridLWR(v, start, end); err != nil {
			return nil, err
		}
		c := *v
		c.Start = start
		c.End = end
		// The membership window looks back Offset+Lookback from each anchor;
		// widen the input spine by that much so every anchor finds its samples.
		// Offset enters with its sign (a negative offset shifts the window
		// forward), mirroring the solver-owned sign-aware scan floor.
		input, err := reanchor(v.Input, start.Add(-v.Offset-v.Lookback), end)
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
		// Projections are off-grid immutable: share the slice header.
		return &Project{Input: input, Projections: v.Projections}, nil
	case *Aggregate:
		input, err := reanchor(v.Input, start, end)
		if err != nil {
			return nil, err
		}
		// GroupBy / GroupByAliases / AggFuncs are off-grid immutable: share.
		c := *v
		c.Input = input
		return &c, nil
	case *TopK:
		input, err := reanchor(v.Input, start, end)
		if err != nil {
			return nil, err
		}
		c := *v
		c.Input = input
		// KExpr / By / SortExpr / Columns are below the spine (KExpr is a
		// computed-K scalar plan): off-grid immutable, share verbatim — they
		// do not participate in the anchor grid.
		return &c, nil
	case *Filter:
		input, err := reanchor(v.Input, start, end)
		if err != nil {
			return nil, err
		}
		// Predicate is off-grid immutable: share.
		return &Filter{Input: input, Predicate: v.Predicate}, nil
	case *VectorJoin:
		// A step-aligned vector-vector join carries NO own anchor grid and NO
		// lookback: each arm is an independent windowed spine that already
		// evaluates over the request grid, and the join step-aligns the two on
		// the per-anchor TimestampColumn. Re-anchor BOTH arms onto the SAME
		// [start, end] the join was asked for (no widening — the arms' own
		// RangeWindow / RangeLWR nodes do their -Range / -Lookback widening),
		// then copy-on-write the join node, re-filling the two arms and sharing
		// the immutable modifier fields (Op / Match / Card / Include /
		// ReturnBool / StepAligned) + the four column names verbatim. An
		// @-pinned or grid-divergent arm surfaces ErrReanchorGridMismatch from
		// the recursion, aborting the whole re-anchor to route A. The
		// instant-mode (!StepAligned) join is kept off this path by the
		// planner's sawInstantVectorJoin fail-closed guard (its emitter
		// synthesizes the join-side timestamp with now64(9), a wall-clock that
		// diverges across shards); registration is by node kind, so the guard —
		// not this case — is what excludes it.
		left, err := reanchor(v.Left, start, end)
		if err != nil {
			return nil, err
		}
		right, err := reanchor(v.Right, start, end)
		if err != nil {
			return nil, err
		}
		c := *v
		c.Left = left
		c.Right = right
		return &c, nil
	default:
		// Off the windowed spine: SHARE the immutable subtree verbatim. The
		// off-spine subtree is byte-identical across all K shards (it does not
		// move in time), so sharing the original pointer is exactly equal to
		// the old per-shard CloneNode while doing K+1 fewer subtree copies.
		// Soundness rests on the no-mutate-after-slice contract: the solver
		// runs each shard through emit only, never mutating a plan node in
		// place (enforced by the differential immutability guards in
		// internal/solver). A future pass that DOES mutate a shared node must
		// clone it first or it will corrupt sibling shards.
		return n, nil
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

// checkPredictedGridLWR is checkPredictedGrid for a RangeLWR. The LWR carries
// no OuterRange field — its grid span is End-Start directly — so the predicted
// grid is just [predStart, predEnd]. Either the bounds are unpinned (zero Start
// and End — the slicer's unpinSpine shape, filled by the re-anchor) or they
// already sit exactly on the predicted grid. Anything else — most importantly
// an @-pinned End diverging from the predicted grid — is rejected so the solver
// routes A.
func checkPredictedGridLWR(r *RangeLWR, predStart, predEnd time.Time) error {
	if r.Start.IsZero() && r.End.IsZero() {
		return nil
	}
	if r.Start.Equal(predStart) && r.End.Equal(predEnd) {
		return nil
	}
	return fmt.Errorf("%w: RangeLWR bounds (Start=%v End=%v) "+
		"do not match predicted grid (Start=%v End=%v) — an @-pinned or non-grid anchor",
		ErrReanchorGridMismatch,
		r.Start, r.End,
		predStart, predEnd)
}

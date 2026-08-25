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
// matrix RangeWindow's own input spine widened per RangeWindow.InputWindow
// (Offset+Range of lookback) so every anchor finds the samples it needs.
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
// End — returns ErrReanchorGridMismatch so the caller aborts to route A
// rather than emit a shard plan that silently disagrees with the @
// semantics. Every range-vector lowering keeps an @-pinned window in
// Step==0 instant shape (broadcasting the single pinned value across the
// step grid via rangeGridShapeFor), so this guard is the last line of
// defense against any other shape that reaches ReanchorRange with bounds
// that do not match the predicted grid.
//
// Spine shape mirrors widenSubquerySpine exactly so the two stay
// equivalent on post-optimizer plans (pinned by the equivalence test in
// internal/promql): matrix RangeWindows (Step > 0) re-anchor and recurse
// into their input via RangeWindow.InputWindow (start.Add(-Offset-Range));
// instant RangeWindows (Step == 0) terminate the walk; the wrapper nodes the
// subquery lowerings interpose (Project / Aggregate / TopK / Filter) pass
// the requirement through unchanged. Every other node type is SHARED
// verbatim (the original pointer, not a copy) — it is below the spine and
// does not move in time.
//
// RangeWindowGridNative (the ClickHouse-native timeSeries<fn>ToGrid lowering of
// the same window semantics) re-anchors exactly like the matrix RangeWindow —
// re-grid (Start, End), widen the input spine by Offset+Range via
// RangeWindowGridNative.InputWindow — with the grid-prediction guard in its
// two-bound (no OuterRange field) form. UnionAll carries no grid of its own
// and re-anchors EVERY arm onto the same [start, end], which is what lets a
// mixed `UnionAll{RangeWindowGridNative, RangeWindow}` spine slice: the arms are
// concatenated positionally, so all of them must move together or the shards
// double-count the arm that did not.
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
		return reanchorRangeWindow(v, start, end)
	case *RangeWindowGridNative:
		return reanchorRangeWindowGridNative(v, start, end)
	case *UnionAll:
		return reanchorUnionAll(v, start, end)
	case *RangeLWR:
		return reanchorRangeLWR(v, start, end)
	case *RangeBucketFanout:
		return reanchorRangeBucketFanout(v, start, end)
	case *HistogramQuantile:
		input, err := reanchor(v.Input, start, end)
		if err != nil {
			return nil, err
		}
		// Phi / other scalar params are off-grid immutable: share.
		c := *v
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
		return reanchorVectorJoin(v, start, end)
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

// reanchorRangeWindow re-grids the fan-out matrix window: fill (Start, End,
// OuterRange) from the requested grid, then recurse into the input spine widened
// by RangeWindow.InputWindow (Offset+Range of lookback) so every anchor finds
// the samples its window covers.
//
// Instant-shape RangeWindows resolve a single anchor themselves and terminate
// the walk (mirrors widenSubquerySpine's Step <= 0 guard).
func reanchorRangeWindow(v *RangeWindow, start, end time.Time) (Node, error) {
	if v.Step <= 0 {
		// Instant-shape window: not re-gridded, share verbatim.
		return v, nil
	}
	if err := checkPredictedGrid(v, start, end); err != nil {
		return nil, err
	}
	// Clone only this spine node; GroupBy / Scalars / ScalarExprs are off-grid
	// immutable, so share the original slice headers (the shard re-grids
	// Start/End/OuterRange only — it never mutates these). DeltaPrefixAggregateInput
	// is shared unexamined too (c := *v below forwards it verbatim, with no
	// recursive reanchor call of its own) — safe because that subtree
	// carries no embedded time bound of its own to begin with
	// (buildDeltaPrefixAggregateArm only ever wraps a Scan in a metric-name
	// Filter, never a time-range one); the ONLY bound ever applied to it is
	// derived at EMIT time in chsql from THIS RangeWindow's own (already
	// correctly reanchored) End/Range fields
	// (deltaMatrixLevelSourceAggregate's `latestDay`). If this field ever
	// grows its own independent time bound, that safety argument breaks and
	// this field must be reanchored explicitly, the same as Input below.
	c := *v
	c.Start = start
	c.End = end
	c.OuterRange = end.Sub(start)
	// Each of this window's anchors looks back Offset+Range (the window is
	// [End-Offset-Range, End-Offset], see the RangeWindow doc comment); widen
	// the input spine by that much via the single shared owner of this
	// arithmetic so the inner grid covers every anchor's window.
	inStart, inEnd := v.InputWindow(start, end)
	input, err := reanchor(v.Input, inStart, inEnd)
	if err != nil {
		return nil, err
	}
	c.Input = input
	return &c, nil
}

// reanchorRangeWindowGridNative re-grids the ClickHouse-native timeSeries<fn>ToGrid
// lowering of the SAME window semantics reanchorRangeWindow handles: the
// aggregate is handed (Start, End, Step, Range) and evaluates grid point i from
// the samples in `(anchor_i - Offset - Range, anchor_i - Offset]`. So
// re-anchoring is the same two moves — re-grid (Start, End), widen the input
// spine by Offset+Range — and the same discipline applies: the grid is filled
// only when the node is either unpinned (the slicer's UnpinSpine shape) or
// already sits exactly on the predicted grid, and an @-pinned divergence routes
// A via ErrReanchorGridMismatch.
//
// The one structural difference from the fan-out arm is that this node carries
// no OuterRange field — its grid span IS End-Start — so the guard is
// checkPredictedGridNative, the RangeLWR-shaped two-bound form rather than the
// three-bound one.
func reanchorRangeWindowGridNative(v *RangeWindowGridNative, start, end time.Time) (Node, error) {
	if v.Step <= 0 {
		// No anchor grid to re-grid. The lowering only builds this node in range
		// mode, so this is unreachable from a real plan; keeping the arm
		// symmetric with RangeWindow / RangeLWR means a hand-built or later
		// instant shape terminates the walk instead of being re-gridded onto
		// bounds it has no anchors for.
		return v, nil
	}
	if err := checkPredictedGridNative(v, start, end); err != nil {
		return nil, err
	}
	// Clone only this spine node; GroupBy / Recollapse / Scalars are off-grid
	// immutable, so share the original slice headers.
	c := *v
	c.Start = start
	c.End = end
	inStart, inEnd := v.InputWindow(start, end)
	input, err := reanchor(v.Input, inStart, inEnd)
	if err != nil {
		return nil, err
	}
	c.Input = input
	return &c, nil
}

// reanchorUnionAll re-anchors a UNION ALL of independent windowed spines. It
// carries NO grid and NO lookback of its own: every arm already evaluates over
// the request grid and each does its own -Offset-Range widening, so every arm
// re-anchors onto the SAME [start, end] the union was asked for — exactly
// reanchorVectorJoin's shape, generalised from two fixed sides to N.
//
// Re-anchoring EVERY arm is what makes the union re-anchorable at all: the arms
// are concatenated positionally, so a shard that re-gridded one arm and shared
// another verbatim would emit one sub-grid's rows beside the full grid's, and
// the K-way composition would double-count every anchor of the shared arm. An
// @-pinned or grid-divergent arm surfaces ErrReanchorGridMismatch from the
// recursion and aborts the whole re-anchor to route A.
//
// A union whose arms carry no grid at all (the classic-histogram
// companion-suffix routing, where the UnionAll sits BENEATH the windowed node)
// recurses into arms that all hit the share-verbatim default, so this allocates
// a new Inputs slice and returns an Equal tree — the same cost the Project /
// Filter arms already pay.
func reanchorUnionAll(v *UnionAll, start, end time.Time) (Node, error) {
	inputs := make([]Node, len(v.Inputs))
	for i, in := range v.Inputs {
		arm, err := reanchor(in, start, end)
		if err != nil {
			return nil, err
		}
		inputs[i] = arm
	}
	return &UnionAll{Inputs: inputs}, nil
}

// reanchorRangeLWR re-grids the bare-selector last-with-respect-to leaf. Its
// eval grid is [Start, End] spaced by Step; each anchor reduces the most-recent
// sample in its offset-aware staleness window
// `(anchor - Offset - Lookback, anchor - Offset]`. The per-(series, anchor)
// value depends only on that window's membership, not on the scan lower bound —
// it is registered slice-invariant — so re-anchoring to a sub-grid yields
// exactly the rows route A would have produced for those anchors. Same
// no-mutate + grid-prediction discipline as the RangeWindow arm: the grid is
// filled only when the node is either unpinned (the slicer's UnpinSpine shape)
// or already sits exactly on the predicted grid; an @-pinned divergence routes A
// via ErrReanchorGridMismatch.
func reanchorRangeLWR(v *RangeLWR, start, end time.Time) (Node, error) {
	if v.Step <= 0 {
		// No anchor grid to re-grid (an instant-shape LWR); share verbatim.
		return v, nil
	}
	if err := checkPredictedGridLWR(v, start, end); err != nil {
		return nil, err
	}
	c := *v
	if v.StepAlign {
		// Epoch-aligned subquery inner (see RangeLWR.StepAlign): the
		// unsliced query would have evaluated this grid at absolute-epoch
		// (phase 0) anchors, independent of [start, end]. epochFloor's
		// residue from phase 0 does not depend on what offset produced
		// its input, so re-deriving the floor directly from this shard's
		// own predicted bounds reproduces the SAME phase every shard
		// would use — keeping every shard commensurate with the others
		// and with the unsliced grid.
		c.End = epochFloor(end, v.Step)
		c.Start = epochFloor(start, v.Step).Add(v.Step)
		if c.Start.After(c.End) {
			c.Start = c.End
		}
	} else {
		c.Start = start
		c.End = end
	}
	// The membership window looks back Offset+Lookback from each anchor;
	// widen the input spine by that much so every anchor finds its samples.
	// Offset enters with its sign (a negative offset shifts the window
	// forward), mirroring the solver-owned sign-aware scan floor.
	input, err := reanchor(v.Input, c.Start.Add(-v.Offset-v.Lookback), c.End)
	if err != nil {
		return nil, err
	}
	c.Input = input
	return &c, nil
}

// reanchorVectorJoin re-anchors a step-aligned vector-vector join. It carries
// NO own anchor grid and NO lookback: each arm is an independent windowed spine
// that already evaluates over the request grid, and the join step-aligns the two
// on the per-anchor TimestampColumn. BOTH arms re-anchor onto the SAME
// [start, end] the join was asked for (no widening — the arms' own RangeWindow /
// RangeLWR nodes do their -Range / -Lookback widening), then the join node is
// copy-on-written, re-filling the two arms and sharing the immutable modifier
// fields (Op / Match / Card / Include / ReturnBool / StepAligned) + the four
// column names verbatim. An @-pinned or grid-divergent arm surfaces
// ErrReanchorGridMismatch from the recursion, aborting the whole re-anchor to
// route A.
//
// The instant-mode (!StepAligned) join is kept off this path by the planner's
// sawInstantVectorJoin fail-closed guard (its emitter synthesizes the join-side
// timestamp with now64(9), a wall-clock that diverges across shards);
// registration is by node kind, so that guard — not this function — is what
// excludes it.
// reanchorRangeBucketFanout re-grids the array-aggregate fan-out behind the
// classic-histogram families — the RangeLWR sibling that collapses each
// (series, anchor)'s raw BucketCounts/ExplicitBounds rows via
// `GROUP BY (<user keys>, anchor)` instead of picking one last sample. Its
// per-anchor membership window is the same shape as RangeLWR's:
// `(anchor - Offset - Lookback, anchor - Offset]`, and it carries no
// OuterRange field and no StepAlign mode (RangeBucketFanout only ever
// lowers already gridded to the request's own step, never as a
// subquery-inner epoch-aligned leaf) — so this mirrors reanchorRangeLWR
// with the StepAlign branch dropped.
func reanchorRangeBucketFanout(v *RangeBucketFanout, start, end time.Time) (Node, error) {
	if v.Step <= 0 {
		// No anchor grid to re-grid; share verbatim.
		return v, nil
	}
	if err := checkPredictedGridBucketFanout(v, start, end); err != nil {
		return nil, err
	}
	c := *v
	c.Start = start
	c.End = end
	// The membership window looks back Offset+Lookback from each anchor;
	// widen the input spine by that much so every anchor finds its samples.
	input, err := reanchor(v.Input, c.Start.Add(-v.Offset-v.Lookback), c.End)
	if err != nil {
		return nil, err
	}
	c.Input = input
	return &c, nil
}

func reanchorVectorJoin(v *VectorJoin, start, end time.Time) (Node, error) {
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

// epochFloor floors t to the nearest absolute-epoch (phase 0) multiple of
// step at or before t. It is the chplan-local twin of
// internal/promql.epochFloor — duplicated rather than shared because chplan
// cannot import promql (promql imports chplan) — and must stay in exact
// lock-step with it: both implement PromQL's subquery inner-sample-grid
// floor, and a StepAlign RangeLWR's phase is only reproducible across shards
// if every caller floors identically.
func epochFloor(t time.Time, step time.Duration) time.Time {
	stepNS := step.Nanoseconds()
	ns := t.UTC().UnixNano()
	floor := ns / stepNS
	if ns%stepNS != 0 && ns < 0 {
		floor--
	}
	return time.Unix(0, floor*stepNS).UTC()
}

// checkPredictedGridNative is checkPredictedGrid for a RangeWindowGridNative. Like
// the RangeLWR form, the node carries no OuterRange field — its grid span IS
// End-Start — so the predicted grid is just [predStart, predEnd]. Either the
// bounds are unpinned (zero Start and End — the slicer's UnpinSpine shape,
// filled by the re-anchor) or they already sit exactly on the predicted grid.
// Anything else — most importantly an @-pinned End diverging from the predicted
// grid — is rejected so the solver routes A.
func checkPredictedGridNative(r *RangeWindowGridNative, predStart, predEnd time.Time) error {
	if r.Start.IsZero() && r.End.IsZero() {
		return nil
	}
	if r.Start.Equal(predStart) && r.End.Equal(predEnd) {
		return nil
	}
	return fmt.Errorf("%w: RangeWindowGridNative bounds (Start=%v End=%v) "+
		"do not match predicted grid (Start=%v End=%v) — an @-pinned or non-grid anchor",
		ErrReanchorGridMismatch,
		r.Start, r.End,
		predStart, predEnd)
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

// checkPredictedGridBucketFanout is checkPredictedGrid for a
// RangeBucketFanout. Like RangeLWR it carries no OuterRange field — its
// grid span is End-Start directly — so the predicted grid is just
// [predStart, predEnd]. Either the bounds are unpinned (zero Start and End —
// the slicer's UnpinSpine shape) or they already sit exactly on the
// predicted grid. Anything else is rejected so the solver routes A.
func checkPredictedGridBucketFanout(r *RangeBucketFanout, predStart, predEnd time.Time) error {
	if r.Start.IsZero() && r.End.IsZero() {
		return nil
	}
	if r.Start.Equal(predStart) && r.End.Equal(predEnd) {
		return nil
	}
	return fmt.Errorf("%w: RangeBucketFanout bounds (Start=%v End=%v) "+
		"do not match predicted grid (Start=%v End=%v) — an @-pinned or non-grid anchor",
		ErrReanchorGridMismatch,
		r.Start, r.End,
		predStart, predEnd)
}

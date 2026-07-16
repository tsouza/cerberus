package chplan

import "reflect"

// IsSliceInvariant reports whether n's node kind is registered as
// slice-invariant: its per-(series, anchor) output is a pure function of
// the samples inside each anchor's window, independent of where the input
// scan's lower bound sits. The sharded-pushdown solver may time-slice a
// plan only if EVERY node in it is slice-invariant; a single unregistered
// node anywhere → route A.
//
// Why a registry, not a type switch. The marker is an explicit,
// machine-checkable assertion the author must opt a node kind into — never
// a `switch n.(type)` the caller updates implicitly. The hazard is the #92
// lagInFrame interaction: if the A-prime cumulative-counter rewrite ever
// ships a formulation whose per-anchor value depends on scan order
// (a window function like lagInFrame seeded at the scan's first row), a
// type-whitelist would route that scan-order-dependent shape SILENTLY into
// K shards — each shard's scan starts at a different row, so each shard's
// lagInFrame seed differs, and the concatenated result is wrong with no
// compile-time or test-time signal. The registry forces every node kind
// (including any #92-substituted shape, and every new node) to be proven
// slice-invariant by the §Parity fixture family before it is admitted.
// Unregistered → false, always.
func IsSliceInvariant(n Node) bool {
	if n == nil {
		return false
	}
	_, ok := sliceInvariantKinds[reflect.TypeOf(n)]
	return ok
}

// sliceInvariantKinds is the registry. Each entry is a node kind whose
// slice-invariance has been argued: its emitted value at an (anchor, series)
// pair is determined entirely by the samples whose timestamps fall in that
// anchor's window, so evaluating it over a sub-grid of anchors with a
// correspondingly-bounded input scan yields exactly the rows route A would
// have produced for those anchors.
//
// Phase 1 set (docs/solver.md §"Eligibility signals", signal 1):
//
//   - Scan / Filter / Project — pure row-wise passthroughs; no cross-row,
//     cross-anchor, or scan-order dependence.
//   - Aggregate — keyed per (series, anchor) (the GroupBy carries the
//     anchor key in the matrix lowerings), so each output row reduces only
//     the rows of one anchor's window.
//   - RangeWindow / RangeLWR / RangeBucketFanout — the windowed-array and
//     bounded sample-fan-out families: each (series, anchor) value is the
//     reduce of exactly that anchor's `(anchor - Offset - Range, anchor -
//     Offset]` window membership, independent of the scan lower bound.
//   - StepGrid — emits the anchor grid itself; a sub-grid is a subset.
//   - UnionAll — slice-invariant iff every arm is (checked structurally by
//     the whole-plan walk, since each arm is itself visited).
//   - VectorJoin — a step-aligned vector-vector binary join. Each output row
//     is the per-pair binary op of two per-(match-key, anchor) inputs joined
//     on the match key AND the anchor timestamp (the emitter ANDs
//     `L.TimestampColumn = R.TimestampColumn` into the ON clause when
//     StepAligned, and adds TimestampColumn to each side's GROUP BY). So each
//     joined row reduces only the samples of one anchor's window on each arm,
//     independent of the scan lower bound. This holds across all matching
//     (on/ignoring), all cardinalities (group_left/group_right — the
//     many-to-one dedup throwIf(uniqExact>1) + Include mapConcat are
//     per-(match-key, anchor) because the anchor timestamp is IN the join
//     key), and all ops incl `bool`. BOUNDARY: only the StepAligned shape is
//     safe. The instant-mode (StepAligned==false) join synthesizes its
//     join-side timestamp with now64(9) — a wall-clock that diverges across
//     shards. Registration here is by node kind, so it admits the instant
//     shape too; the solver's planner carries an explicit sawInstantVectorJoin
//     fail-closed guard (ReasonInstantJoin) that keeps !StepAligned joins on
//     route A. VectorSetOp / NaryVectorSetOp (and/or/unless) remain absent —
//     each is its own PR.
//
// Extension point. Phase-3 node families (TopK as per-anchor LIMIT K BY,
// VectorSetOp, HistogramQuantile{,Native}, AbsentOverTime, the metrics_*
// TraceQL family, nested spines under the lcm clamp) are DELIBERATELY ABSENT:
// each enters this registry only with its own slice-invariance proof + the
// reset-at-seam fixture family, one node family per PR. To register a kind,
// argue its per-(series, anchor) output is scan-lower-bound-independent, add
// it here, and extend the §Parity lanes — do not add a kind merely because it
// "looks safe".
var sliceInvariantKinds = func() map[reflect.Type]struct{} {
	kinds := []Node{
		&Scan{},
		&Filter{},
		&Project{},
		&Aggregate{},
		&RangeWindow{},
		&RangeLWR{},
		&RangeBucketFanout{},
		&StepGrid{},
		&UnionAll{},
		&VectorJoin{},
	}
	m := make(map[reflect.Type]struct{}, len(kinds))
	for _, k := range kinds {
		m[reflect.TypeOf(k)] = struct{}{}
	}
	return m
}()

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
//
//   - Aggregate — keyed per (series, anchor) (the GroupBy carries the
//     anchor key in the matrix lowerings), so each output row reduces only
//     the rows of one anchor's window.
//
//   - RangeWindow / RangeLWR / RangeBucketFanout — the windowed-array and
//     bounded sample-fan-out families: each (series, anchor) value is the
//     reduce of exactly that anchor's `(anchor - Offset - Range, anchor -
//     Offset]` window membership, independent of the scan lower bound.
//
//     RangeWindow.LagAdjacency=true (issue #2759) is the #92 hazard this
//     doc's own opening paragraph warns about — a lagInFrame formulation
//     seeded at the scan's first row — and it is admitted into this SAME
//     entry (not a separate registration) because the hazard does not
//     materialise here. lagInFrame runs over Input widened by
//     RangeWindow.InputWindow (Offset + Range past the shard's own oldest
//     anchor — the identical widening every RangeWindow shard already scans,
//     proven by this entry's own criterion above), and every per-(series,
//     anchor) value the emitter derives from the annotation ONLY consults a
//     pair whose prev_ts satisfies `prev_ts > anchor - range` — checked BY
//     CONSTRUCTION before the pair contributes anything (chsql's
//     lagAdjacencyValidPrevFrag). A pair a shard's lagInFrame seeds
//     WRONG (defaults to the column's zero value at the first row of a
//     shard's own scan, or — for a row whose TRUE full-series predecessor
//     sits before the widened scan's lower bound — skips straight to a
//     later, wrong "prev") always has prev_ts AT OR BEFORE that widened lower
//     bound, which is itself `<= anchor - range` for every anchor the shard
//     covers (the widening IS anchor_min - Offset - Range); the validity
//     check therefore excludes every such pair whether route A or a K-sharded
//     route B produced it. A pair the validity check ADMITS has prev_ts
//     strictly inside the widened scan, which places prev_ts's OWN sample row
//     inside that same shard's scan too — a plain contiguous time-range read,
//     not a sample — so lagInFrame finds the SAME true immediately-preceding
//     row un-sliced execution would have found. Verified by dual-emit parity
//     (LagAdjacency=false vs =true, internal/chsql's
//     range_window_lag_adjacency_chdb_test.go) across duplicate-timestamp,
//     all-NaN, and DELTA-temporality-reset windows — not merely asserted.
//
//     RangeWindow.FixedAccumulatorExtrapolated=true (issue #2760, rate() /
//     increase() / delta()) rests on the SAME criterion, extended to its own
//     two window-function passes (chsql/range_window_fixed_accumulator.go).
//     Its reset-correction term reuses lagInFrame exactly as LagAdjacency
//     does — same lagAdjacencyValidPrevFrag admission check, same argument.
//     Its FIRST pass (fixedAccumDedupLayer's forward-looking leadInFrame,
//     collapsing duplicate timestamps) carries no analogous hazard for a
//     different reason: leadInFrame there only ever looks past rows sharing
//     the CURRENT row's own timestamp, and two rows sharing one exact
//     timestamp can never be split across a shard boundary in the first
//     place (a shard's own boundary is itself a `ts <= X` / `ts > X` cut, so
//     equal-ts rows compare equal and always land on the same side) —
//     whichever shard a duplicate-timestamp run reaches, the WHOLE run
//     reaches it together, so the forward look never needs to see past the
//     shard's own scan to find its answer. Verified by dual-emit parity
//     (FixedAccumulatorExtrapolated=false vs =true,
//     range_window_fixed_accumulator_chdb_test.go) across duplicate-timestamp,
//     single-sample, exactly-two-samples-at-the-boundary, and
//     counter-reset windows.
//
//     RangeWindow.SortedSlabOverTime=true (issue #2761, sum_over_time() /
//     avg_over_time(), widened by issue #2804 to first_over_time() /
//     stddev_over_time() / stdvar_over_time() / mad_over_time()) needs no
//     comparable argument at all: unlike LagAdjacency /
//     FixedAccumulatorExtrapolated it reads no window function (no
//     lagInFrame/leadInFrame seeded at the scan's first row).
//     chsql/range_window_sorted_slab.go's per-anchor value is
//     overTimeArrayValueFrag(r.Func, ...) — arraySum/arrayAvg, a position
//     pick, or a two-pass moment computation depending on r.Func — over
//     `arrayFilter(samples, a-range < ts <= a)`, which — same as the base
//     RangeWindow entry above, and REGARDLESS of which of the six reducers
//     r.Func selects — is a pure function of that anchor's window membership
//     over `samples`
//     (itself the per-series groupArray over Input, widened by
//     RangeWindow.InputWindow exactly like every other RangeWindow shard),
//     with no cross-anchor or scan-position state threaded through. Verified
//     by dual-emit parity (SortedSlabOverTime=false vs =true,
//     range_window_sorted_slab_chdb_test.go) across duplicate-timestamp,
//     single-sample, and empty-window cases.
//
//   - RangeWindowGridNative — the ClickHouse-native timeSeries<fn>ToGrid lowering
//     of the SAME window semantics. The aggregate is handed (start, end, step,
//     window) and evaluates grid point i from exactly the samples inside
//     `(anchor_i - Offset - Range, anchor_i - Offset]`, so its per-(series,
//     anchor) value is scan-lower-bound-independent for the identical reason
//     the fan-out arm's is — the criterion above, not a resemblance argument.
//     Note what is NOT being claimed: the aggregate's INTERNAL evaluation is
//     order-dependent (extrapolatedRate walks the window's samples in time
//     order and reads the first/last pair), but that order is a property of
//     the window's own membership, which slicing does not touch. The hazard
//     this registry exists to police is a value seeded at the SCAN's first row
//     (lagInFrame), and the aggregate reads no such seed: every shard hands it
//     a scan widened by Offset+Range past that shard's oldest anchor, so each
//     grid point sees the same sample multiset it saw unsliced. Measured
//     against the fan-out arm on a 500k-row / 5000-series seed across three
//     temporality mixes: identical cell key sets, zero missing and zero extra
//     cells (issue #2117).
//
//   - RangeBucketGridNative — the ClickHouse-native classic-histogram ladder
//     that serves `histogram_quantile(phi, <agg> by(le) (rate(<bucket>[range])))`.
//     Its per-(series, anchor) output is scan-lower-bound-independent stage by
//     stage, by the same criterion RangeWindowGridNative's entry rests on, not
//     by resemblance to it:
//     the Level-0 unnest is row-wise (each stored row yields its own `le` rungs
//     from its OWN bounds/counts, reading no other row); Level 1 runs
//     timeSeriesRateToGrid and timeSeriesResetsToGrid per (series, `le` rung),
//     and each answers grid point i from exactly the samples inside
//     `(anchor_i - Offset - Range, anchor_i - Offset]`; Level 2's `ifNull(rate,
//     0)` short-window resolution and the `IS NOT NULL` presence filter both
//     read one (rung, anchor) cell; Level 3 regroups keyed by (series, anchor)
//     and its `HAVING max(_hqn_short) = 0` min-samples drop reads the +Inf
//     rung's own NULL AT THAT ANCHOR (the +Inf rung is reported by every stored
//     row, so its per-anchor sample set IS that anchor's window membership);
//     Levels 4-5 are row-wise array reshapes of one (series, anchor) row. No
//     stage carries a value seeded at the scan's first row — the lagInFrame
//     hazard this registry polices — and every shard hands the aggregate a scan
//     widened by Offset+Range past its own oldest anchor, so each grid point
//     sees the same sample multiset it saw unsliced.
//     Registering it is what lets the failure-driven route memo answer a
//     wide-window classic-histogram quantile that busts a single query's memory
//     cap by time-slicing it (#2677): before this entry the node was the one
//     grid carrier route B could not shard, so such a query had no relief at
//     all. See internal/chplan/reanchor.go's *RangeBucketGridNative arm and the
//     route-A-vs-K-sharded differential fixtures in internal/solver.
//
//   - StepGrid — emits the anchor grid itself; a sub-grid is a subset.
//
//   - UnionAll — slice-invariant iff every arm is (checked structurally by
//     the whole-plan walk, since each arm is itself visited).
//
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
//   - HistogramQuantile — the classic-histogram bucket-array-to-quantile
//     interpolation. Its input is a reshape Project over a RangeBucketFanout,
//     whose GROUP BY carries the anchor key (AnchorAlias, always prepended),
//     so HistogramQuantile's own per-(series, anchor) computation reads
//     exactly one input row's BucketCounts/ExplicitBounds columns — no
//     cross-anchor read, no scan-order dependence. By default
//     (UseNativeQuantileAggregate == false) the emitter renders plain
//     arrayMap/arraySort over that one row's array columns, no window
//     function, no GROUP BY of its own. When chopt.FeatureQuantilePromHistogram
//     is boot-enabled (UseNativeQuantileAggregate == true — opt-in only,
//     AutoSelect: false), the emitter instead ARRAY JOINs that SAME single
//     row's arrays into per-bucket sub-rows and GROUP BYs them back down by
//     the identical GroupBy key the row already carried — see
//     internal/chsql/histogram_quantile_rankwalk_native.go. The GROUP BY
//     never spans more than the one input row's own exploded sub-rows, so it
//     collapses ACROSS BUCKETS, never across series or anchors, and the same
//     locality argument holds unchanged: slicing the anchor grid upstream
//     changes which (series, anchor) rows this node sees, never what any
//     surviving row's own group computes. Its per-(series, anchor) output is
//     therefore exactly as scan-lower-bound-independent as its
//     RangeBucketFanout input already is, under EITHER emission. See
//     internal/chplan/reanchor.go's *HistogramQuantile arm (a pass-through,
//     mirroring *Project) and internal/solver/avb_chdb_lane_test.go's
//     classic-histogram fixtures for the differential (route A vs
//     K-sharded route B) proof this registry entry rests on —
//     TestSolver_AvsB_ChDB_Differential covers the default (fan-out)
//     emission and TestSolver_AvsB_ChDB_Differential_NativeQuantileHistogram
//     covers the opt-in native emission (cerberus issue #2791), so the
//     locality argument above is empirically proven end-to-end under BOTH.
//     Issue #2790's PR 2 later bounded the native emission's own ARRAY JOIN
//     to a small window (at most 3 rungs per row, not the whole bucket
//     ladder — internal/chsql/histogram_quantile_rankwalk_native.go's own
//     header doc) purely to cut memory at high series cardinality; every
//     new stage it added still derives from that SAME single input row's
//     own columns (Star() propagation, no cross-row read), so the locality
//     argument is unaffected and TestSolver_AvsB_ChDB_Differential_
//     NativeQuantileHistogram re-proves it against the bounded shape with
//     zero diffs — no new differential fixture was needed.
//     HistogramQuantileNative / HistogramProjection (the
//     native/exponential-histogram siblings) remain DELIBERATELY ABSENT —
//     same shape family, but no production traffic exists yet to validate
//     against; a follow-up PR with its own fixtures registers them.
//
//   - RangeWindowGridNativeVectorAgg — the ForEach-combinator vector-aggregation
//     narrowing of RangeWindowGridNative (cerberus issue #2763). Its own GROUP
//     BY runs directly over Input's per-series (grid, grid_ts) row — the exact
//     same row RangeWindowGridNative's entry above already argues is
//     scan-lower-bound-independent — and combines it with a `-ForEach`
//     aggregate that reads only the array ELEMENTS at each position, never a
//     neighbouring row or a scan-order-seeded state. Its final explode reads
//     grid_ts by RECOMPUTING the same pure timeSeriesRange(start, end, step)
//     call Input's own array level issues (a function of the query's fixed
//     (Start, End, Step), not of any row), so it carries no additional
//     scan-order dependence beyond Input's own. Slicing an anchor sub-grid
//     therefore changes which (series, anchor) values feed the ForEach combine
//     — exactly as it changes which rows reach RangeWindowGridNative's own
//     explode — never what any surviving combine computes. Verified by
//     dual-emit parity (Aggregate-over-exploded-rows vs
//     RangeWindowGridNativeVectorAgg) in
//     internal/chsql/range_window_grid_native_vector_agg_chdb_test.go.
//
// Extension point. Phase-3 node families (TopK as per-anchor LIMIT K BY,
// VectorSetOp, HistogramQuantileNative, HistogramProjection, AbsentOverTime,
// RangeWindowStaleResample, the metrics_* TraceQL family, nested spines
// under the lcm clamp) are DELIBERATELY ABSENT:
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
		&RangeWindowGridNative{},
		&RangeWindowGridNativeVectorAgg{},
		&RangeLWR{},
		&RangeBucketFanout{},
		&RangeBucketGridNative{},
		&HistogramQuantile{},
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

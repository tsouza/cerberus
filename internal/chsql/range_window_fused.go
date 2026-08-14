package chsql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file implements the memory-bounded FUSED emitter for PromQL
// subqueries of the shape `<reducer>(rate|increase|delta(m[range])[outer:step])`
// where <reducer> is an order-independent *_over_time aggregate
// (min/max/count), in BOTH outer shapes: the INSTANT reducer (`/api/v1/query`,
// OuterRange == 0 && Step == 0) and the MATRIX reducer (`/api/v1/query_range`,
// OuterRange > 0 && Step > 0).
//
// The materialized path (emitWindowedArrayExtrapolatedMatrix +
// emitRangeWindowOverTimeDirect) builds a 5-layer stack whose
// `GROUP BY (Attributes, anchor_ts)` regroup materialises numAnchors×series
// groups, then the outer reducer regroups again by series. At high cardinality
// / long subquery range that intermediate fanout OOMs (the instant analogue of
// the run-27277793810 rate(...) matrix OOM). The range case is strictly worse:
// the inner grid is widened to the WHOLE request window plus one subquery
// range, so `query_range` over a subquery materialises an inner matrix orders
// of magnitude larger than the outer answer (#1505).
//
// The fused shape collapses both regroups into a SINGLE `GROUP BY Attributes`
// that builds ONE sorted per-series (ts, value) array, then maps the anchor
// grid → per-anchor extrapolated value → `arrayReduce('<agg>', …)`. Peak
// memory is O(samples-in-window) per series, independent of anchor count.
//
// Result-equivalence (NOT byte-equivalence) to the materialized path is the
// contract: the per-anchor window slice is computed with the SAME half-open
// `(a-range, a]` membership + dedup-by-ts the materialized regroup applies, the
// SAME scan-time bound (maybePushInnerScanTimeBounds) limits which samples
// enter the per-series array, and the SAME outer anchor-window filter
// (endOuter-outerRange, endOuter] the existing direct path applies. The
// per-anchor extrapolation arithmetic drives the SAME shared helpers the
// materialized path does (extrapolatedValueExpr / extrapThresholdClampExpr /
// secondsBetweenFrag in range_window.go), passing inline slice-derived operands
// instead of mid-layer aliases — one source of truth, no clone to drift.
// `arrayReduce('max',…)` invokes the identical CH aggregate the materialized
// `max(Value)` does, so NaN/empty semantics match by construction.

// fusedExtrapolationMinSamples is the window size an extrapolating inner
// (rate/increase/delta) needs before it produces a value: Prometheus's
// extrapolatedRate consults the first and last in-window samples and the
// pairwise counter-reset deltas between them, so a 0- or 1-sample window
// yields no point. The materialized path spells the same gate as its
// `WHERE length(window_vals) >= 2`.
const fusedExtrapolationMinSamples int64 = 2

// fusedAnchorGridAlias names the per-series column holding ONE
// `(anchor, qualifies, value)` triple per inner subquery anchor — the fused
// matrix shape's replacement for the materialized inner matrix's
// numAnchors×series rows.
const fusedAnchorGridAlias = "anchor_grid"

// fusedOuterAnchorAlias names the arrayJoin'd `(anchor, value)` pair that
// turns the per-series row back into one row per (series, outer anchor).
const fusedOuterAnchorAlias = "outer_anchor"

// fusedReduce maps the per-(qualifying-anchor) value array to the final
// per-series scalar Value frag, replicating what the materialized outer
// reducer (emitRangeWindowOverTimeDirect's direct CH aggregate) produces.
type fusedReduce func(perAnchorVals Frag) Frag

// fusedOuterReducer maps an outer *_over_time reducer to its fused array
// reducer, or reports ok=false for reducers that must stay on the
// materialized path. Only the order-independent reducers that route through
// emitRangeWindowOverTimeDirect are fusible here; sum/avg/quantile/stddev/…
// reach the array path (emitWindowedArray) and never hit this dispatch.
//
// present_over_time is deliberately absent: it is NOT a member of
// rangeVectorFn (internal/promql/subquery.go), the set that gates whether a
// PromQL function accepts a subquery argument at all, so no valid query can
// build the nested-RangeWindow shape tryEmitFusedSubquery dispatches
// on for it. Adding a case here without also widening rangeVectorFn would be
// dead code with zero fixture or chDB coverage backing its correctness (#1706).
func fusedOuterReducer(fn string) (fusedReduce, bool) {
	switch fn {
	case "max_over_time":
		return func(vals Frag) Frag { return Call("arrayReduce", InlineLit("max"), vals) }, true
	case "min_over_time":
		return func(vals Frag) Frag { return Call("arrayReduce", InlineLit("min"), vals) }, true
	case "count_over_time":
		// Counts the qualifying-anchor rows the materialized direct path
		// would `toFloat64(count())` over. arrayReduce('count', vals) counts
		// every element (incl. NaN), matching count()'s row count.
		return func(vals Frag) Frag {
			return Call("toFloat64", Call("arrayReduce", InlineLit("count"), vals))
		}, true
	}
	return nil, false
}

// extrapolatingKindForFunc maps an inner range function to its
// extrapolationKind, reporting ok=false for non-extrapolating inners (the
// pairwise irate/idelta forms, *_over_time, etc.) that must not be fused.
func extrapolatingKindForFunc(fn string) (extrapolationKind, bool) {
	switch fn {
	case "rate":
		return extrapolationKindRate, true
	case "increase":
		return extrapolationKindIncrease, true
	case "delta":
		return extrapolationKindDelta, true
	}
	return 0, false
}

// tryEmitFusedSubquery attempts the fused emit for an outer *_over_time
// reducer over an extrapolating inner matrix RangeWindow. It returns
// handled=true when it emitted (or failed to emit) the fused shape, and
// handled=false when the shape is not fusible and the caller must fall through
// to the existing materialized path unchanged.
func (e *emitter) tryEmitFusedSubquery(r *chplan.RangeWindow) (handled bool, err error) {
	// The outer reducer is either INSTANT (a single anchor at End, no grid) or
	// MATRIX (one anchor per request step across [End-OuterRange, End]). A
	// half-set pair is neither — OuterRange>0 with no Step has no grid spacing
	// and Step>0 with no OuterRange has no span — so the fused entry declines
	// rather than inventing a grid the materialized path would not produce.
	instantOuter := r.OuterRange == 0 && r.Step == 0
	matrixOuter := r.OuterRange > 0 && r.Step > 0
	if !instantOuter && !matrixOuter {
		return false, nil
	}
	inner, ok := r.Input.(*chplan.RangeWindow)
	if !ok {
		return false, nil
	}
	// Inner must be an extrapolating MATRIX RangeWindow (the subquery inner
	// sample grid: OuterRange = subquery range, Step = subquery resolution).
	if inner.Identity || inner.OuterRange <= 0 || inner.Step <= 0 {
		return false, nil
	}
	kind, ok := extrapolatingKindForFunc(inner.Func)
	if !ok {
		return false, nil
	}
	reduce, ok := fusedOuterReducer(r.Func)
	if !ok {
		return false, nil
	}
	if inner.TimestampColumn == "" || inner.ValueColumn == "" {
		return false, nil
	}
	// The fused per-series samples array is bounded by the SAME scan-time
	// pushdown the materialized matrix path uses (maybePushInnerScanTimeBounds),
	// which is gated on inner.Start/End being set. Without that bound the fused
	// shape would groupArray the full per-series retention — fall through to
	// the materialized path (identically gated) rather than introduce an
	// unbounded scan here.
	if inner.Start.IsZero() || inner.End.IsZero() {
		return false, nil
	}
	g, err := e.newFusedSubqueryGrid(r, inner, kind)
	if err != nil {
		return true, err
	}
	if matrixOuter {
		return true, e.emitFusedMatrixSubquery(r, g, reduce)
	}
	return true, e.emitFusedInstantSubquery(r, g, reduce)
}

// fusedSubqueryGrid holds the inner-subquery sample-grid quantities both fused
// emitters share — the same derivation emitWindowedArrayExtrapolatedMatrix
// applies to the inner RangeWindow, resolved once so the instant and matrix
// shapes cannot drift apart.
type fusedSubqueryGrid struct {
	inner      *chplan.RangeWindow
	kind       extrapolationKind
	groupFrags []Frag
	innerSub   Frag
	endInner   Frag
	// temporality references the per-series AggregationTemporality read
	// samplesQuery projects (windowTemporalityAlias), or is nil when the
	// inner window carries no such column — the DELTA-vs-CUMULATIVE branch
	// every per-anchor counter_delta routes through. See
	// CounterOrDeltaSum and issue #1963.
	temporality  Frag
	stepNS       int64
	rangeNS      int64
	rangeSeconds float64
	numAnchors   int64
}

// newFusedSubqueryGrid resolves the shared grid quantities. The GroupBy frags
// are collected BEFORE the inner relation is rendered so the emitted query
// arguments keep the materialized path's ordering.
func (e *emitter) newFusedSubqueryGrid(
	r, inner *chplan.RangeWindow, kind extrapolationKind,
) (*fusedSubqueryGrid, error) {
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return nil, err
	}
	innerSub, err := e.subqueryFrag(inner.Input)
	if err != nil {
		return nil, err
	}
	endInner := endExprFrag(inner)
	stepNS := inner.Step.Nanoseconds()
	numAnchors := inner.OuterRange.Nanoseconds()/stepNS + 1
	endInner, numAnchors = stepAlignGrid(inner, endInner, stepNS, numAnchors)
	return &fusedSubqueryGrid{
		inner:        inner,
		kind:         kind,
		groupFrags:   groupFrags,
		innerSub:     innerSub,
		endInner:     endInner,
		temporality:  windowTemporalityRef(inner),
		stepNS:       stepNS,
		rangeNS:      inner.Range.Nanoseconds(),
		rangeSeconds: inner.Range.Seconds(),
		numAnchors:   numAnchors,
	}, nil
}

// samplesQuery renders the per-series sorted (ts, value) samples array — the
// fused shape's single materialisation. The scan-time bound mirrors the
// materialized matrix fanout's pushdown so the array holds exactly the sample
// universe that path's per-(series, anchor) regroup saw; the post-groupArray
// arrayFilter stays the precise window gate.
//
// When the inner window carries a TemporalityColumn, the series' single
// AggregationTemporality reading rides along under windowTemporalityAlias —
// this ONE group is the fused shape's only GROUP BY, so it is also the only
// place the any() read can happen, and every per-anchor counter_delta above
// reads it back from here (see fusedExtrapolatedValueFrag).
func (g *fusedSubqueryGrid) samplesQuery() *QueryBuilder {
	q := NewQuery().From(g.innerSub)
	q.Select(g.groupFrags...)
	q.Select(As(groupArrayPairFrag(g.inner.TimestampColumn, g.inner.ValueColumn), "samples"))
	if g.temporality != nil {
		q.Select(As(Call("any", Col(g.inner.TemporalityColumn)), windowTemporalityAlias))
	}
	maybePushInnerScanTimeBounds(q, g.inner, g.inner.TimestampColumn, g.rangeNS)
	q.GroupBy(g.groupFrags...)
	return q
}

// anchorsFrag renders the inner subquery sample grid, walking back from the
// step-aligned inner end by i*step — byte-identical to the materialized
// fanout's per-i anchor base.
func (g *fusedSubqueryGrid) anchorsFrag() Frag {
	return Call(
		"arrayMap",
		Lambda1("i", anchorBaseAtIdxFrag(g.endInner, g.stepNS)),
		Call("range", InlineLit(g.numAnchors)),
	)
}

// sliceOf renders the dedup-by-ts of the half-open (a-range, a] window over the
// per-series samples array — element-for-element identical to the materialized
// window_pairs(a) (same membership, same arraySort order, same dedup keeping
// last-of-equal-ts run).
func (g *fusedSubqueryGrid) sliceOf(a Frag) Frag {
	win := Call(
		"arrayFilter",
		Lambda1("p", And(
			Gt(tupleElemFrag(BareIdent("p"), 1), rangeStartFrag(a, g.rangeNS)),
			Lte(tupleElemFrag(BareIdent("p"), 1), a),
		)),
		BareIdent("samples"),
	)
	return dedupWindowPairsByTsFrag(win)
}

// mapAnchors renders `arrayMap(a -> <body(a, slice(a))>, <anchors>)` with the
// anchor's window slice bound ONCE per anchor (see letBindFrag): every
// reference to the slice inside `body` (~50 in the extrapolation arithmetic)
// would otherwise re-render the O(samples) arrayFilter+dedup, a per-anchor
// recomputation that is quadratic for a dense grid (the #1109 GAP-2 review's
// CPU-blowup finding). The slice itself never escapes into a carried array, so
// peak memory stays O(samples-in-window) per series — the whole point of the
// fused path.
func (g *fusedSubqueryGrid) mapAnchors(body func(a, s Frag) Frag) Frag {
	return Call(
		"arrayMap",
		Lambda1("a", letBindFrag("s", g.sliceOf(BareIdent("a")), func(s Frag) Frag {
			return body(BareIdent("a"), s)
		})),
		g.anchorsFrag(),
	)
}

// qualifiesFrag renders the per-anchor "this anchor produces a point" gate,
// replacing the materialized inner's `WHERE length(window_vals) >= 2`.
func qualifiesFrag(s Frag) Frag {
	return Gte(Call("length", s), InlineLit(fusedExtrapolationMinSamples))
}

// letBindFrag binds `value` to the lambda parameter `name`, evaluated ONCE:
// ClickHouse has no LET, so `array(<value>)` materialises it a single time and
// the wrapping `arrayMap(<name> -> body, …)[1]` binds it.
func letBindFrag(name string, value Frag, body func(bound Frag) Frag) Frag {
	return Subscript(
		Call("arrayMap", Lambda1(name, body(BareIdent(name))), Array(value)),
		InlineLit(int64(1)),
	)
}

// emitFusedInstantSubquery renders the three-layer fused shape for an INSTANT
// outer reducer:
//
//	SELECT <series>, arrayReduce('<agg>', <per-anchor extrapolated values>) AS Value
//	FROM (
//	  SELECT <series>, samples,
//	    arrayFilter(a -> <outer-window> AND length(<slice(a)>) >= 2, <anchors>) AS qualified_anchors
//	  FROM (
//	    SELECT <series>, arraySort(groupArray((ts, val))) AS samples
//	    FROM (<inner.Input>)
//	    WHERE <scan bounds>
//	    GROUP BY <series>
//	  )
//	)
//	WHERE length(qualified_anchors) > 0
//
// where <slice(a)> = dedup-by-ts(arrayFilter(p -> p.ts > a-range AND p.ts <= a, samples)).
func (e *emitter) emitFusedInstantSubquery(
	r *chplan.RangeWindow, g *fusedSubqueryGrid, reduce fusedReduce,
) error {
	// Outer instant reducer anchor-window: the existing direct path filters
	// anchor_ts to (endOuter - outerRange, endOuter] before reducing.
	endOuter := endExprFrag(r)
	outerRangeNS := r.Range.Nanoseconds()

	// outerWindowPred(a): the existing direct path's anchor-window filter,
	// (endOuter - outerRange, endOuter].
	outerWindowPred := func(a Frag) Frag {
		return And(
			Gt(a, rangeStartFrag(endOuter, outerRangeNS)),
			Lte(a, endOuter),
		)
	}

	// Layer 1 — per-series sorted (ts, value) samples array.
	samplesQ := g.samplesQuery()

	// Per-anchor (qualifies, extrapolated_value) over the full grid, slice bound
	// once. qualifies = outer anchor-window ∧ length(slice)>=2. The value is
	// computed for every anchor (cheap scalars) and the non-qualifying ones are
	// dropped next — CH array-index OOB on a short/empty slice yields 0, never
	// an error.
	perAnchor := g.mapAnchors(func(a, s Frag) Frag {
		return Tuple(
			And(outerWindowPred(a), qualifiesFrag(s)),
			e.fusedExtrapolatedValueFrag(s, a, g.kind, g.rangeNS, g.rangeSeconds, g.temporality),
		)
	})

	// Layer 2 — the qualifying anchors' values, materialised once as `vals`
	// (O(numAnchors) scalars, not slices).
	valsQ := NewQuery().From(samplesQ.Frag())
	valsQ.Select(g.groupFrags...)
	valsQ.Select(As(
		Call("arrayMap",
			Lambda1("t", tupleElemFrag(BareIdent("t"), 2)),
			Call("arrayFilter", Lambda1("t", tupleElemFrag(BareIdent("t"), 1)), perAnchor)),
		"vals",
	))

	// Layer 3 — reduce by the outer aggregate. arrayReduce('<agg>', …) invokes
	// the same CH aggregate the materialized outer GROUP BY would, so NaN/empty
	// semantics match by construction. A series with zero qualifying anchors
	// emits no row — matching the materialized path producing no group for it.
	outerQ := NewQuery().From(valsQ.Frag())
	outerQ.Select(g.groupFrags...)
	outerQ.Select(As(reduce(BareIdent("vals")), r.ValueColumn))
	outerQ.Where(Gt(Call("length", BareIdent("vals")), InlineLit(int64(0))))

	return e.emitSelect(outerQ)
}

// emitFusedMatrixSubquery renders the four-layer fused shape for a MATRIX outer
// reducer (`/api/v1/query_range`):
//
//	SELECT <series>, tupleElement(outer_anchor, 1) AS anchor_ts,
//	       [<grid anchor> AS <ts>,] tupleElement(outer_anchor, 2) AS Value
//	FROM (
//	  SELECT <series>,
//	    arrayJoin(arrayFilter(p -> tupleElement(p, 3) > 0,
//	      arrayMap(o -> (o, <agg over covered values>, <covered count>),
//	               <outer anchors>))) AS outer_anchor
//	  FROM (
//	    SELECT <series>, arrayMap(a -> (a, <qualifies>, <value>), <inner anchors>) AS anchor_grid
//	    FROM (<per-series samples>)
//	  )
//	)
//
// The inner subquery grid is built ONCE per series as `anchor_grid` — the
// materialized path's numAnchors×series intermediate rows collapse into one
// array of scalars per series — and each outer anchor then reduces the
// contiguous run of inner anchors its window covers (see coveredValuesFrag).
// Row-for-row equivalent to emitRangeWindowOverTimeDirectMatrix over
// emitWindowedArrayExtrapolatedMatrix, without materialising the inner matrix.
func (e *emitter) emitFusedMatrixSubquery(
	r *chplan.RangeWindow, g *fusedSubqueryGrid, reduce fusedReduce,
) error {
	endOuter := endExprFrag(r)
	outerRangeNS := r.Range.Nanoseconds()
	outerStepNS := r.Step.Nanoseconds()
	// The outer grid is the request's own step grid: anchors walk back from
	// `End - Offset` by i*Step, end-inclusive. Deliberately NOT step-aligned —
	// epoch alignment is the INNER subquery sample grid's rule (PromQL's
	// evalSubquery), while the outer reducer reports on the user's
	// start + k*step grid, exactly as emitRangeWindowOverTimeDirectMatrix does.
	numOuterAnchors := r.OuterRange.Nanoseconds()/outerStepNS + 1

	// Layer 1 — per-series sorted (ts, value) samples array.
	samplesQ := g.samplesQuery()

	// Layer 2 — the inner subquery sample grid: one (anchor, qualifies, value)
	// triple per inner anchor, materialised ONCE per series. Keeping the
	// non-qualifying anchors in place is what makes the grid index-addressable
	// by coveredValuesFrag's arithmetic slice.
	gridQ := NewQuery().From(samplesQ.Frag())
	gridQ.Select(g.groupFrags...)
	gridQ.Select(As(
		g.mapAnchors(func(a, s Frag) Frag {
			return Tuple(
				a,
				qualifiesFrag(s),
				e.fusedExtrapolatedValueFrag(s, a, g.kind, g.rangeNS, g.rangeSeconds, g.temporality),
			)
		}),
		fusedAnchorGridAlias,
	))

	// Layer 3 — one row per (series, outer anchor) whose window covers at least
	// one qualifying inner anchor, carrying the reduced value. The reduction
	// happens INSIDE the array (before the arrayJoin) so the carried payload
	// stays O(numOuterAnchors) scalars rather than O(numOuterAnchors × covered)
	// values.
	outerAnchors := Call(
		"arrayMap",
		Lambda1("i", anchorBaseAtIdxFrag(endOuter, outerStepNS)),
		Call("range", InlineLit(numOuterAnchors)),
	)
	perOuter := Call(
		"arrayMap",
		Lambda1("o", letBindFrag("q", g.coveredValuesFrag(BareIdent("o"), outerRangeNS), func(q Frag) Frag {
			return Tuple(BareIdent("o"), reduce(q), Call("length", q))
		})),
		outerAnchors,
	)
	joinQ := NewQuery().From(gridQ.Frag())
	joinQ.Select(g.groupFrags...)
	joinQ.Select(As(
		Call("arrayJoin", Call(
			"arrayFilter",
			Lambda1("p", Gt(tupleElemFrag(BareIdent("p"), 3), InlineLit(int64(0)))),
			perOuter,
		)),
		fusedOuterAnchorAlias,
	))

	// Layer 4 — the matrix column shape emitRangeWindowOverTimeDirectMatrix
	// produces: the offset-SHIFTED anchor under `anchor_ts`, the un-shifted
	// request-grid timestamp under the schema timestamp column (so a wrapping
	// Aggregate's per-step GROUP BY resolves), and the reduced Value.
	outQ := NewQuery().From(joinQ.Frag())
	outQ.Select(g.groupFrags...)
	outQ.Select(As(tupleElemFrag(BareIdent(fusedOuterAnchorAlias), 1), RangeWindowAnchorAlias))
	if r.TimestampColumn != "" && r.TimestampColumn != RangeWindowAnchorAlias {
		outQ.Select(As(gridAnchorFrag(r), r.TimestampColumn))
	} else if r.TimestampColumn == RangeWindowAnchorAlias {
		// The outer reducer consumes anchor_ts, while the root matrix answer
		// must also expose the request-grid timestamp to downstream consumers.
		outQ.Select(As(gridAnchorFrag(r), "TimeUnix"))
	}
	outQ.Select(As(tupleElemFrag(BareIdent(fusedOuterAnchorAlias), 2), r.ValueColumn))

	return e.emitSelect(outQ)
}

// coveredValuesFrag renders the values of the qualifying inner anchors that
// fall inside outer anchor `o`'s half-open window `(o - outerRange, o]`.
//
// The inner anchors sit on the arithmetic grid `a_i = B - i·step` (B = the
// step-aligned inner base, i ascending ⇒ a_i descending), so the covered set is
// a CONTIGUOUS index run and an arraySlice answers it in O(covered) rather than
// rescanning all numAnchors entries per outer anchor. With
// `d = dateDiff('nanosecond', o, B) = B - o`:
//
//	a_i <= o                ⇔  i·step >= d        ⇔  i >= ceil(d / step)
//	a_i >  o - outerRange   ⇔  i·step <  d + R    ⇔  i <  ceil((d + R) / step)
//
// — the index-arithmetic dual of sampleAnchorFanoutFrag's sample-side bounds
// (there the window belongs to the anchor grid; here the anchor grid IS the
// membership candidate set), spelled through the same floor-division helper via
// anchorGridCeilIdxFrag. Both bounds are clamped into [0, numAnchors] by the
// same monotone greatest/least pair, so an outer anchor whose window falls off
// the grid degenerates to a zero-length slice rather than a negative one.
func (g *fusedSubqueryGrid) coveredValuesFrag(o Frag, outerRangeNS int64) Frag {
	dist := distBehindAnchorFrag(o, g.endInner)
	lo := Call("greatest", InlineLit(int64(0)), anchorGridCeilIdxFrag(dist, 0, g.stepNS))
	hi := Call("least", InlineLit(g.numAnchors), anchorGridCeilIdxFrag(dist, outerRangeNS, g.stepNS))
	covered := Call(
		"arraySlice",
		BareIdent(fusedAnchorGridAlias),
		Add(lo, InlineLit(int64(1))), // arraySlice offsets are 1-based
		Call("greatest", InlineLit(int64(0)), Sub(hi, lo)),
	)
	return Call(
		"arrayMap",
		Lambda1("c", tupleElemFrag(BareIdent("c"), 3)),
		Call("arrayFilter", Lambda1("c", tupleElemFrag(BareIdent("c"), 2)), covered),
	)
}

// tupleElemFrag renders `tupleElement(<t>, <idx>)` — the 1-based tuple
// accessor used to pull ts (idx 1) / value (idx 2) out of a (ts, value) pair.
func tupleElemFrag(t Frag, idx int64) Frag {
	return Call("tupleElement", t, InlineLit(idx))
}

// fusedExtrapolatedValueFrag computes the per-anchor extrapolated Value over the
// dedup'd slice `w` and anchor `a` by feeding inline, slice-derived operands to
// the SHARED extrapolation arithmetic (extrapThresholdClampExpr +
// extrapolatedValueExpr in range_window.go) — the same helpers the materialized
// path drives with its mid-layer column aliases. Only the operands differ
// (inline exprs here, aliases there); the arithmetic shape is single-sourced, so
// the two paths cannot drift. The inline operands are Paren-wrapped where the
// materialized aliases are bare single tokens, so `… / sampled_interval` doesn't
// re-associate a trailing `/ 1e9` once inlined.
//
// temporality is the per-series AggregationTemporality reference
// samplesQuery projected (nil when the inner window carries no such
// column), and routes the raw window total through the SAME
// CounterOrDeltaSum branch the materialized path applies — without it a
// subquery over a DELTA-temporality counter read every anchor's window
// through Prometheus's counter-reset rule (issue #1963, item 2).
func (e *emitter) fusedExtrapolatedValueFrag(
	w, a Frag, kind extrapolationKind, rangeNS int64, rangeSeconds float64, temporality Frag,
) Frag {
	lenW := Call("length", w)
	firstTs := tupleElemFrag(Subscript(w, InlineLit(int64(1))), 1)
	lastTs := tupleElemFrag(Subscript(w, lenW), 1)
	firstVal := tupleElemFrag(Subscript(w, InlineLit(int64(1))), 2)
	lastVal := tupleElemFrag(Subscript(w, lenW), 2)
	counterDelta := CounterOrDeltaSum(w, temporality)
	if temporality != nil {
		// Prometheus sees the DELTA seed as a running total. Its first value
		// is therefore the prefix through this window's first observation.
		firstVal = If(
			Eq(temporality, InlineLit(schema.AggregationTemporalityDelta)),
			Call("arraySum", Call("arrayMap", Lambda1("p", tupleElemFrag(BareIdent("p"), 2)), Call(
				"arrayFilter", Lambda1("p", Lte(tupleElemFrag(BareIdent("p"), 1), firstTs)), BareIdent("samples"),
			))),
			firstVal,
		)
	}

	// sampled_interval and the duration-to-edge raws share secondsBetweenFrag
	// with the materialized path (Paren-wrapped here because, unlike the
	// materialized column aliases, the inlined form must not let a trailing
	// `/ 1e9` re-associate when this divides a larger expression).
	sampledInterval := Paren(secondsBetweenFrag(firstTs, lastTs))
	// numSamplesMinusOne = (length(w) - 1); the length>=2 qualifying gate keeps
	// it non-zero.
	nm1 := numSamplesMinusOneFrag(w)

	durToStartRaw := Paren(secondsBetweenFrag(rangeStartFrag(a, rangeNS), firstTs))
	durToEndRaw := Paren(secondsBetweenFrag(lastTs, a))
	durToStart := extrapThresholdClampExpr(durToStartRaw, sampledInterval, nm1)
	durToEnd := extrapThresholdClampExpr(durToEndRaw, sampledInterval, nm1)

	return extrapolatedValueExpr(kind, rangeSeconds,
		counterDelta, sampledInterval, firstVal, lastVal, durToStart, durToEnd)
}

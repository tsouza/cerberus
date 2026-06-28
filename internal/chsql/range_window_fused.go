package chsql

import "github.com/tsouza/cerberus/internal/chplan"

// This file implements the memory-bounded FUSED emitter for instant PromQL
// subqueries of the shape `<reducer>(rate|increase|delta(m[range])[outer:step])`
// where <reducer> is an order-independent *_over_time aggregate
// (min/max/count/present).
//
// The materialized path (emitWindowedArrayExtrapolatedMatrix +
// emitRangeWindowOverTimeDirect) builds a 5-layer stack whose
// `GROUP BY (Attributes, anchor_ts)` regroup materialises numAnchors×series
// groups, then the outer reducer regroups again by series. At high cardinality
// / long subquery range that intermediate fanout OOMs (the instant analogue of
// the run-27277793810 rate(...) matrix OOM).
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

// fusedReduce maps the per-(qualifying-anchor) value array to the final
// per-series scalar Value frag, replicating what the materialized outer
// reducer (emitRangeWindowOverTimeDirect's direct CH aggregate) produces.
type fusedReduce func(perAnchorVals Frag) Frag

// fusedOuterReducer maps an instant outer *_over_time reducer to its fused
// array reducer, or reports ok=false for reducers that must stay on the
// materialized path. Only the order-independent reducers that route through
// emitRangeWindowOverTimeDirect are fusible here; sum/avg/quantile/stddev/…
// reach the array path (emitWindowedArray) and never hit this dispatch.
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
	case "present_over_time":
		// The outer WHERE length(qualified_anchors) > 0 guarantees at least
		// one qualifying anchor, so present is the constant 1 — matching the
		// materialized direct path's toFloat64(1). vals is intentionally
		// unused (and so never rendered).
		return func(Frag) Frag { return Call("toFloat64", InlineLit(int64(1))) }, true
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

// tryEmitFusedInstantSubquery attempts the fused emit for an instant outer
// reducer (OuterRange == 0 && Step == 0) over an extrapolating inner matrix
// RangeWindow. It returns handled=true when it emitted (or failed to emit) the
// fused shape, and handled=false when the shape is not fusible and the caller
// must fall through to the existing materialized path unchanged.
func (e *emitter) tryEmitFusedInstantSubquery(r *chplan.RangeWindow) (handled bool, err error) {
	// Instant outer reducer only: OuterRange>0 is the range-query/matrix shape
	// (handled before this call) and would make the fused single-anchor collapse
	// wrong. The caller already routes OuterRange>0 away, but assert it here so
	// the fused entry is self-guarding rather than relying on a caller invariant.
	if r.OuterRange != 0 || r.Step != 0 {
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
	return true, e.emitFusedInstantSubquery(r, inner, kind, reduce)
}

// emitFusedInstantSubquery renders the three-layer fused shape:
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
	r, inner *chplan.RangeWindow, kind extrapolationKind, reduce fusedReduce,
) error {
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	// Inner subquery grid quantities (same derivation as
	// emitWindowedArrayExtrapolatedMatrix).
	endInner := endExprFrag(inner)
	rangeNS := inner.Range.Nanoseconds()
	stepNS := inner.Step.Nanoseconds()
	rangeSeconds := inner.Range.Seconds()
	numAnchors := inner.OuterRange.Nanoseconds()/stepNS + 1
	endInner, numAnchors = stepAlignGrid(inner, endInner, stepNS, numAnchors)

	// Outer instant reducer anchor-window: the existing direct path filters
	// anchor_ts to (endOuter - outerRange, endOuter] before reducing.
	endOuter := endExprFrag(r)
	outerRangeNS := r.Range.Nanoseconds()

	innerSub, err := e.subqueryFrag(inner.Input)
	if err != nil {
		return err
	}

	// Layer 1 — per-series sorted (ts, value) samples array. The scan-time
	// bound mirrors the materialized matrix fanout's pushdown so the array
	// holds exactly the sample universe that path's per-(series, anchor)
	// regroup saw — the post-groupArray arrayFilter stays the precise window
	// gate.
	samplesQ := NewQuery().From(innerSub)
	for _, g := range groupFrags {
		samplesQ.Select(g)
	}
	samplesQ.Select(As(groupArrayPairFrag(inner.TimestampColumn, inner.ValueColumn), "samples"))
	maybePushInnerScanTimeBounds(samplesQ, inner, inner.TimestampColumn, rangeNS)
	samplesQ.GroupBy(groupFrags...)

	// Anchor grid (inner subquery sample grid), walking back from the
	// step-aligned inner end by i*step — byte-identical to the materialized
	// fanout's per-i anchor base.
	anchors := Call(
		"arrayMap",
		Lambda1("i", anchorBaseAtIdxFrag(endInner, stepNS)),
		Call("range", InlineLit(numAnchors)),
	)

	// slice(a): dedup-by-ts of the half-open (a-range, a] window over the
	// per-series samples array — element-for-element identical to the
	// materialized window_pairs(a) (same membership, same arraySort order,
	// same dedup keeping last-of-equal-ts run).
	sliceOf := func(a Frag) Frag {
		win := Call(
			"arrayFilter",
			Lambda1("p", And(
				Gt(tupleElemFrag(BareIdent("p"), 1), rangeStartFrag(a, rangeNS)),
				Lte(tupleElemFrag(BareIdent("p"), 1), a),
			)),
			BareIdent("samples"),
		)
		return dedupWindowPairsByTsFrag(win)
	}

	// outerWindowPred(a): the existing direct path's anchor-window filter,
	// (endOuter - outerRange, endOuter].
	outerWindowPred := func(a Frag) Frag {
		return And(
			Gt(a, rangeStartFrag(endOuter, outerRangeNS)),
			Lte(a, endOuter),
		)
	}

	// letSlice binds an anchor's window slice to `s`, evaluated ONCE: ClickHouse
	// has no LET, so `array(<slice>)` materialises the slice a single time and
	// the wrapping `arrayMap(s -> body, …)[1]` binds it. Without this, every
	// reference to the slice inside `body` (~50 in the extrapolation arithmetic)
	// re-renders the O(samples) arrayFilter+dedup — recomputed per anchor that is
	// quadratic for a dense grid (the #1109 GAP-2 review's CPU-blowup finding).
	// The slice itself never escapes into a carried array, so peak memory stays
	// O(samples-in-window) per series — the whole point of the fused path.
	letSlice := func(a Frag, body func(s Frag) Frag) Frag {
		return Subscript(
			Call("arrayMap", Lambda1("s", body(BareIdent("s"))), Array(sliceOf(a))),
			InlineLit(int64(1)),
		)
	}

	// Per-anchor (qualifies, extrapolated_value) over the full grid, slice bound
	// once. qualifies = outer anchor-window ∧ length(slice)>=2 (replaces the
	// materialized inner `WHERE length(window_vals) >= 2`). The value is computed
	// for every anchor (cheap scalars) and the non-qualifying ones are dropped
	// next — CH array-index OOB on a short/empty slice yields 0, never an error.
	perAnchor := Call(
		"arrayMap",
		Lambda1("a", letSlice(BareIdent("a"), func(s Frag) Frag {
			return Tuple(
				And(outerWindowPred(BareIdent("a")), Gte(Call("length", s), InlineLit(int64(2)))),
				e.fusedExtrapolatedValueFrag(s, BareIdent("a"), kind, rangeNS, rangeSeconds),
			)
		})),
		anchors,
	)

	// Layer 2 — the qualifying anchors' values, materialised once as `vals`
	// (O(numAnchors) scalars, not slices).
	valsQ := NewQuery().From(samplesQ.Frag())
	for _, g := range groupFrags {
		valsQ.Select(g)
	}
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
	for _, g := range groupFrags {
		outerQ.Select(g)
	}
	outerQ.Select(As(reduce(BareIdent("vals")), r.ValueColumn))
	outerQ.Where(Gt(Call("length", BareIdent("vals")), InlineLit(int64(0))))

	e.emitSelect(outerQ)
	return nil
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
func (e *emitter) fusedExtrapolatedValueFrag(
	w, a Frag, kind extrapolationKind, rangeNS int64, rangeSeconds float64,
) Frag {
	lenW := Call("length", w)
	firstTs := tupleElemFrag(Subscript(w, InlineLit(int64(1))), 1)
	lastTs := tupleElemFrag(Subscript(w, lenW), 1)
	firstVal := tupleElemFrag(Subscript(w, InlineLit(int64(1))), 2)
	lastVal := tupleElemFrag(Subscript(w, lenW), 2)
	counterDelta := Call("arraySum", CounterDelta(w))

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

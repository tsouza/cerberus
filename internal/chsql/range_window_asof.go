package chsql

import (
	"github.com/tsouza/cerberus/internal/chplan"
)

// rateAsofMinSamples is the minimum per-window sample count rate /
// increase / delta need before extrapolation runs — Prom's
// `len(samples.Floats) > 1` guard (functions.go:230). Below it the
// series is dropped for the anchor. The ASOF boundary path expresses
// the count as `last_rank - first_rank + 1` (the two boundary ranks),
// so the gate is a scalar comparison rather than `length(window_vals)`.
const rateAsofMinSamples = 2

// emitWindowedArrayExtrapolatedMatrixASOF is the fan-out-free matrix
// emitter for rate / increase / delta under query_range (OuterRange >
// 0). It replaces emitWindowedArrayExtrapolatedMatrix's per-(sample,
// covering-anchor) fan-out — which duplicates each sample across the
// ~range/step+1 anchors whose window contains it (O(rows ×
// anchors_per_window) intermediate rows) — with a per-series
// reset-adjusted cumulative + two ASOF boundary joins (O((samples +
// anchors)·log)).
//
// # Why the fan-out is avoidable
//
// Prom's extrapolatedRate (functions.go:188-314) reads only six
// per-window quantities off the windowed sample set:
//
//	first_ts, last_ts, first_val   — the two boundary samples,
//	counter_delta                  — reset-adjusted increase (rate/increase)
//	                                  or last_val - first_val (delta),
//	n  (= numSamplesMinusOne + 1)  — the in-window sample COUNT, feeding
//	                                  averageDurationBetweenSamples =
//	                                  sampledInterval / (n - 1).
//
// None of these needs the interior samples materialised per anchor:
//
//   - The window (anchor - range, anchor] over a per-series ts-sorted
//     stream is a CONTIGUOUS slice [first .. last]. So a per-series
//     reset-adjusted cumulative cumV (cumV[k] = Σ_{j≤k} if(v[j]<v[j-1],
//     v[j], v[j]-v[j-1]); the same `if(c<p,c,c-p)` reset repair the
//     fan-out's CounterDelta applies pairwise) gives the window's
//     counter_delta as cumV[last] - cumV[first] — the pre-window pair's
//     contribution cancels in the subtraction, leaving exactly
//     Σ_{j=first+1..last}, identical to the fan-out's arraySum.
//   - n is rank[last] - rank[first] + 1, where rank is the sample's
//     1-based position in the per-series sorted stream.
//   - first_ts/first_val/last_ts are read straight off the two boundary
//     samples.
//
// So each series is enriched ONCE with (ts, v, rank, cumV) (one row per
// sample — no anchor multiplication), and each anchor finds its two
// boundary samples via ASOF joins. The extrapolation arithmetic
// downstream is byte-for-byte the existing extrapolatedValueFrag /
// duration*Frag chain — only the SOURCE of first_ts/last_ts/first_val/
// counter_delta/n changes (boundary samples, not a regrouped array).
//
// # SQL skeleton (N = OuterRange/Step + 1)
//
//	SELECT <group>, anchor_ts [, anchor_ts AS <TimestampColumn>],
//	       <extrapolatedValueFrag> AS Value
//	FROM (
//	  SELECT <group>, anchor_ts, first_val, sampled_interval,
//	         <durationToStartFrag>, <durationToEndFrag>, counter_delta, window_vals
//	  FROM (
//	    SELECT g.<group>, g.anchor_ts,
//	           S.ts first_ts, E.ts last_ts, S.v first_val,
//	           (E.cumv - S.cumv) counter_delta,     -- rate/increase
//	           [S.v, E.v for delta's last_val - first_val]
//	           (E.rnk - S.rnk + 1) AS n
//	    FROM (<grid: distinct series × anchor grid>) g
//	    ASOF LEFT JOIN (<enriched stream>) E
//	         ON g.<group> = E.<group> AND E.ts <= g.anchor_ts
//	    ASOF LEFT JOIN (<enriched stream>) S
//	         ON g.<group> = S.<group> AND S.ts > g.anchor_ts - range
//	    WHERE E.ts <= anchor_ts AND S.ts <= anchor_ts
//	      AND S.ts > anchor_ts - range          -- boundary existence
//	  ) WHERE n >= 2
//	)
//
// The WHERE existence guards drop anchors whose window holds < 2
// samples (or none): a LEFT ASOF with no qualifying right row yields
// NULL ts, which fails `E.ts <= anchor_ts`; a window whose only sample
// sits past the anchor (S matched but E NULL, or vice versa) fails too.
// This reproduces the fan-out's `WHERE length(window_vals) >= 2` drop
// exactly — verified against the fan-out shape over random counters
// (incl. resets, irregular spacing, the extrapolation-threshold clamp,
// and the counter zero-crossing clamp) via the property/oracle lane.
//
// window_vals is projected (as a 2-element placeholder array sized to
// n) only so the shared extrapThresholdClampFrag's `length(window_vals)
// - 1` divisor reads the right count; the array's CONTENTS are never
// inspected by the extrapolation arithmetic (it reads first_val /
// counter_delta / sampled_interval / the boundary timestamps), so a
// length-only stand-in is exact.
func (e *emitter) emitWindowedArrayExtrapolatedMatrixASOF(r *chplan.RangeWindow, kind extrapolationKind) error {
	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	stepNS := r.Step.Nanoseconds()
	rangeSeconds := r.Range.Seconds()
	numAnchors := r.OuterRange.Nanoseconds()/stepNS + 1
	end, numAnchors = stepAlignGrid(r, end, stepNS, numAnchors)
	anchor := verbatim("anchor_ts")
	rangeStart := rangeStartFrag(anchor, rangeNS)

	// The ASOF path keys every layer (grid, enriched stream, equi-join)
	// on the series-identity columns by their bare NAMES — the wrapping
	// consumer (a `sum by (...)` Aggregate) references them by column
	// name (e.g. `Attributes`), so they must survive under that name,
	// not a synthetic alias. asofGroupColNames extracts those names;
	// the dispatcher only routes here when every GroupBy is a bare
	// ColumnRef, so the extraction never fails on this path.
	names, ok := asofGroupColNames(r.GroupBy)
	if !ok {
		return e.emitWindowedArrayExtrapolatedMatrix(r, kind)
	}

	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innerSub, srcTs := fanoutTsSource(innerSub, r.TimestampColumn)

	// The enriched per-series stream is referenced three times (the grid's
	// DISTINCT-series discovery, and both ASOF boundary joins E / S).
	// Materialise it ONCE as a `WITH enr AS (...)` CTE so it is scanned +
	// re-exploded a single time, not textually triplicated — the
	// triplication would re-derive the arrayCumSum / arrayJoin per copy
	// and triple the measured intermediate cardinality.
	enrBody := e.asofEnrichedStreamFrag(r, innerSub, srcTs, names, rangeNS)
	enrRef := verbatim(asofEnrCTE)
	grid := asofGridFrag(enrRef, names, end, stepNS, numAnchors)

	boundaries := asofBoundaryJoinFrag(grid, enrRef, names, kind, rangeNS)

	// Mid SELECT — the extrapolation scalars, sourced from the boundary
	// samples instead of a regrouped window array. Mirrors
	// emitWindowedArrayExtrapolatedMatrix's mid+extrap layers, but
	// first_ts / last_ts / first_val / counter_delta / window_vals are
	// already projected by the boundary join (window_vals is the
	// length-n placeholder), so this layer only derives the duration
	// quantities.
	extrap := NewQuery().From(boundaries)
	for _, name := range names {
		extrap.Select(Col(name))
	}
	extrap.Select(Col("anchor_ts"))
	extrap.Select(Col("window_vals"))
	extrap.Select(Col("counter_delta"))
	extrap.Select(Col("first_val"))
	extrap.Select(As(sampledIntervalFrag(), "sampled_interval"))
	extrap.Select(As(durationToStartFrag(rangeStart, kind.isCounter()), "duration_to_start"))
	extrap.Select(As(durationToEndFrag(anchor), "duration_to_end"))

	// Outer SELECT — per-(series, anchor) row, identical projection +
	// value expression to the fan-out matrix emitter. The enriched-stream
	// CTE is declared here (visible to every nested subquery: grid, E, S).
	outer := NewQuery().With(asofEnrCTE, Paren(enrBody)).From(extrap.Frag())
	for _, name := range names {
		outer.Select(Col(name))
	}
	outer.Select(Col("anchor_ts"))
	if r.TimestampColumn != "" && r.TimestampColumn != "anchor_ts" {
		outer.Select(As(verbatim("anchor_ts"), r.TimestampColumn))
	}
	outer.Select(As(extrapolatedValueFrag(kind, rangeSeconds), r.ValueColumn))

	e.emitSelect(outer)
	return nil
}

// asofEnrCTE is the WITH-clause name for the per-series enriched sample
// stream (ts, v, rank, cumV). Referenced by the grid's series discovery
// and both ASOF boundary joins so the stream is built once.
const asofEnrCTE = "enr"

// asofEnrichedStreamFrag renders the per-series enriched sample stream:
// one row per (series, sample) carrying ts, v, the 1-based per-series
// rank, and the reset-adjusted cumulative cumV — with NO anchor
// multiplication. Built by grouping each series into a sorted (ts, v)
// pair array once, deriving the parallel rank / cumV arrays over it,
// then re-exploding via a single arrayJoin over the zipped tuples.
//
//	SELECT <group-as-alias>, tupleElement(z,1) AS _ts, tupleElement(z,2) AS _v,
//	       tupleElement(z,3) AS _rnk, tupleElement(z,4) AS _cumv
//	FROM (
//	  SELECT <group-as-alias>, arrayJoin(arrayZip(
//	    arrayMap(p -> tupleElement(p,1), series_array),   -- ts
//	    arrayMap(p -> tupleElement(p,2), series_array),   -- v
//	    arrayEnumerate(series_array),                      -- rank (1-based)
//	    arrayCumSum(arrayConcat([head], CounterDelta(series_array)))  -- cumV
//	  )) AS z
//	  FROM (SELECT <group-as-alias>, arraySort(groupArray((ts,v))) AS series_array
//	        FROM (<input>) [WHERE scan-bounds] GROUP BY <group>)
//	)
//
// cumV's first element is the first sample's value (the reset repair's
// step(1): `if(v[1] < <none>, v[1], v[1])` degenerates to v[1]); the
// CounterDelta array supplies step(2..n) for the consecutive pairs. The
// arrayConcat([v[1]], CounterDelta) reproduces the fan-out's
// arraySum(CounterDelta(window)) telescoping exactly: cumV[last] -
// cumV[first] = Σ step(first+1 .. last), the in-window reset-adjusted
// increase.
func (e *emitter) asofEnrichedStreamFrag(
	r *chplan.RangeWindow,
	innerSub Frag,
	srcTs string,
	names []string,
	rangeNS int64,
) Frag {
	// Group each series into the sorted (ts, v) pair array. The series
	// columns flow through under their bare names (no re-alias) so the
	// grid / equi-join / final outer SELECT all reference them by the
	// name the wrapping consumer expects.
	base := NewQuery().From(innerSub)
	for _, name := range names {
		base.Select(Col(name))
	}
	base.Select(As(groupArrayPairFrag(srcTs, r.ValueColumn), "series_array"))
	maybePushInnerScanTimeBounds(base, r, srcTs, rangeNS)
	groupKeys := make([]Frag, 0, len(names))
	for _, name := range names {
		groupKeys = append(groupKeys, Col(name))
	}
	base.GroupBy(groupKeys...)

	// Derive the value array once; reused for v projection + cumV.
	valsArr := Call("arrayMap",
		Lambda1("p", Call("tupleElement", BareIdent("p"), InlineLit(int64(2)))),
		BareIdent("series_array"))
	// cumV = arrayCumSum([v[1], step(2), step(3), ...]) where step(k) is
	// the reset-repaired delta. The head term is v[1] (series_array[1].v).
	firstV := Call("tupleElement", Subscript(BareIdent("series_array"), InlineLit(int64(1))), InlineLit(int64(2)))
	cumv := Call("arrayCumSum",
		Call("arrayConcat", Array(firstV), CounterDelta(BareIdent("series_array"))))

	// tsArr is the parallel per-sample timestamp array.
	tsArr := Call("arrayMap",
		Lambda1("p", Call("tupleElement", BareIdent("p"), InlineLit(int64(1)))),
		BareIdent("series_array"))
	// multArr[i] = how many samples share series_array[i]'s exact
	// timestamp — `countEqual(tsArr, tsArr[i])` over the sorted array.
	// Needed only to reproduce the fan-out's all-same-timestamp NaN: when
	// CH's ASOF collapses both boundaries onto a single duplicate-ts row
	// (rank diff 0), the true in-window count is this multiplicity, so the
	// `sampled_interval == 0 -> nan` row still surfaces instead of being
	// dropped. For the all-distinct-ts common case every entry is 1.
	multArr := Call("arrayMap",
		Lambda1("t", Call("countEqual", tsArr, BareIdent("t"))),
		tsArr)

	z := Call("arrayJoin", Call(
		"arrayZip",
		tsArr,
		valsArr,
		Call("arrayEnumerate", BareIdent("series_array")),
		cumv,
		multArr,
	))

	exploded := NewQuery().From(base.Frag())
	for _, name := range names {
		exploded.Select(Col(name))
	}
	exploded.Select(As(z, "z"))

	out := NewQuery().From(exploded.Frag())
	for _, name := range names {
		out.Select(Col(name))
	}
	out.Select(As(Call("tupleElement", BareIdent("z"), InlineLit(int64(1))), asofTsCol))
	out.Select(As(Call("tupleElement", BareIdent("z"), InlineLit(int64(2))), asofValCol))
	out.Select(As(Call("tupleElement", BareIdent("z"), InlineLit(int64(3))), asofRankCol))
	out.Select(As(Call("tupleElement", BareIdent("z"), InlineLit(int64(4))), asofCumvCol))
	out.Select(As(Call("tupleElement", BareIdent("z"), InlineLit(int64(5))), asofMultCol))
	return out.Frag()
}

// Column aliases the enriched stream exposes (un-backticked synthetic
// tokens, like the windowed-array idiom's series_array / window_pairs).
const (
	asofTsCol   = "_ts"
	asofValCol  = "_v"
	asofRankCol = "_rnk"
	asofCumvCol = "_cumv"
	asofMultCol = "_mult"
)

// asofGridFrag renders the (series × anchor) grid relation: one row per
// (distinct series, grid anchor). The series set is the DISTINCT
// group-key tuple of the enriched stream, cross-joined against the
// anchor grid via the shared anchorFanoutFrag (arrayJoin over range(0,
// N)). With no group-by columns the grid degenerates to a single
// pseudo-series so a metric-name-only rate still emits its anchor grid.
//
//	SELECT <group-as-alias>, <anchorFanoutFrag> AS anchor_ts
//	FROM (SELECT DISTINCT <group-as-alias> FROM (<enriched>))
//
// The grid is O(series × N) rows — exactly the result cardinality (each
// series emits one row per anchor anyway), so it carries no fan-out
// blow-up; it is the irreducible output shape, not the avoided
// per-sample multiplication.
func asofGridFrag(enriched Frag, names []string, end Frag, stepNS, numAnchors int64) Frag {
	// Distinct series: GROUP BY the group-key columns (mirrors
	// metricsZeroFillGridArm's discovery shape). The dispatcher only
	// routes keyed rate here (CH ASOF needs an equi-key), so names is
	// always non-empty on this path.
	distinct := NewQuery().From(enriched)
	keys := make([]Frag, 0, len(names))
	for _, name := range names {
		distinct.Select(Col(name))
		keys = append(keys, Col(name))
	}
	distinct.GroupBy(keys...)

	grid := NewQuery().From(distinct.Frag())
	for _, name := range names {
		grid.Select(Col(name))
	}
	grid.Select(As(anchorFanoutFrag(end, stepNS, numAnchors), "anchor_ts"))
	return grid.Frag()
}

// asofBoundaryJoinFrag renders the two ASOF LEFT JOINs that resolve each
// anchor's window-end (E: closest sample with ts <= anchor_ts) and
// window-start (S: closest sample with ts > anchor_ts - range) boundary
// samples, then projects the per-(series, anchor) extrapolation inputs.
//
// The ON predicates carry the series equi-keys first and the single
// ASOF inequality last (CH requires the inequality as the final ON
// term). counter_delta is cumV[E] - cumV[S] for rate/increase
// (reset-adjusted) and last_val - first_val (= E._v - S._v) for delta's
// gauge difference. n = E._rnk - S._rnk + 1 is the in-window sample
// count; window_vals is a length-n placeholder array (range(1, n+1)) so
// the shared extrapThresholdClampFrag divisor `length(window_vals) - 1`
// equals numSamplesMinusOne without materialising the interior samples.
//
// The WHERE existence guards (E._ts <= anchor_ts AND S._ts <= anchor_ts
// AND S._ts > anchor_ts - range) drop anchors with no qualifying
// boundary (LEFT ASOF NULLs fail the comparison) — reproducing the
// fan-out's empty-window drop. The outer `n >= 2` gate then mirrors
// `WHERE length(window_vals) >= 2`.
func asofBoundaryJoinFrag(
	grid, enriched Frag,
	names []string,
	kind extrapolationKind,
	rangeNS int64,
) Frag {
	const (
		gridAlias  = "g"
		endAlias   = "E"
		startAlias = "S"
	)
	anchorG := Qual(gridAlias, "anchor_ts")
	// Window-left edge `g.anchor_ts - toIntervalNanosecond(range)`,
	// rebuilt per reference so both ASOF predicates render their own
	// (Frag closures are single-shot writers).
	windowLeft := func() Frag { return rangeStartFrag(Qual(gridAlias, "anchor_ts"), rangeNS) }

	onEnd := asofJoinOn(names, gridAlias, endAlias, Lte(Qual(endAlias, asofTsCol), anchorG))
	onStart := asofJoinOn(names, gridAlias, startAlias, Gt(Qual(startAlias, asofTsCol), windowLeft()))

	joined := NewQuery().From(aliasedFrag(grid, gridAlias)).
		Join(ASOFLeftJoin, aliasedFrag(enriched, endAlias), onEnd).
		Join(ASOFLeftJoin, aliasedFrag(enriched, startAlias), onStart)

	for _, name := range names {
		joined.Select(As(Qual(gridAlias, name), name))
	}
	joined.Select(As(anchorG, "anchor_ts"))
	joined.Select(As(Qual(startAlias, asofTsCol), "first_ts"))
	joined.Select(As(Qual(endAlias, asofTsCol), "last_ts"))
	joined.Select(As(Qual(startAlias, asofValCol), "first_val"))

	// counter_delta: reset-adjusted increase (rate/increase) or gauge
	// difference (delta).
	var counterDelta Frag
	if kind.isCounter() {
		counterDelta = Paren(Sub(Qual(endAlias, asofCumvCol), Qual(startAlias, asofCumvCol)))
	} else {
		counterDelta = Paren(Sub(Qual(endAlias, asofValCol), Qual(startAlias, asofValCol)))
	}
	joined.Select(As(counterDelta, "counter_delta"))

	// n = the in-window sample count. With distinct boundaries it is the
	// rank span `E._rnk - S._rnk + 1`. When CH's ASOF collapsed both
	// boundaries onto a single duplicate-timestamp row (rank span 0 — the
	// whole window sits at one timestamp), the true count is that
	// timestamp's multiplicity `E._mult`, so a 2+-sample all-same-ts
	// window keeps its row and surfaces the `sampled_interval == 0 -> nan`
	// value exactly as the fan-out emitter did (instead of being dropped).
	// Both branches are cast to Int64 so CH has a common supertype:
	// _rnk (arrayEnumerate -> UInt32) makes the span signed, while _mult
	// (countEqual -> UInt64) is unsigned, and `if` rejects the mixed pair.
	rankSpan := Sub(Qual(endAlias, asofRankCol), Qual(startAlias, asofRankCol))
	n := Call(
		"if",
		Eq(Qual(endAlias, asofRankCol), Qual(startAlias, asofRankCol)),
		Call("toInt64", Qual(endAlias, asofMultCol)),
		Call("toInt64", Add(Paren(rankSpan), InlineLit(int64(1)))),
	)
	// window_vals = arrayWithConstant(n, last_val) — a length-n array
	// whose LAST element is the window-end sample's value. The shared
	// extrapolatedValueFrag reads two things off window_vals:
	//   - length(window_vals) = n, for numSamplesMinusOne in the 1.1x
	//     extrapolation-threshold clamp (rate / increase / delta), and
	//   - window_vals[length(window_vals)] = last_val, the gauge raw
	//     result `last_val - first_val` for DELTA (functions.go:234).
	// Filling every slot with last_val satisfies both: the count is n and
	// the tail is last_val. rate / increase read counter_delta instead of
	// the tail, so the fill value is immaterial to them.
	joined.Select(As(Call("arrayWithConstant", n, Qual(endAlias, asofValCol)), "window_vals"))

	// Boundary existence: drop anchors whose window has no qualifying
	// end/start sample, or whose start sample sits past the anchor.
	joined.Where(Lte(Qual(endAlias, asofTsCol), Qual(gridAlias, "anchor_ts")))
	joined.Where(Lte(Qual(startAlias, asofTsCol), Qual(gridAlias, "anchor_ts")))
	joined.Where(Gt(Qual(startAlias, asofTsCol), windowLeft()))

	// Outer wrap applies the `length(window_vals) >= 2` (= n >= 2) gate.
	gated := NewQuery().From(joined.Frag())
	gated.Select(Star())
	gated.Where(windowLenAtLeastFrag("window_vals", rateAsofMinSamples))
	return gated.Frag()
}

// asofJoinOn builds an ASOF join ON predicate: the series equi-keys
// (`g.<alias> = <side>.<alias>` for each group column) AND'd with the
// single ASOF inequality, which MUST be the last conjunct (CH
// requirement). With no group columns the predicate is the inequality
// alone — but CH's ASOF needs at least one equi-key, so a keyless rate
// can't take the ASOF path; the dispatcher gates that case back to the
// fan-out emitter (see emitWindowedArrayExtrapolated).
func asofJoinOn(names []string, gridAlias, sideAlias string, inequality Frag) Frag {
	parts := make([]Frag, 0, len(names)+1)
	for _, name := range names {
		parts = append(parts, Eq(Qual(gridAlias, name), Qual(sideAlias, name)))
	}
	parts = append(parts, inequality)
	return And(parts...)
}

// asofGroupColNames extracts the bare column names of a RangeWindow's
// GroupBy when every entry is a plain ColumnRef (the series-identity
// shape — `[ColumnRef{Attributes}]` for OTel-CH rate). Returns ok=false
// for any non-ColumnRef entry (an aggregation expression), signalling
// the dispatcher to keep that shape on the fan-out emitter: the ASOF
// path threads the series key through every layer by NAME (so the
// wrapping `sum by (...)` Aggregate's column reference resolves) and a
// computed group expression has no stable name to thread.
func asofGroupColNames(group []chplan.Expr) ([]string, bool) {
	if len(group) == 0 {
		return nil, false
	}
	names := make([]string, 0, len(group))
	for _, g := range group {
		cr, ok := g.(*chplan.ColumnRef)
		if !ok || cr.Qualifier != "" {
			return nil, false
		}
		names = append(names, cr.Name)
	}
	return names, true
}

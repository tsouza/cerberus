package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Column aliases the native classic-histogram ladder emit threads between
// its levels. Every one is emitter-owned and synthetic — the plan carries
// only the anchor / BucketCounts / ExplicitBounds names — and each is a
// named constant so the producing level and the consuming level cannot
// drift apart into a ClickHouse `Unknown identifier` at query time.
const (
	// bucketGridRungAlias holds the unnested `(le, cumulative_count)` tuple
	// one arrayJoin element produced: one row per stored histogram row per
	// `le` rung.
	bucketGridRungAlias = "_hqn_rung"
	// bucketGridLeAlias / bucketGridCumAlias split that tuple into the rung's
	// bound and its cumulative observation count — the SCALAR counter series
	// reference Prometheus models `<name>_bucket{le="X"}` as.
	bucketGridLeAlias  = "_hqn_le"
	bucketGridCumAlias = "_hqn_cum"
	// bucketGridRateArrayAlias is the per-(series, rung) Array(Nullable(Float64))
	// of per-grid-point rates timeSeriesRateToGrid returns; bucketGridSeenArrayAlias
	// is the parallel presence axis (see bucketGridSeenFn); bucketGridTSArrayAlias
	// is the parallel Array(DateTime) anchor axis.
	bucketGridRateArrayAlias = "_hqn_grid"
	bucketGridSeenArrayAlias = "_hqn_seen"
	bucketGridTSArrayAlias   = "_hqn_gts"
	// bucketGridRateValAlias / bucketGridSeenValAlias / bucketGridAnchorRawAlias
	// are the three exploded per-row cells the ARRAY JOIN produces, in lockstep.
	bucketGridRateValAlias   = "_hqn_rate"
	bucketGridSeenValAlias   = "_hqn_seenv"
	bucketGridAnchorRawAlias = "_hqn_anchor"
	// bucketGridRungValAlias is the rung's rate with the < 2-sample NULL
	// resolved to 0 — see the emitter doc for why 0 is the right resolution
	// and not a dropped rung.
	bucketGridRungValAlias = "_hqn_v"
	// bucketGridShortAlias marks the +Inf rung of an anchor whose window holds
	// fewer than two samples: the whole (series, anchor) row is then dropped,
	// which is this shape's spelling of RangeBucketFanout.MinSamples.
	bucketGridShortAlias = "_hqn_short"
	// bucketGridPairsAlias holds the per-(series, anchor) ladder as an
	// le-sorted array of `(le, rate)` tuples; bucketGridLadderAlias is its
	// value column alone, read twice by the differencing below.
	bucketGridPairsAlias  = "_hqn_pairs"
	bucketGridLadderAlias = "_hqn_ladder"
)

// Lambda parameter names for the ladder reshape. Distinct one-letter names
// so a nested lambda never shadows an enclosing one.
const (
	bucketGridCountParam  = "hc"
	bucketGridPairParam   = "hp"
	bucketGridBoundParam  = "hb"
	bucketGridLadderParam = "hj"
)

// bucketGridRateFn is the native per-grid-point rate aggregate — the same
// timeSeriesRateToGrid emitRangeWindowGridNative uses for a scalar counter,
// applied here to one `le` rung's cumulative counter series.
const bucketGridRateFn = "timeSeriesRateToGrid"

// bucketGridSeenFn is the native aggregate this emit reads as a per-grid-point
// PRESENCE signal rather than for its own value.
//
// The fan-out shape builds each (series, anchor) ladder over the UNION of the
// bucket layouts that anchor's own window carries — a bound the window never
// reported contributes no rung at all. The native shape aggregates per (series,
// `le`) across the WHOLE grid, so without a presence signal every rung the
// series ever used would appear at every anchor, and a rung outside this
// anchor's window would land on the ladder carrying a fabricated 0. That is not
// a harmless extra bound: the monotonic-envelope repair above lifts it to its
// predecessor's cumulative value, which MOVES the interpolation's lower bound
// and so changes the answered quantile.
//
// timeSeriesResetsToGrid returns NULL for a grid point whose window holds ZERO
// samples and a (possibly 0) count for one that holds at least one, which is
// exactly the "did this rung appear in this anchor's window" predicate. Its own
// counter-reset count is never read. It shares the family's version floor and
// experimental gate, so it costs no additional capability.
const bucketGridSeenFn = "timeSeriesResetsToGrid"

// emitRangeBucketGridNative renders a chplan.RangeBucketGridNative — the
// ClickHouse-native lowering of the classic-histogram `rate` window fold that
// sits under `histogram_quantile(phi, <agg> by(le) (rate(<bucket>[range])))`
// in range mode.
//
// Shape (six levels; <gk> is the series-identity key list):
//
//	-- 5. difference the ladder back into per-bucket counts
//	SELECT <gk>, anchor_ts,
//	       arrayMap(hj -> if(hj = 1, _hqn_ladder[hj], _hqn_ladder[hj] - _hqn_ladder[hj-1]),
//	                arrayEnumerate(_hqn_ladder)) AS BucketCounts,
//	       ExplicitBounds
//	FROM (
//	  -- 4. split the sorted pair array into ladder + union bounds
//	  SELECT <gk>, anchor_ts,
//	         arrayMap(hp -> hp.2, _hqn_pairs) AS _hqn_ladder,
//	         arrayFilter(hb -> isFinite(hb), arrayMap(hp -> hp.1, _hqn_pairs)) AS ExplicitBounds
//	  FROM (
//	    -- 3. regroup the per-rung rates into one ladder per (series, anchor)
//	    SELECT <gk>, anchor_ts, arraySort(groupArray((_hqn_le, _hqn_v))) AS _hqn_pairs
//	    FROM (
//	      -- 2. explode the three parallel grid axes in lockstep
//	      SELECT <gk>, _hqn_le, toDateTime64(_hqn_anchor, 9) AS anchor_ts,
//	             ifNull(_hqn_rate, 0) AS _hqn_v,
//	             (isNull(_hqn_rate) AND NOT isFinite(_hqn_le)) AS _hqn_short
//	      FROM (
//	        -- 1. one native rate grid per (series, `le` rung)
//	        SELECT <gk>, _hqn_le,
//	               timeSeriesRateToGrid(<start>, <end>, <step_s>, <window_s>)(<ts>, _hqn_cum) AS _hqn_grid,
//	               timeSeriesResetsToGrid(<start>, <end>, <step_s>, <window_s>)(<ts>, _hqn_cum) AS _hqn_seen,
//	               timeSeriesRange(<start>, <end>, <step_s>) AS _hqn_gts
//	        FROM (
//	          -- 0. unnest each stored row into one row per `le` rung
//	          SELECT <gk>, <ts>, _hqn_rung.1 AS _hqn_le, _hqn_rung.2 AS _hqn_cum
//	          FROM (SELECT <gk-exprs>, <ts>, arrayJoin(arrayZip(<bounds ++ inf>, <cumulative counts>)) AS _hqn_rung
//	                FROM (<Input>) WHERE <scan time bound>)
//	        )
//	        GROUP BY <gk>, _hqn_le
//	      ) ARRAY JOIN _hqn_grid AS _hqn_rate, _hqn_seen AS _hqn_seenv, _hqn_gts AS _hqn_anchor
//	      WHERE _hqn_seenv IS NOT NULL
//	    )
//	    GROUP BY <gk>, anchor_ts
//	    HAVING max(_hqn_short) = 0
//	  )
//	)
//
// Load-bearing details, each pinned to a semantic the fan-out fold owns:
//
//   - The unnest builds the rung's CUMULATIVE count, not its bucket count:
//     `arrayCumSum` over the finite bounds' counts, with the +Inf rung carrying
//     the whole row's `arraySum`. The +Inf rung is appended explicitly rather
//     than read off BucketCounts' trailing element because a stored row may
//     carry len(BucketCounts) == len(ExplicitBounds) (no materialised overflow
//     bucket) just as legitimately as len+1 — the fan-out fold's own
//     `arraySlice(cs, 1, length(bs))` + `arraySum(cs)` pair makes the identical
//     allowance.
//
//   - A rung PRESENT in the anchor's window but carrying fewer than two samples
//     resolves to 0, not to a dropped rung: the fan-out's per-rung
//     counterIncreaseFold answers 0 for a one-sample rung (an empty consecutive-
//     diff array) and its extrapolation factor collapses to 1, so the rung
//     appears on the ladder with value 0. `ifNull(_hqn_rate, 0)` reproduces that
//     exactly; bucketGridSeenFn is what keeps it from also fabricating rungs the
//     window never carried.
//
//   - The rung's rate is multiplied back by the window seconds.
//     timeSeriesRateToGrid answers PromQL's `rate` — the extrapolated increase
//     DIVIDED by the range — while the fan-out ladder this node substitutes for
//     carries the undivided increase (internal/promql's histogramWindowFold
//     leaves the division out on the range path: `histogram_quantile` reads only
//     the ratios between rungs, and a per-series constant cancels out of every
//     ratio). Within one arm that difference is invisible for exactly that
//     reason. Across the two arms of a temporality union it is NOT: the
//     DELTA arm stays on the fan-out and publishes an undivided ladder, so a
//     native arm publishing a rate would have the across-series merge add two
//     ladders 300x apart and answer a quantile neither arm means. Restoring the
//     scale here makes this node's output the SAME QUANTITY the fan-out
//     publishes rather than merely a proportional one.
//
//   - The +Inf rung is reported by EVERY stored row, so its own sample set is
//     the series' whole in-window sample set and its NULL is precisely
//     "this (series, anchor) window holds fewer than two samples". Dropping the
//     group on it is this shape's spelling of RangeBucketFanout.MinSamples =
//     rateMinSamples, which reference PromQL applies per SERIES.
//
//   - `arraySort(groupArray((le, rate)))` pairs the two values inside ONE
//     tuple rather than aligning two independent groupArrays positionally, so
//     the ladder cannot be built from a mismatched pairing, and sorting the
//     tuple orders the ladder by `le` with +Inf last.
//
// The experimental setting `allow_experimental_time_series_aggregate_functions=1`
// is NOT emitted here: the engine detects the native node in the plan and
// stamps it onto the per-query ClickHouse context, exactly as it does for
// chplan.RangeWindowGridNative.
func (e *emitter) emitRangeBucketGridNative(r *chplan.RangeBucketGridNative) error {
	if r.Input == nil {
		return fmt.Errorf("%w: RangeBucketGridNative.Input is nil", ErrUnsupported)
	}
	if r.Step <= 0 {
		return fmt.Errorf("%w: RangeBucketGridNative requires Step > 0 (range mode)", ErrUnsupported)
	}
	if r.Range <= 0 {
		return fmt.Errorf("%w: RangeBucketGridNative requires Range > 0", ErrUnsupported)
	}
	if r.Start.IsZero() || r.End.IsZero() {
		return fmt.Errorf("%w: RangeBucketGridNative requires a pinned Start/End grid", ErrUnsupported)
	}
	if r.AnchorAlias == "" {
		return fmt.Errorf("%w: RangeBucketGridNative requires AnchorAlias", ErrUnsupported)
	}
	if r.TimestampCol == "" {
		return fmt.Errorf("%w: RangeBucketGridNative requires TimestampCol", ErrUnsupported)
	}
	if r.BucketCountsCol == "" || r.ExplicitBoundsCol == "" {
		return fmt.Errorf("%w: RangeBucketGridNative requires BucketCountsCol and ExplicitBoundsCol", ErrUnsupported)
	}
	if len(r.GroupBy) != len(r.GroupByAliases) {
		return fmt.Errorf("%w: RangeBucketGridNative GroupBy/GroupByAliases length mismatch (%d vs %d)",
			ErrUnsupported, len(r.GroupBy), len(r.GroupByAliases))
	}
	for i, a := range r.GroupByAliases {
		if a == "" {
			return fmt.Errorf("%w: RangeBucketGridNative.GroupByAliases[%d] is empty — every level above the "+
				"unnest references its keys by name", ErrUnsupported, i)
		}
	}

	// Rendered before the input subquery because collectGroupByFrags binds any
	// captured args at CALL time and the group keys are the first args-bearing
	// text the emitted SQL writes (the innermost SELECT list, ahead of its own
	// FROM). See collectGroupByFrags.
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}
	inner, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}

	offsetNS := r.Offset.Nanoseconds()
	// The membership grid folds Offset onto both edges (the window becomes
	// (anchor - Offset - Range, anchor - Offset]); the REPORTED anchor axis
	// must not move, so its own timeSeriesRange keeps the unshifted bounds.
	// Identical contract to emitRangeWindowGridNative.
	gridParams := []Frag{
		nativeGridTimeBoundFrag(r.Start, offsetNS),
		nativeGridTimeBoundFrag(r.End, offsetNS),
		InlineLit(int64(r.Step.Seconds())),
		InlineLit(int64(r.Range.Seconds())),
	}
	anchorAxis := Call(
		"timeSeriesRange",
		nativeGridTimeBoundFrag(r.Start, 0),
		nativeGridTimeBoundFrag(r.End, 0),
		InlineLit(int64(r.Step.Seconds())),
	)

	keyCols := make([]Frag, 0, len(r.GroupByAliases))
	for _, a := range r.GroupByAliases {
		keyCols = append(keyCols, Col(a))
	}

	// Level 0 — unnest one stored histogram row into one row per `le` rung.
	unnest := NewQuery().From(inner)
	for i, g := range groupFrags {
		unnest.Select(As(g, r.GroupByAliases[i]))
	}
	unnest.Select(Col(r.TimestampCol))
	unnest.Select(As(Call("arrayJoin", bucketGridRungsFrag(r)), bucketGridRungAlias))
	maybePushRangeScanTimeBound(unnest, r.TimestampCol, r.Start, r.End, offsetNS, r.Range.Nanoseconds())

	// Level 0b — split the tuple so the aggregate level can group on the bound
	// and reduce the count. Kept as its own level because a GROUP BY key that
	// is a tuple-index of a same-SELECT arrayJoin alias is not a shape
	// ClickHouse resolves.
	rungs := NewQuery().From(unnest.Frag())
	rungs.Select(keyCols...)
	rungs.Select(Col(r.TimestampCol))
	rungs.Select(As(TupleIndex(Col(bucketGridRungAlias), 1), bucketGridLeAlias))
	rungs.Select(As(TupleIndex(Col(bucketGridRungAlias), 2), bucketGridCumAlias))

	// Level 1 — one native rate grid (and one presence grid) per (series, rung).
	grids := NewQuery().From(rungs.Frag())
	grids.Select(keyCols...)
	grids.Select(Col(bucketGridLeAlias))
	grids.Select(As(Parametric(bucketGridRateFn, gridParams,
		Col(r.TimestampCol), Col(bucketGridCumAlias)), bucketGridRateArrayAlias))
	grids.Select(As(Parametric(bucketGridSeenFn, gridParams,
		Col(r.TimestampCol), Col(bucketGridCumAlias)), bucketGridSeenArrayAlias))
	grids.Select(As(anchorAxis, bucketGridTSArrayAlias))
	grids.GroupBy(append(append([]Frag{}, keyCols...), Col(bucketGridLeAlias))...)

	// Level 2 — explode the three parallel axes in lockstep and resolve the
	// two distinct NULL meanings.
	explode := NewQuery().From(grids.Frag())
	explode.Select(keyCols...)
	explode.Select(Col(bucketGridLeAlias))
	explode.Select(As(Call("toDateTime64", Col(bucketGridAnchorRawAlias), InlineLit(int64(nanoScale))), r.AnchorAlias))
	explode.Select(As(Mul(
		Call("ifNull", Col(bucketGridRateValAlias), InlineLit(0.0)),
		InlineLit(r.Range.Seconds()),
	), bucketGridRungValAlias))
	explode.Select(As(And(
		IsNull(Col(bucketGridRateValAlias)),
		Not(Call("isFinite", Col(bucketGridLeAlias))),
	), bucketGridShortAlias))
	explode.ArrayJoin(
		As(Col(bucketGridRateArrayAlias), bucketGridRateValAlias),
		As(Col(bucketGridSeenArrayAlias), bucketGridSeenValAlias),
		As(Col(bucketGridTSArrayAlias), bucketGridAnchorRawAlias),
	)
	explode.Where(IsNotNull(Col(bucketGridSeenValAlias)))

	// Level 3 — regroup the per-rung rates into one ladder per (series, anchor).
	regroup := NewQuery().From(explode.Frag())
	regroup.Select(keyCols...)
	regroup.Select(Col(r.AnchorAlias))
	regroup.Select(As(Call("arraySort",
		Call("groupArray", Tuple(Col(bucketGridLeAlias), Col(bucketGridRungValAlias)))),
		bucketGridPairsAlias))
	regroup.GroupBy(append(append([]Frag{}, keyCols...), Col(r.AnchorAlias))...)
	regroup.Having(Eq(Call("max", Col(bucketGridShortAlias)), InlineLit(int64(0))))

	// Level 4 — split the sorted pairs into the cumulative ladder and the union
	// bounds. The +Inf rung is dropped from the bounds (a stored ExplicitBounds
	// never carries it) but KEPT on the ladder, so the differenced counts array
	// is one longer than the bounds — the classic-histogram row contract.
	split := NewQuery().From(regroup.Frag())
	split.Select(Col(r.AnchorAlias))
	split.Select(keyCols...)
	split.Select(As(Call("arrayMap",
		Lambda1(bucketGridPairParam, TupleIndex(BareIdent(bucketGridPairParam), 2)),
		Col(bucketGridPairsAlias)), bucketGridLadderAlias))
	split.Select(As(Call("arrayFilter",
		Lambda1(bucketGridBoundParam, Call("isFinite", BareIdent(bucketGridBoundParam))),
		Call("arrayMap",
			Lambda1(bucketGridPairParam, TupleIndex(BareIdent(bucketGridPairParam), 1)),
			Col(bucketGridPairsAlias))), r.ExplicitBoundsCol))

	// Level 5 — difference the ladder back into the per-bucket counts every
	// downstream consumer of a classic-histogram row expects. Column order
	// matches the fan-out reshape's final Project (anchor, keys, counts,
	// bounds) so the two arms of a temporality union line up positionally.
	counts := NewQuery().From(split.Frag())
	counts.Select(Col(r.AnchorAlias))
	counts.Select(keyCols...)
	counts.Select(As(bucketGridDifferenceFrag(), r.BucketCountsCol))
	counts.Select(Col(r.ExplicitBoundsCol))

	return e.emitSelect(counts)
}

// bucketGridRungsFrag renders one stored histogram row's `(le, cumulative
// count)` rungs as an Array(Tuple(Float64, Float64)), ready for arrayJoin:
//
//	arrayZip(arrayPushBack(<bounds>, inf),
//	         arrayPushBack(arrayCumSum(arraySlice(<counts_f>, 1, length(<bounds>))),
//	                       arraySum(<counts_f>)))
//
// with `<counts_f> = arrayMap(hc -> toFloat64(hc), <BucketCounts>)`.
//
// Both arms are exactly `length(<bounds>) + 1` long — arrayZip rejects a
// mismatch outright — which is what makes the row shape allowance explicit:
// the +Inf rung's total comes from `arraySum` over the WHOLE counts array and
// the finite rungs from the first `length(<bounds>)` entries, so a row storing
// an overflow bucket and a row omitting it both produce the same ladder.
func bucketGridRungsFrag(r *chplan.RangeBucketGridNative) Frag {
	countsF := Call("arrayMap",
		Lambda1(bucketGridCountParam, Call("toFloat64", BareIdent(bucketGridCountParam))),
		Col(r.BucketCountsCol))
	bounds := Col(r.ExplicitBoundsCol)
	return Call(
		"arrayZip",
		// `inf` is ClickHouse's Float64 infinity constant, the bound of the
		// overflow rung PromQL spells `le="+Inf"`.
		Call("arrayPushBack", bounds, BareIdent("inf")),
		Call("arrayPushBack",
			Call("arrayCumSum", Call("arraySlice", countsF, InlineLit(int64(1)), Call("length", bounds))),
			Call("arraySum", countsF)),
	)
}

// bucketGridDifferenceFrag renders `counts[i] = ladder[i] - ladder[i-1]`, with
// the first rung passing through unchanged — the same round trip out of the
// cumulative domain internal/promql's classicBucketWindowCountsExpr performs,
// and exact for the same reason: the stage above re-accumulates these counts,
// and a sum of differences telescopes back to the ladder it came from.
func bucketGridDifferenceFrag() Frag {
	ladder := Col(bucketGridLadderAlias)
	pos := BareIdent(bucketGridLadderParam)
	return Call("arrayMap",
		Lambda1(bucketGridLadderParam, If(
			Eq(pos, InlineLit(int64(1))),
			Subscript(ladder, pos),
			Sub(Subscript(ladder, pos), Subscript(ladder, Sub(pos, InlineLit(int64(1))))),
		)),
		Call("arrayEnumerate", ladder))
}

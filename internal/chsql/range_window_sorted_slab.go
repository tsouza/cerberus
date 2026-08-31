package chsql

import "github.com/tsouza/cerberus/internal/chplan"

// This file implements the SORTED-SLAB emitter for the query_range matrix
// shape of sum_over_time() / avg_over_time() (cerberus issue #2761),
// gated by chplan.RangeWindow.SortedSlabOverTime.
//
// The unchanged path (emitWindowedArrayMatrix, reached via
// emitRangeWindowOverTime for every *_over_time member without a direct
// aggregate) is a SAMPLE-SIDE fan-out: each input row arrayJoins across every
// anchor whose window contains it, then a `GROUP BY (series, anchor)`
// rebuilds one (ts, value) array PER anchor. That regroup carries no size
// guard at all (unlike rate/increase/delta's fixed-accumulator/native paths,
// which #2429 gates) — peak memory is O(samples-in-range x anchors) per
// series, an unguarded fan-out axis of the same shape that caused the 2.12
// GiB rate() OOM (run 27277793810).
//
// The sorted-slab shape collapses the per-(series, anchor) regroup into a
// SINGLE `GROUP BY series` that builds one sorted (ts, value) array (the
// "slab") per series, then maps the anchor grid to a per-anchor value by
// slicing that ONE array with arrayFilter — mirroring the fused subquery
// emitter's sliceOf/mapAnchors shape (range_window_fused.go), applied here to
// the matrix's own step-anchor grid over the RAW per-series samples rather
// than to an inner subquery's anchor grid over an already-reduced value
// array. Peak memory is O(samples-in-range) per series, independent of
// anchor count.
//
// Order/precision: the per-anchor window slice feeds overTimeArrayValueFrag
// UNCHANGED — the exact function the array-fold's own outer SELECT calls —
// so sum_over_time's arraySum and avg_over_time's arrayAvg fold their
// slice in the SAME left-to-right order the array-fold's arraySum/arrayAvg
// over window_vals uses (arrayFilter preserves the samples array's
// arraySort-ascending relative order, element for element). This is the
// "arraySlice[-shaped] form ... MANDATORY ... under the byte-identical
// contract" the issue calls for: reusing the identical reducer over an
// identically-ordered slice, rather than introducing arrayReduceInRanges
// (whose segment-tree partial-state merging would reorder the float
// summation and break byte-identity with the array-fold — see
// chplan.RangeWindow.SortedSlabOverTime's own doc).
//
// No dedup: unlike the extrapolated/fused matrix emitters
// (dedupWindowPairsByTsFrag), the array-fold path this narrows
// (windowValsFrag, range_window.go) does NOT collapse duplicate-timestamp
// samples before reducing — every sample counts individually. This emitter
// reproduces that exactly: the slab is a plain arraySort(groupArray(...))
// with no dedup layer, so a duplicate-timestamp pair sums/averages the same
// as the array-fold's own window_vals would.
//
// Deliberately arrayFilter, not arraySlice-via-binary-search: `samples` is
// NOT anchor-grid-aligned (raw sample timestamps), so cutting an anchor's
// window out of it needs a real index lookup (e.g. a binary-search-style
// function) rather than the arithmetic grid-index math
// range_window_fused.go's coveredValuesFrag uses for two co-aligned anchor
// grids. Locating (lo, hi) via such a lookup was not verified against this
// codebase's pinned chDB substrate in this change, so it stays a documented
// follow-up (issue #2804) rather than shipping unverified; arrayFilter is
// the proven-safe idiom (byte-for-byte range_window_fused.go's own sliceOf)
// that already delivers this issue's actual goal — O(samples-in-range) peak
// memory per series, independent of anchor count — without it.
//
// Scope: sum_over_time / avg_over_time only — chopt.FeatureSortedSlabOverTime
// documents why the remaining array-path *_over_time members
// (first/last/stddev/stdvar/mad_over_time) are tracked as a follow-up
// (issue #2804) rather than included here.

// emitRangeWindowSortedSlabOverTime renders the sorted-slab SQL skeleton for
// a shape-eligible matrix sum_over_time / avg_over_time RangeWindow:
//
//	SELECT <series>, anchor_row.1 AS anchor_ts, [<ts col>,] anchor_row.3 AS Value FROM (
//	  SELECT <series>, arrayJoin(arrayFilter(p -> p.2, anchor_grid)) AS anchor_row FROM (
//	    SELECT <series>,
//	      arrayMap(a -> (a, length(vals) >= 1, <overTimeArrayValueFrag(fn, vals)>), <anchors>)
//	        AS anchor_grid
//	    FROM (
//	      SELECT <series>, arraySort(groupArray((TimeUnix, Value))) AS samples
//	      FROM (<input>)
//	      WHERE <scan bounds>
//	      GROUP BY <series>
//	    )
//	  )
//	)
//
// where `vals` is bound once per anchor (letBindFrag) to
// `arrayMap(p -> p.2, arrayFilter(p -> p.1 > a-range AND p.1 <= a, samples))`
// — the SAME half-open `(a-range, a]` membership windowFilterPairsFrag
// applies, over the per-series samples array instead of a per-(series,
// anchor) regrouped one.
//
// Anchors whose window is empty are dropped by the `anchor_row.2` qualifying
// filter — the sorted-slab analogue of emitWindowedArrayMatrix's implicit
// "no group for an empty anchor" behavior (both shapes drop empty windows by
// construction, matching minWindowSize=1's semantics without a separate
// WHERE on the outer SELECT).
func (e *emitter) emitRangeWindowSortedSlabOverTime(r *chplan.RangeWindow) error {
	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	stepNS := r.Step.Nanoseconds()
	numAnchors := r.OuterRange.Nanoseconds()/stepNS + 1
	end, numAnchors = stepAlignGrid(r, end, stepNS, numAnchors)
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innerSub, srcTs := fanoutTsSource(innerSub, r.TimestampColumn)

	// Layer 1 — per-series sorted (ts, value) samples slab, ONE GROUP BY
	// series (vs. emitWindowedArrayMatrix's GROUP BY (series, anchor)). No
	// dedup — see this file's doc comment.
	samplesQ := NewQuery().From(innerSub)
	samplesQ.Select(groupFrags...)
	samplesQ.Select(As(groupArrayPairFrag(srcTs, r.ValueColumn), "samples"))
	maybePushInnerScanTimeBounds(samplesQ, r, srcTs, rangeNS)
	samplesQ.GroupBy(groupFrags...)

	// The per-anchor value expression, built once against the `vals`
	// identifier letBindFrag binds below — byte-identical to the
	// array-fold's own overTimeArrayValueFrag(r.Func, BareIdent("window_vals"))
	// call, over a differently-sliced (but identically-ordered) array.
	valueExpr, err := overTimeArrayValueFrag(r.Func, BareIdent("vals"))
	if err != nil {
		return err
	}

	// Layer 2 — one (anchor_ts, qualifies, value) triple per grid anchor,
	// materialised ONCE per series as `anchor_grid`. The window slice is
	// bound once per anchor via letBindFrag so `valueExpr`'s multiple
	// references to `vals` don't re-run the arrayFilter per reference.
	anchors := Call(
		"arrayMap",
		Lambda1("i", anchorBaseAtIdxFrag(end, stepNS)),
		Call("range", InlineLit(numAnchors)),
	)
	perAnchor := Call(
		"arrayMap",
		Lambda1("a", letBindFrag("vals", sortedSlabWindowValsFrag(BareIdent("a"), rangeNS), func(vals Frag) Frag {
			return Tuple(BareIdent("a"), Gte(Call("length", vals), InlineLit(int64(1))), valueExpr)
		})),
		anchors,
	)
	gridQ := NewQuery().From(samplesQ.Frag())
	gridQ.Select(groupFrags...)
	gridQ.Select(As(perAnchor, "anchor_grid"))

	// Layer 3 — arrayJoin the qualifying anchors back into one row per
	// (series, anchor), carrying the reduced value.
	joinQ := NewQuery().From(gridQ.Frag())
	joinQ.Select(groupFrags...)
	joinQ.Select(As(
		Call("arrayJoin", Call(
			"arrayFilter",
			Lambda1("p", tupleElemFrag(BareIdent("p"), 2)),
			BareIdent("anchor_grid"),
		)),
		"anchor_row",
	))

	// Outer SELECT — matches emitWindowedArrayMatrix's output column shape.
	outer := NewQuery().From(joinQ.Frag())
	outer.Select(groupFrags...)
	outer.Select(As(tupleElemFrag(BareIdent("anchor_row"), 1), RangeWindowAnchorAlias))
	projectAnchorAsTimestampColumn(outer, r)
	outer.Select(As(tupleElemFrag(BareIdent("anchor_row"), 3), r.ValueColumn))

	return e.emitSelect(outer)
}

// sortedSlabWindowValsFrag renders the values of the per-series `samples`
// array whose timestamp falls in anchor `a`'s half-open `(a-rangeNS, a]`
// window — element order preserved from `samples` (arraySort-ascending),
// matching windowFilterPairsFrag + windowValsFrag's combined membership and
// ordering over a per-(series, anchor) regrouped array, applied instead to
// the single per-series slab.
func sortedSlabWindowValsFrag(a Frag, rangeNS int64) Frag {
	return Call(
		"arrayMap",
		Lambda1("p", tupleElemFrag(BareIdent("p"), 2)),
		Call(
			"arrayFilter",
			Lambda1("p", And(
				Gt(tupleElemFrag(BareIdent("p"), 1), rangeStartFrag(a, rangeNS)),
				Lte(tupleElemFrag(BareIdent("p"), 1), a),
			)),
			BareIdent("samples"),
		),
	)
}

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
// anchor count — but ONLY when the per-query max_block_size cap below is
// also in effect. Real ClickHouse 26.6.4.55 profiling (cerberus issue
// #3046) found the naive SQL shape alone does NOT bound memory this way:
// ClickHouse's vectorized block execution evaluates the anchor-arrayMap
// lambda across every series row batched into ONE block simultaneously, so
// the per-anchor arrayFilter intermediates this comment describes as
// "sliced anchor by anchor, freed as it goes" were in practice retained for
// the WHOLE block's worth of series before being freed — an
// O(block-width x samples-in-range) footprint, not O(samples-in-range). A
// max_block_size=1 sweep against #3046's own 500-series/480-anchor
// reproduction proved the block width (not the anchor or sample count in
// isolation) drives this: peak memory scaled ~linearly with block width
// (98 MiB at block=1 up to a 6 GiB-container OOM at the default ~65505-row
// block), and forcing block=1 restores the intended per-row bound because
// each series row then finishes (and frees its own anchor-grid
// intermediates) before the next one starts. See
// internal/engine.applySortedSlabOverTimeMemoryBound, the mandatory
// per-query companion setting that makes this file's own O(samples-in-range)
// claim actually hold — this emitter's SQL shape is necessary but not
// sufficient for it on its own.
//
// Order/precision: the per-anchor window slice feeds overTimeArrayValueFrag
// UNCHANGED — the exact function the array-fold's own outer SELECT calls —
// so every reducer this shape covers folds the slice in the SAME
// left-to-right order the array-fold's identical reducer over window_vals
// uses (arrayFilter preserves the samples array's arraySort-ascending
// relative order, element for element). This is the "arraySlice[-shaped]
// form ... MANDATORY ... under the byte-identical contract" the issue calls
// for: reusing the identical reducer over an identically-ordered slice,
// rather than introducing arrayReduceInRanges (whose segment-tree
// partial-state merging would reorder the float summation and break
// byte-identity with the array-fold — see
// chplan.RangeWindow.SortedSlabOverTime's own doc). Per function:
//
//   - sum_over_time / avg_over_time: arraySum / arrayAvg — a single
//     left-to-right fold, unaffected by anything but element order.
//   - first_over_time: `vals[1]` — a POSITION pick, not a reduction; order
//     preservation is not merely a precision nicety here but the entire
//     correctness argument (a shuffled slice picks the wrong element).
//   - stddev_over_time / stdvar_over_time: the two-pass
//     `μ = arrayAvg(vals); Σ(x-μ)² / N` (varPopTwoPassFrag) — TWO
//     left-to-right folds over the identically-ordered slice (one for μ, one
//     for the centred sum), so the same order argument applies twice.
//   - mad_over_time: two nested applications of the SAME
//     quantileExactInclusive(0.5) median (medianOverArrayFrag) — CH's
//     quantile implementation sorts its input internally, so it is already
//     order-INDEPENDENT of the slice's incoming order; byte-identity here
//     therefore rests on set-membership equality (same elements survive the
//     slice, whichever order they arrive in), not fold order.
//
// See range_window_sorted_slab_chdb_test.go's per-function subtests for the
// executed byte-identical-contract proof of each.
//
// Duplicate rows: the slab is assembled through the SAME windowSamplePairsFrag
// gate the array-fold path uses (range_window.go), so it answers under the
// identical rule — the distinct (timestamp, value) rows when the window
// declares chplan.RangeWindow.DistinctSampleRows (cerberus issue #2914), every
// stored row otherwise. It deliberately does NOT apply the rate family's
// stronger per-timestamp collapse (dedupWindowPairsByTsFrag), which the
// extrapolated/fused matrix emitters carry and this family does not: a
// same-timestamp pair carrying DIFFERENT values still contributes both samples
// here, exactly as the array-fold's own window_vals does (cerberus issue
// #2905).
//
// Deliberately arrayFilter, not arraySlice-via-binary-search — CLOSED with a
// negative result (cerberus issue #2804), not an open follow-up:
//
// `samples` is NOT anchor-grid-aligned (raw sample timestamps), so cutting
// an anchor's window out of it needs a real index LOOKUP (find the (lo, hi)
// position bounding the `(a-range, a]` interval) rather than the arithmetic
// grid-index math range_window_fused.go's coveredValuesFrag uses for two
// co-aligned anchor grids. #2761 punted this for lack of a verified
// function name/version floor; #2804 went and verified it, against this
// codebase's pinned chDB substrate (ClickHouse 26.5.1.1, `SELECT version()`)
// and the upstream ClickHouse array-functions reference:
//
//   - `system.functions` on that substrate carries no arrayBinarySearch,
//     lower-bound, upper-bound, or bisect-shaped array function of any
//     name — confirmed by direct query, not by absence-of-recall.
//   - The one function that DOES run a genuine binary-search algorithm over
//     a sorted array, `indexOfAssumeSorted(arr, x)` (added ClickHouse 2024),
//     has the wrong CONTRACT for this site: it is an EXACT-match search —
//     verified live, it returns 0 for a probe value that falls strictly
//     between two elements, below the array's minimum, or above its
//     maximum, exactly like plain `indexOf`, just faster on a large sorted
//     array. It answers "is x present, and where", never "how many
//     elements are <= x" (the insertion-point / lower-bound question an
//     anchor boundary — an arbitrary timestamp essentially never equal to a
//     stored sample — actually needs). No combination of `indexOfAssumeSorted`
//     calls recovers a lower-bound answer without first locating a
//     guaranteed-present probe value, which the boundary itself is not.
//   - The upstream ClickHouse docs' own array-functions reference lists no
//     binary-search, sorted-array index-lookup, lower-bound, upper-bound,
//     insertion-point, or bisect function either.
//
// With no verified function at any available version floor, arraySlice-via-
// binary-search has no implementation to measure — the negative result IS
// the answer, mirroring this codebase's other closed-without-a-function
// investigations (#2768, #2750, #2923, #2894). arrayFilter stays the
// mechanism: it is the proven-safe idiom (byte-for-byte
// range_window_fused.go's own sliceOf) that already delivers this issue's
// actual goal — O(samples-in-range) peak memory per series, independent of
// anchor count (cerberus issue #2761, memory-bound-corrected by #3046/#3051)
// — without needing an index lookup at all. Re-open only if a future
// ClickHouse version ships a real lower-bound/upper-bound array primitive;
// until then this file's own arrayFilter cost (an O(samples) re-scan per
// anchor) is a CPU refinement with no verified path forward, not a
// correctness or memory gap.
//
// Scope: sum_over_time / avg_over_time / first_over_time / stddev_over_time /
// stdvar_over_time / mad_over_time — chopt.FeatureSortedSlabOverTime
// documents the #2761 → #2804 widening history. last_over_time is
// deliberately EXCLUDED (its own native FeatureTSGridLastOverTime staleness
// resample already answers the same question — see
// promql.SortedSlabOverTimeLowerer's own doc for the precedence
// argument); quantile_over_time and the min/max/count/present_over_time
// direct-aggregate family never reach this emitter at all (see
// overTimeArrayValueFrag's own doc and emitRangeWindowOverTime's
// overTimeDirectAggFrag fast path).

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
	// series (vs. emitWindowedArrayMatrix's GROUP BY (series, anchor)),
	// assembled through the shared windowSamplePairsFrag gate so this
	// lowering answers under the same duplicate-row rule the array-fold does
	// — see this file's doc comment.
	samplesQ := NewQuery().From(innerSub)
	samplesQ.Select(groupFrags...)
	samplesQ.Select(As(windowSamplePairsFrag(r, srcTs, r.ValueColumn), "samples"))
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

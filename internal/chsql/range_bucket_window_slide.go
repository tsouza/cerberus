package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Column / alias constants the anchor-injection window-slide emit threads
// between its levels. Every one is emitter-owned and synthetic — the plan
// carries only the anchor / BucketCounts / ExplicitBounds names — and each
// is a named constant for the same reason range_bucket_grid_native.go's
// bucketGrid* aliases are: the producing level and the consuming level
// cannot drift apart into a ClickHouse `Unknown identifier` at query time.
const (
	// rwsRawNsAlias is the per-row MILLISECOND-ENCODED position the
	// sliding window's ORDER BY / RANGE frame reads — Task 1's proven
	// ceil-to-ms formula (windowSlideMsEncodeFrag) applied to a real row's
	// own raw TimestampCol (unshifted — the membership Offset shifts the
	// SENTINEL side instead, see rwsAnchorNsAlias's sibling column below
	// and the emit doc comment's "which side carries the shift" note), or
	// to a sentinel row's `anchor - Offset` position. Despite the name,
	// this is NOT the raw nanosecond value — see windowSlideMsEncodeFrag.
	rwsRawNsAlias = "_rws_raw_ns"
	// rwsAnchorNsAlias carries the REPORTED anchor position (unshifted) on
	// a sentinel row; meaningless on a real row (never read there — real
	// rows are dropped by the final is_anchor filter).
	rwsAnchorNsAlias = "_rws_anchor_ns"
	// rwsCountsFAlias / rwsBoundsFAlias are each row's own BucketCounts /
	// ExplicitBounds cast to Array(Float64) — a sentinel row carries empty
	// arrays for both.
	rwsCountsFAlias = "_rws_counts_f"
	rwsBoundsFAlias = "_rws_bounds_f"
	// rwsIsAnchorAlias distinguishes a real sample row (0) from a
	// synthetic per-anchor sentinel row (1) — the anchor-injection marker
	// the final level filters on.
	rwsIsAnchorAlias = "_rws_is_anchor"
	// rwsAnchorNsArrAlias / rwsAnchorNsArr2Alias hold the sentinel arm's
	// two parallel per-anchor Array(Int64) axes (encoded ns, reported ns)
	// before ArrayJoin zips them row-wise — mirrors
	// range_bucket_grid_native.go's own array-then-explode two-level shape
	// (bucketGridRateArrayAlias etc. before its own ArrayJoin).
	rwsAnchorNsArrAlias  = "_rws_anchor_ns_arr"
	rwsAnchorNsArr2Alias = "_rws_anchor_ns_arr2"
	// rwsCanonBoundsAlias holds the per-SERIES union of every contributing
	// row's own ExplicitBounds (plus +Inf), computed once as a
	// whole-partition window aggregate — see canonicalBoundsFrag's doc.
	rwsCanonBoundsAlias = "_rws_canon_bounds"
	// rwsReprojAlias holds one row's own raw bucket counts reprojected
	// onto rwsCanonBoundsAlias, with ONE extra trailing element —
	// `toFloat64(1 - is_anchor)`, the MinSamples real-row marker — appended
	// so the sliding window's single sumForEach call carries both signals
	// at once (see the emit doc's level-4 note for why this rides on ONE
	// window function rather than a second one sharing the identical
	// PARTITION BY/ORDER BY/RANGE clause).
	rwsReprojAlias = "_rws_reproj"
	// rwsWindowedCountsAlias holds the ONE sliding window aggregate: the
	// anchor-injection ladder (every element but the last) with the
	// MinSamples real-row count riding as its trailing element — see the
	// final level's split.
	rwsWindowedCountsAlias = "_rws_windowed_counts"
)

// Lambda parameter names. Distinct one-letter-ish names so a nested lambda
// never shadows an enclosing one, mirroring range_bucket_grid_native.go's
// own bucketGrid* param constants.
const (
	rwsCountParam  = "rc"
	rwsBoundParam  = "rb"
	rwsOwnBParam   = "ro"
	rwsCanonParam  = "rk"
	rwsLadderParam = "rj"
)

// windowSlideMsScale is nanoseconds per millisecond — the resolution issue
// #2408's Task-1 boundary spike proved exact for the anchor-injection
// mechanism (ClickHouse's numeric RANGE frame needs a plain integer ORDER
// BY column; millisecond precision is the finest that keeps every
// legitimate lookback comfortably under the frame-offset cap below while
// never losing information PromQL's own step/lookback resolution needs —
// see windowSlideEligible's whole-millisecond gate, internal/promql).
const windowSlideMsScale = int64(1_000_000)

// windowSlideMsCeilBias is the ceil-to-ms rounding bias: `intDiv(x + bias,
// scale)` rounds x/scale UP rather than truncating down, which is what
// keeps a sample landing mid-millisecond from being encoded to a position
// BEFORE its true one — the Task-1 spike's proven formula.
const windowSlideMsCeilBias = windowSlideMsScale - 1

// windowSlideMaxFrameOffset is the largest numeric RANGE frame offset
// ClickHouse accepts: the literal 2147483647 is itself rejected even cast,
// so the largest usable value is one less. See
// [RangeBetweenPrecedingAndCurrentRow]'s doc for the confirmed error text.
const windowSlideMaxFrameOffset = int64(2147483646)

// windowSlideMinSamples is the per-(series, anchor) sample floor this
// node's SUM-fold (sum_over_time) window applies: reference PromQL emits a
// sample for ANY non-empty window (internal/promql's
// histogramWindowMinSamples returns stalenessMinSamples = 1 for every
// windowFn but rate/increase/delta/irate/idelta, and this node is never
// built for those — see windowSlideEligible). A window whose frame holds
// only the sentinel's own zero-valued CURRENT ROW (zero real samples) must
// therefore be dropped, not emitted as an all-zero ladder — mirrored via
// rwsRealCountAlias's window-count gate in the final level below.
const windowSlideMinSamples = 1

// emitRangeBucketWindowSlide renders a chplan.RangeBucketWindowSlide — the
// anchor-injection lowering of the classic-histogram SUM-fold window
// (`sum_over_time`) in range mode. See the plan node's own doc comment for
// the mechanism (anchor injection, proven exact over the wrong ASOF-JOIN
// alternative) and why this shape needs no per-rung unnest the way
// RangeBucketGridNative's `rate` shape does.
//
// Shape (six levels; <gk> is the series-identity key list; N =
// r.NumAnchors()):
//
//	-- 5. split the trailing MinSamples marker off, filter to anchor rows
//	--    with >=1 real sample in their window
//	SELECT fromUnixTimestamp64Nano(_rws_anchor_ns) AS anchor_ts, <gk>,
//	       arraySlice(_rws_windowed_counts, 1, length(_rws_windowed_counts) - 1) AS BucketCounts,
//	       arrayFilter(b -> isFinite(b), _rws_canon_bounds) AS ExplicitBounds
//	FROM (
//	  -- 4. the ONE sliding window aggregate (the MinSamples real-row count
//	  --    rides as _rws_reproj's own trailing element — see the
//	  --    load-bearing-details note on why NOT a second window function)
//	  SELECT <gk>, _rws_is_anchor, _rws_anchor_ns, _rws_canon_bounds,
//	         sumForEach(_rws_reproj) OVER (PARTITION BY <gk> ORDER BY _rws_raw_ns
//	                    RANGE BETWEEN <lookbackMs-1> PRECEDING AND CURRENT ROW) AS _rws_windowed_counts
//	  FROM (
//	    -- 3. reproject each row's own bucket layout onto the per-series
//	    --    canonical bound set, with the MinSamples marker appended
//	    SELECT <gk>, _rws_raw_ns, _rws_is_anchor, _rws_anchor_ns, _rws_canon_bounds,
//	           arrayPushBack(<reprojected, differenced ladder>, toFloat64(1 - _rws_is_anchor)) AS _rws_reproj
//	    FROM (
//	      -- 2. per-series union of every row's own ExplicitBounds (+Inf),
//	      --    a whole-partition window aggregate — runs UNCONDITIONALLY,
//	      --    not gated behind a layout-stability probe (Task 1 point 3)
//	      SELECT <gk>, _rws_raw_ns, _rws_counts_f, _rws_bounds_f, _rws_is_anchor, _rws_anchor_ns,
//	             arraySort(arrayDistinct(arrayFlatten(
//	               groupArray(arrayPushBack(_rws_bounds_f, inf)) OVER (PARTITION BY <gk>)
//	             ))) AS _rws_canon_bounds
//	      FROM (
//	        -- 1. UNION ALL of the real sample rows and one synthetic
//	        --    sentinel row per (series, anchor) — the anchor-injection
//	        --    step itself, bounded by windowSlideBoundedSourceFrag
//	        --    (<gk>, _rws_raw_ns, _rws_counts_f, _rws_bounds_f, _rws_is_anchor, _rws_anchor_ns)
//	        (<real rows, ts unshifted>) UNION ALL (<sentinel rows, one per (series, anchor)>)
//	      )
//	    )
//	  )
//	)
//
// Load-bearing details:
//
//   - Only the sentinel side is shifted by Offset (`anchor - Offset`); the
//     real side's own timestamp is read unshifted. Both spellings of the
//     Offset fold are algebraically equivalent (the membership window
//     `(anchor - Offset - Range, anchor - Offset]` is unchanged by which
//     side absorbs the shift), and shifting the sentinel keeps the SCAN
//     bound pushed onto the real side (maybePushRangeScanTimeBound) in its
//     usual, already-proven form.
//   - Bucket-layout reprojection (level 2/3) always runs, exactly as
//     RangeBucketGridNative's own rung-unnest always runs: a standalone
//     layout-stability probe was measured to cost nearly as much as just
//     doing the reprojection while buying nothing actionable (Task 1
//     point 3). A sentinel row's own empty _rws_counts_f/_rws_bounds_f
//     reprojects to an all-zero ladder of the correct (canonical) length
//     through the SAME formula real rows use — no special case needed
//     (see reprojectedLadderFrag's own doc for why the empty-array case
//     falls out for free).
//   - sumForEach pads shorter contributing arrays with zero, but that
//     alone would silently mis-align two rows whose OWN bound sets are
//     the same LENGTH but different VALUES (a layout change that adds one
//     bound and drops another) — reprojection onto rwsCanonBoundsAlias is
//     what keeps the elementwise sum meaningful position-by-position, not
//     just length-compatible.
//   - Level 4 uses exactly ONE window function, not two. A second
//     `sum(1 - is_anchor) OVER (<identical PARTITION BY/ORDER BY/RANGE>)`
//     computing the MinSamples real-row count alongside sumForEach was the
//     first shape tried, and it reproduces a genuine ClickHouse bug when
//     combined with this bound source's two independent reads: `Code: 10.
//     DB::Exception: Not found column __table2._rws_reproj in block
//     sumCount(__table2._rws_is_anchor) ... (NOT_FOUND_COLUMN_IN_BLOCK)`,
//     confirmed against real chDB and reproducible even with
//     optimize_syntax_fuse_functions=0. Appending the marker onto
//     rwsReprojAlias itself (reprojectedLadderFrag) and splitting it back
//     off in level 5 needs only the one window function and sidesteps the
//     bug entirely.
func (e *emitter) emitRangeBucketWindowSlide(r *chplan.RangeBucketWindowSlide) error {
	if r.Input == nil {
		return fmt.Errorf("%w: RangeBucketWindowSlide.Input is nil", ErrUnsupported)
	}
	if r.Step <= 0 {
		return fmt.Errorf("%w: RangeBucketWindowSlide requires Step > 0 (range mode)", ErrUnsupported)
	}
	if r.Range <= 0 {
		return fmt.Errorf("%w: RangeBucketWindowSlide requires Range > 0", ErrUnsupported)
	}
	if r.Start.IsZero() || r.End.IsZero() {
		return fmt.Errorf("%w: RangeBucketWindowSlide requires a pinned Start/End grid", ErrUnsupported)
	}
	if r.AnchorAlias == "" {
		return fmt.Errorf("%w: RangeBucketWindowSlide requires AnchorAlias", ErrUnsupported)
	}
	if r.TimestampCol == "" {
		return fmt.Errorf("%w: RangeBucketWindowSlide requires TimestampCol", ErrUnsupported)
	}
	if r.BucketCountsCol == "" || r.ExplicitBoundsCol == "" {
		return fmt.Errorf("%w: RangeBucketWindowSlide requires BucketCountsCol and ExplicitBoundsCol", ErrUnsupported)
	}
	if len(r.GroupBy) != len(r.GroupByAliases) {
		return fmt.Errorf("%w: RangeBucketWindowSlide GroupBy/GroupByAliases length mismatch (%d vs %d)",
			ErrUnsupported, len(r.GroupBy), len(r.GroupByAliases))
	}
	for i, a := range r.GroupByAliases {
		if a == "" {
			return fmt.Errorf("%w: RangeBucketWindowSlide.GroupByAliases[%d] is empty — every level above the "+
				"union references its keys by name", ErrUnsupported, i)
		}
	}

	offsetNS := r.Offset.Nanoseconds()
	rangeNS := r.Range.Nanoseconds()
	stepNS := r.Step.Nanoseconds()
	startNS := r.Start.UnixNano()
	numAnchors := r.NumAnchors()

	if rangeNS%windowSlideMsScale != 0 {
		return fmt.Errorf("%w: RangeBucketWindowSlide requires a whole-millisecond Range (got %s)",
			ErrUnsupported, r.Range)
	}
	lookbackMs := rangeNS / windowSlideMsScale
	if lookbackMs < 1 || lookbackMs-1 > windowSlideMaxFrameOffset {
		return fmt.Errorf("%w: RangeBucketWindowSlide Range %s encodes to %dms, outside ClickHouse's "+
			"numeric RANGE frame offset headroom (1..%dms)",
			ErrUnsupported, r.Range, lookbackMs, windowSlideMaxFrameOffset+1)
	}
	// baseNS keeps every ms-encoded value non-negative: the earliest raw
	// timestamp any anchor's window can reach back to is Start-Offset-Range
	// (the first anchor's own lower edge) — Task 1's proven formula.
	baseNS := startNS - offsetNS - rangeNS

	// Rendered before the input subquery for the same reason
	// emitRangeBucketGridNative renders its groupFrags first —
	// collectGroupByFrags binds any captured args at CALL time and the
	// group keys are the first args-bearing text the emitted SQL writes.
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}
	inner, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}

	partitionBy := make([]Frag, 0, len(r.GroupByAliases))
	for _, a := range r.GroupByAliases {
		partitionBy = append(partitionBy, Col(a))
	}

	// Level 1a — real sample rows, ts unshifted (see the Offset note
	// above). BucketCounts/ExplicitBounds are cast to Array(Float64) here
	// so both UNION ALL arms carry identical column types.
	real := NewQuery().From(inner)
	for i, g := range groupFrags {
		real.SelectAs(g, r.GroupByAliases[i])
	}
	real.SelectAs(windowSlideMsEncodeFrag(baseNS, Call("toUnixTimestamp64Nano", Col(r.TimestampCol))), rwsRawNsAlias)
	real.SelectAs(toFloat64ArrFrag(rwsCountParam, Col(r.BucketCountsCol)), rwsCountsFAlias)
	real.SelectAs(toFloat64ArrFrag(rwsBoundParam, Col(r.ExplicitBoundsCol)), rwsBoundsFAlias)
	real.SelectAs(InlineLit(int64(0)), rwsIsAnchorAlias)
	// Cast to Int64 explicitly, matching anchorNsArrayFrag's own cast: the
	// bare literal 0 infers as UInt8, and left uncast this placeholder
	// would widen rwsAnchorNsAlias to a Variant(UInt8, Int64) column across
	// the UNION ALL — see anchorNsArrayFrag's doc for why that is fatal
	// (fromUnixTimestamp64Nano needs a concrete Int64, not a Variant).
	real.SelectAs(Call("toInt64", InlineLit(int64(0))), rwsAnchorNsAlias)
	maybePushRangeScanTimeBound(real, r.TimestampCol, r.Start, r.End, offsetNS, rangeNS)

	// Level 1b — discovery: one row per distinct series the bounded scan
	// actually carries, replaying the same scan-bound pushdown as the
	// real arm. Mirrors metricsZeroFillGridArm's own discovery subquery
	// (internal/chsql/range_window.go) — see that function's doc for why
	// the replay, not a CTE or a bound scalar array, is the shape that
	// measures no worse while staying simplest.
	disc := NewQuery().From(inner)
	discKeys := make([]Frag, 0, len(r.GroupByAliases))
	for i, g := range groupFrags {
		disc.SelectAs(g, r.GroupByAliases[i])
		discKeys = append(discKeys, Col(r.GroupByAliases[i]))
	}
	disc.GroupBy(discKeys...)
	maybePushRangeScanTimeBound(disc, r.TimestampCol, r.Start, r.End, offsetNS, rangeNS)

	// Level 1c — the two parallel per-anchor axes: the ENCODED position
	// (anchor - Offset, the side that absorbs the membership shift) and
	// the REPORTED position (anchor, unshifted).
	sentinelArrays := NewQuery().From(disc.Frag())
	for _, a := range r.GroupByAliases {
		sentinelArrays.Select(Col(a))
	}
	sentinelArrays.SelectAs(
		msEncodeArrayFrag(baseNS, anchorNsArrayFrag(startNS-offsetNS, stepNS, numAnchors)),
		rwsAnchorNsArrAlias,
	)
	sentinelArrays.SelectAs(anchorNsArrayFrag(startNS, stepNS, numAnchors), rwsAnchorNsArr2Alias)

	// Level 1d — explode the two axes in lockstep (ArrayJoin zips
	// same-length arrays positionally, exactly as
	// emitRangeBucketGridNative's own explode.ArrayJoin does) and pad the
	// real arm's columns with each sentinel's empty/marker values.
	sentinel := NewQuery().From(sentinelArrays.Frag())
	for _, a := range r.GroupByAliases {
		sentinel.Select(Col(a))
	}
	sentinel.Select(Col(rwsRawNsAlias))
	sentinel.SelectAs(Call("emptyArrayFloat64"), rwsCountsFAlias)
	sentinel.SelectAs(Call("emptyArrayFloat64"), rwsBoundsFAlias)
	sentinel.SelectAs(InlineLit(int64(1)), rwsIsAnchorAlias)
	sentinel.Select(Col(rwsAnchorNsAlias))
	sentinel.ArrayJoin(
		As(Col(rwsAnchorNsArrAlias), rwsRawNsAlias),
		As(Col(rwsAnchorNsArr2Alias), rwsAnchorNsAlias),
	)

	// Level 1 — UNION ALL, bounded on its own cost axis
	// (scanned_rows + series x anchors) — see
	// windowSlideBoundedSourceFrag's doc for why this mirrors
	// lwrFanoutBoundedSourceFrag rather than sharing its constant.
	unionSrc := windowSlideBoundedSourceFrag(
		Paren(UnionAll(real.Frag(), sentinel.Frag())), rwsRawNsAlias,
	)

	// Level 2 — per-series canonical bound set: the union of every
	// contributing row's own ExplicitBounds (+Inf appended so
	// canonicalBoundsFrag's arrayFirstIndex below always finds a match),
	// as a whole-partition (no ORDER BY, no frame) window aggregate.
	canon := NewQuery().From(unionSrc)
	for _, a := range r.GroupByAliases {
		canon.Select(Col(a))
	}
	canon.Select(Col(rwsRawNsAlias))
	canon.Select(Col(rwsCountsFAlias))
	canon.Select(Col(rwsBoundsFAlias))
	canon.Select(Col(rwsIsAnchorAlias))
	canon.Select(Col(rwsAnchorNsAlias))
	canon.SelectAs(canonicalBoundsFrag(partitionBy), rwsCanonBoundsAlias)

	// Level 3 — reproject each row's own (possibly narrower or
	// differently-laid-out) bucket ladder onto the canonical bound set.
	reproj := NewQuery().From(canon.Frag())
	for _, a := range r.GroupByAliases {
		reproj.Select(Col(a))
	}
	reproj.Select(Col(rwsRawNsAlias))
	reproj.Select(Col(rwsIsAnchorAlias))
	reproj.Select(Col(rwsAnchorNsAlias))
	reproj.Select(Col(rwsCanonBoundsAlias))
	reproj.SelectAs(reprojectedLadderFrag(), rwsReprojAlias)

	// Level 4 — the ONE sliding window aggregate. windowSlideMinSamples'
	// real-row count rides as a TRAILING element on the same summed array
	// rather than a second `sum(...) OVER (...)` sharing this window's
	// identical PARTITION BY/ORDER BY/RANGE clause: two window functions
	// over the same window definition where one reads rwsReprojAlias (an
	// expression several levels deep) and the other reads rwsIsAnchorAlias
	// directly hits a real ClickHouse column-pruning bug across this
	// bound source's two independent reads — confirmed against real chDB
	// (25.8/dev): `Code: 10. DB::Exception: Not found column
	// __table2._rws_reproj in block sumCount(__table2._rws_is_anchor) ...
	// (NOT_FOUND_COLUMN_IN_BLOCK)`, reproducible even with
	// optimize_syntax_fuse_functions=0. A single window function has no
	// such interaction, so rwsRealCountFrag appends `toFloat64(1 -
	// is_anchor)` as one more array element ahead of sumForEach, and the
	// final level below splits it back off.
	orderBy := []OrderKey{{Expr: Col(rwsRawNsAlias)}}
	frame := RangeBetweenPrecedingAndCurrentRow(InlineLit(lookbackMs - 1))
	windowed := NewQuery().From(reproj.Frag())
	for _, a := range r.GroupByAliases {
		windowed.Select(Col(a))
	}
	windowed.Select(Col(rwsIsAnchorAlias))
	windowed.Select(Col(rwsAnchorNsAlias))
	windowed.Select(Col(rwsCanonBoundsAlias))
	windowed.SelectAs(
		WindowFrame(Call("sumForEach", Col(rwsReprojAlias)), partitionBy, orderBy, frame),
		rwsWindowedCountsAlias,
	)

	// Level 5 — split the trailing real-count marker back off, keep only
	// the anchor rows whose window covers at least windowSlideMinSamples
	// real samples, and strip +Inf back out of the reported ExplicitBounds
	// (kept internally so arrayFirstIndex always matches — see
	// canonicalBoundsFrag).
	final := NewQuery().From(windowed.Frag())
	final.SelectAs(Call("fromUnixTimestamp64Nano", Col(rwsAnchorNsAlias)), r.AnchorAlias)
	for _, a := range r.GroupByAliases {
		final.Select(Col(a))
	}
	extLen := Call("length", Col(rwsWindowedCountsAlias))
	final.SelectAs(Call("arraySlice", Col(rwsWindowedCountsAlias), InlineLit(int64(1)), Sub(extLen, InlineLit(int64(1)))),
		r.BucketCountsCol)
	final.SelectAs(Call("arrayFilter",
		Lambda1(rwsBoundParam, Call("isFinite", BareIdent(rwsBoundParam))),
		Col(rwsCanonBoundsAlias)), r.ExplicitBoundsCol)
	final.Where(And(
		Eq(Col(rwsIsAnchorAlias), InlineLit(int64(1))),
		Gte(Subscript(Col(rwsWindowedCountsAlias), extLen), InlineLit(float64(windowSlideMinSamples))),
	))

	return e.emitSelect(final)
}

// toFloat64ArrFrag renders `arrayMap(<param> -> toFloat64(<param>), arr)` —
// casts an Array(UInt64)/Array(Float64) column to Array(Float64) so both
// UNION ALL arms of the anchor-injection stream carry identical types.
func toFloat64ArrFrag(param string, arr Frag) Frag {
	return Call("arrayMap", Lambda1(param, Call("toFloat64", BareIdent(param))), arr)
}

// anchorNsArrayFrag renders `arrayMap(i -> <baseNS> + i * <stepNS>,
// range(0, <numAnchors>))` — the N-point per-series anchor grid as plain
// Int64 nanosecond values (not DateTime), so downstream arithmetic (the
// Offset shift, the ceil-to-ms encode) stays integer-exact with no
// DateTime64 scale-9 Decimal coercion in the way (see
// [RangeBetweenPrecedingAndCurrentRow]'s doc and nativeGridTimeBoundFrag's
// for the coercion hazard this sidesteps by never touching DateTime here at
// all — the only DateTime64 cast is the FINAL emitted AnchorAlias column,
// via fromUnixTimestamp64Nano).
// The arrayMap body is cast to Int64 explicitly: ClickHouse infers a bare
// positive integer literal past a magnitude threshold as UInt64 (confirmed
// against real ClickHouse — `toTypeName(1767225900000000000)` is UInt64,
// not Int64), and toUnixTimestamp64Nano — the real arm's own rwsRawNsAlias
// producer — returns Int64. Left uncast, UNION ALL of the two arms widens
// rwsRawNsAlias/rwsAnchorNsAlias to a Variant(Int64, UInt64) column, and
// ClickHouse rejects a Variant/Dynamic column as an ORDER BY key outright
// ("Data types Variant/Dynamic are not allowed in ORDER BY keys") — fatal
// for the sliding window's own ORDER BY on rwsRawNsAlias. The explicit cast
// keeps both arms at the identical Int64 type so no Variant widening ever
// happens.
func anchorNsArrayFrag(gridBaseNS, stepNS, numAnchors int64) Frag {
	return Call("arrayMap",
		Lambda1("i", Call("toInt64", Add(InlineLit(gridBaseNS), Mul(BareIdent("i"), InlineLit(stepNS))))),
		Call("range", InlineLit(int64(0)), InlineLit(numAnchors)))
}

// windowSlideMsEncodeFrag renders Task 1's proven ceil-to-ms encoding of a
// raw nanosecond expression: `toInt64(intDiv(x - baseNS + 999999,
// 1000000))`. baseNS is the emit-time-computed additive constant
// (Start-Offset-Range in ns) that keeps every encoded value non-negative;
// x is any Int64 nanosecond expression. The explicit toInt64 wrap matches
// anchorNsArrayFrag's own cast — see that function's doc for why an
// uncast expression risks a Variant-typed UNION ALL column.
func windowSlideMsEncodeFrag(baseNS int64, x Frag) Frag {
	return Call("toInt64", Call("intDiv",
		Add(Sub(x, InlineLit(baseNS)), InlineLit(windowSlideMsCeilBias)),
		InlineLit(windowSlideMsScale)))
}

// msEncodeArrayFrag applies [windowSlideMsEncodeFrag] to every element of
// an Array(Int64) of raw nanosecond values — the sentinel arm's per-anchor
// axis, generated in bulk by [anchorNsArrayFrag].
func msEncodeArrayFrag(baseNS int64, arr Frag) Frag {
	return Call("arrayMap", Lambda1("m", windowSlideMsEncodeFrag(baseNS, BareIdent("m"))), arr)
}

// canonicalBoundsFrag renders the per-series canonical bound set: the
// sorted, deduplicated union of every contributing row's own
// (ExplicitBounds + Inf), computed as a whole-partition window aggregate
// (no ORDER BY, no frame — [Window], not [WindowFrame]) so it is available
// on every row of the partition, sentinel rows included, with no separate
// join back.
//
//	arraySort(arrayDistinct(arrayFlatten(
//	  groupArray(arrayPushBack(_rws_bounds_f, inf)) OVER (PARTITION BY <gk>)
//	)))
//
// A sentinel row contributes `[inf]` (its own _rws_bounds_f is empty), which
// arrayFlatten/arrayDistinct silently absorb — the union is unaffected by
// how many sentinel rows sit in the partition.
func canonicalBoundsFrag(partitionBy []Frag) Frag {
	perRow := Call("arrayPushBack", Col(rwsBoundsFAlias), BareIdent("inf"))
	return Call("arraySort", Call("arrayDistinct", Call("arrayFlatten",
		Window(Call("groupArray", perRow), partitionBy, nil))))
}

// reprojectedLadderFrag renders one row's own raw BucketCounts, reprojected
// onto rwsCanonBoundsAlias and re-differenced back to per-bucket counts —
// sumForEach's per-row input.
//
//	ownBoundsInf = arrayPushBack(_rws_bounds_f, inf)
//	ownCum       = arrayPushBack(arrayCumSum(arraySlice(_rws_counts_f, 1, length(_rws_bounds_f))),
//	                             arraySum(_rws_counts_f))
//	reprojCum    = arrayMap(rk -> ownCum[arrayFirstIndex(ro -> ro >= rk, ownBoundsInf)], _rws_canon_bounds)
//	reprojDiff   = arrayMap(rj -> if(rj = 1, reprojCum[rj], reprojCum[rj] - reprojCum[rj-1]),
//	                        arrayEnumerate(reprojCum))
//
// ownCum is the row's own CUMULATIVE-at-bound reading (the same
// monotone-envelope construction range_bucket_grid_native.go's
// bucketGridRungsFrag uses for the identical reason: OTLP's ExplicitBounds
// contract makes a prefix sum over the row's own counts exactly its
// cumulative reading at each of ITS OWN bounds). Reprojection then reads,
// for each CANONICAL bound rk, the row's own cumulative reading at the
// smallest of its own bounds >= rk — the standard monotone-envelope
// re-bucketing: a row whose own layout is coarser than the canonical set
// assigns the whole coarse bucket's reading to every finer canonical bound
// it subsumes, which is exact for the read that ships (each canonical
// bound's reprojected value is CORRECT; the interior split a coarser row
// cannot resolve is exactly the same information loss any monotone-envelope
// reprojection carries — nothing narrower is recoverable from a coarser
// stored layout). ownBoundsInf's trailing +Inf guarantees arrayFirstIndex
// always finds a match (+Inf >= any finite canonical bound, and the
// canonical set's own trailing entry is +Inf too), so reprojCum is always
// exactly len(_rws_canon_bounds) long, matching every other row's — which
// is what makes sumForEach's elementwise sum across rows meaningful
// position-by-position, not merely length-compatible (sumForEach ALONE
// only guarantees length-compatibility via zero-padding — see the emit
// doc's load-bearing-details note).
//
// A sentinel row's own _rws_counts_f/_rws_bounds_f are both empty:
// ownBoundsInf degenerates to `[inf]`, ownCum to `[arraySum([])] = [0.0]`,
// and arrayFirstIndex always resolves to index 1 regardless of rk (since
// `[inf]`'s only entry is >= any rk) — so reprojCum is an all-zero array of
// the canonical length, and reprojDiff (differences of an all-zero array)
// is too. No special case is needed for the sentinel side.
//
// The returned array is reprojDiff with ONE extra trailing element —
// `toFloat64(1 - is_anchor)`, the windowSlideMinSamples real-row marker (1
// for a real row, 0 for a sentinel) — appended via arrayPushBack. Every
// row in a partition therefore carries an array of the SAME length
// (len(_rws_canon_bounds) + 1), so sumForEach's per-position sum stays
// meaningful for the marker slot too: the window's final element is the
// COUNT of real rows the frame covers. See the emit doc's level-4 note for
// why this rides on the ladder array itself rather than a second window
// function sharing the identical PARTITION BY/ORDER BY/RANGE clause.
func reprojectedLadderFrag() Frag {
	countsF := Col(rwsCountsFAlias)
	boundsF := Col(rwsBoundsFAlias)
	ownBoundsInf := Call("arrayPushBack", boundsF, BareIdent("inf"))
	ownCum := Call("arrayPushBack",
		Call("arrayCumSum", Call("arraySlice", countsF, InlineLit(int64(1)), Call("length", boundsF))),
		Call("arraySum", countsF))
	reprojCum := Call("arrayMap",
		Lambda1(rwsCanonParam, Subscript(ownCum,
			Call("arrayFirstIndex",
				Lambda1(rwsOwnBParam, Gte(BareIdent(rwsOwnBParam), BareIdent(rwsCanonParam))),
				ownBoundsInf))),
		Col(rwsCanonBoundsAlias))
	pos := BareIdent(rwsLadderParam)
	reprojDiff := Call("arrayMap",
		Lambda1(rwsLadderParam, If(
			Eq(pos, InlineLit(int64(1))),
			Subscript(reprojCum, pos),
			Sub(Subscript(reprojCum, pos), Subscript(reprojCum, Sub(pos, InlineLit(int64(1))))),
		)),
		Call("arrayEnumerate", reprojCum))
	// Trailing MinSamples marker — see rwsReprojAlias's doc for why this
	// rides on the ladder array itself instead of a second window
	// function. 1 for a real row, 0 for a sentinel.
	realMarker := Call("toFloat64", Sub(InlineLit(int64(1)), Col(rwsIsAnchorAlias)))
	return Call("arrayPushBack", reprojDiff, realMarker)
}

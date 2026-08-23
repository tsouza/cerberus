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
	// rwsCanonBoundsAlias holds the per-SERIES union of every row's own
	// ExplicitBounds (plus +Inf) that the bounded scan carries ANYWHERE in
	// [Start-Offset-Range, End-Offset] — computed ONCE per series by
	// rwsCanonBySeriesFrag's own linear (non-windowed) GROUP BY, then
	// attached to every real/sentinel row of that series by the JOIN in
	// the emit function. This is a SUPERSET of any one anchor's own
	// reported-bound set; rwsSumPresenceAlias (computed downstream) is
	// what narrows it back down per anchor — see the emit doc's
	// "per-anchor union, not per-query-range union" note.
	rwsCanonBoundsAlias = "_rws_canon_bounds"
	// rwsCanonKeyAliasPrefix names the JOIN key columns on the
	// canon-by-series side of the join — deliberately DISTINCT from
	// r.GroupByAliases (which the union side uses) so the two sides never
	// share a column name the ON clause has to disambiguate beyond simple
	// qualification, mirroring instantDeltaPrefixSource's own
	// delta_prefix_key_%d convention (internal/chsql/range_window.go).
	rwsCanonKeyAliasPrefix = "_rws_canon_key_"
	// rwsExtendedAlias holds, per row, THREE signals concatenated into one
	// Array(Float64) so the sliding window needs exactly ONE sumForEach
	// call (see the emit doc's level-4 note for why not more than one):
	//
	//  1. [1..canonLen]              has-gated cumulative reading at each
	//                                 canonical bound (0 where this row's
	//                                 own layout does not report that
	//                                 EXACT bound)
	//  2. [canonLen+1..2*canonLen]   per-bound presence indicator (1/0)
	//  3. [2*canonLen+1]             the windowSlideMinSamples real-row
	//                                 marker (1 for a real row, 0 for a
	//                                 sentinel)
	//
	// canonLen = length(rwsCanonBoundsAlias), IDENTICAL across every row
	// of one series (attached by the same JOIN), so every row in a
	// partition carries an array of the exact same length — sumForEach
	// never needs its own zero-padding fallback here.
	rwsExtendedAlias = "_rws_extended"
	// rwsWindowedExtAlias holds the ONE sliding window aggregate over
	// rwsExtendedAlias — see rwsExtendedAlias's own doc for its layout.
	rwsWindowedExtAlias = "_rws_windowed_ext"
	// rwsFilteredCumAlias / rwsFilteredBoundsAlias hold, per anchor row,
	// the windowed cumulative reading and the canonical bound narrowed
	// down to just the positions THIS anchor's own window actually
	// reported (sum-presence >= 1) — the per-(series, anchor) union bound
	// set the fan-out's classicBucketUnionBoundsExpr computes directly via
	// its own GROUP BY (series, anchor); this shape narrows down from the
	// wider per-series superset instead, since a sliding window has no
	// per-anchor GROUP BY to compute the narrower set directly. See the
	// emit doc's B2 note.
	rwsFilteredCumAlias    = "_rws_filtered_cum"
	rwsFilteredBoundsAlias = "_rws_filtered_bounds"
	// rwsRealCountAlias holds the split-off windowSlideMinSamples marker.
	rwsRealCountAlias = "_rws_real_count"
)

// Lambda parameter names. Distinct one-letter-ish names so a nested lambda
// never shadows an enclosing one, mirroring range_bucket_grid_native.go's
// own bucketGrid* param constants.
const (
	rwsCountParam         = "rc"
	rwsBoundParam         = "rb"
	rwsCanonParam         = "rk"
	rwsLadderParam        = "rj"
	rwsFilterValueParam   = "rf"
	rwsFilterPresenceParm = "rp"
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
// Shape (eight levels; <gk> is the series-identity key list; N =
// r.NumAnchors(); <ck> is the JOIN-only canon-by-series key list, distinct
// column names from <gk> — see rwsCanonKeyAliasPrefix):
//
//	-- 7. split the trailing MinSamples marker off, narrow the per-series
//	--    canonical bound set down to what THIS anchor's window actually
//	--    reported (sum-presence >= 1), difference back to per-bucket
//	--    counts, filter to anchor rows with >= windowSlideMinSamples
//	SELECT anchor_ts, <gk>,
//	       <differenced _rws_filtered_cum> AS BucketCounts,
//	       arrayFilter(b -> isFinite(b), _rws_filtered_bounds) AS ExplicitBounds
//	FROM (
//	  -- 6. split _rws_windowed_ext into its three signals and narrow
//	  SELECT anchor_ts, <gk>, _rws_is_anchor,
//	         arrayFilter((c,p) -> p >= 1, sumCum, sumPresence) AS _rws_filtered_cum,
//	         arrayFilter((b,p) -> p >= 1, _rws_canon_bounds, sumPresence) AS _rws_filtered_bounds,
//	         _rws_windowed_ext[2*canonLen+1] AS _rws_real_count
//	  FROM (
//	    -- 5. the ONE sliding window aggregate
//	    SELECT <gk>, _rws_is_anchor, _rws_anchor_ns, _rws_canon_bounds,
//	           sumForEach(_rws_extended) OVER (PARTITION BY <gk> ORDER BY _rws_raw_ns
//	                      RANGE BETWEEN <lookbackMs-1> PRECEDING AND CURRENT ROW) AS _rws_windowed_ext
//	    FROM (
//	      -- 4. per-row has-gated cumulative reading + presence indicator +
//	      --    MinSamples marker, concatenated into ONE array
//	      SELECT <gk>, _rws_raw_ns, _rws_is_anchor, _rws_anchor_ns, _rws_canon_bounds,
//	             arrayConcat(<cumArr>, <presenceArr>, [toFloat64(1 - _rws_is_anchor)]) AS _rws_extended
//	      FROM (
//	        -- 3. UNION ALL of the real sample rows (JOINed to their
//	        --    series' canon-by-series row) and one synthetic sentinel
//	        --    row per (series, anchor) (generated directly FROM
//	        --    canon-by-series, so it carries _rws_canon_bounds with no
//	        --    join needed) — the anchor-injection step itself, bounded
//	        --    by lwrFanoutBoundedSourceFrag
//	        (<real rows JOIN canon-by-series>) UNION ALL (<sentinel rows>)
//	      )
//	    )
//	  )
//	)
//	-- (canon-by-series itself, read by both arms above:)
//	--   -- 2. per-series union of every row's own ExplicitBounds (+Inf) —
//	--   --    an ORDINARY (non-windowed) GROUP BY, linear in row count,
//	--   --    NOT a whole-partition window aggregate (issue found in
//	--   --    review: a windowed groupArray here would materialise
//	--   --    O(rows^2) per series)
//	--   SELECT <ck>, arraySort(arrayDistinct(arrayFlatten(
//	--     groupArray(arrayPushBack(_rws_bounds_f, inf))
//	--   ))) AS _rws_canon_bounds
//	--   FROM (-- 1. real sample rows --) GROUP BY <ck>
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
//   - GROUP-BY / JOIN key rendering. r.GroupBy is rendered via a FRESH
//     Frag at every independent SELECT statement that needs it (never a
//     single pre-rendered Frag reused across statements) — chsql's
//     collectGroupByFrags binds its args ONCE, to e.args, on the promise
//     the returned Frag's SQL text is spliced EXACTLY once; this emitter's
//     shape needs the group key in at least three independent statements
//     (the real arm, canon-by-series, and — before this file's own
//     review fix — a second read of each via the bound's two independent
//     reads), so collectGroupByFrags's contract does not hold here. Each
//     site instead renders r.GroupBy[i] through a fresh closure calling
//     Builder.Expr directly (mirroring groupKeyFrags /
//     emitRangeWindowMetrics's own group-by rendering), which re-derives
//     its own correctly-paired args on every independent invocation.
//     GROUP BY clauses reference the alias the SAME statement's SELECT
//     list just bound (Col(alias), needing no second render) — confirmed
//     against real ClickHouse that GROUP BY resolves to the SELECT-list
//     alias even when it collides with a same-named raw FROM column
//     (mapApply-stripped alias correctly collapsed two distinct raw Map
//     values into one group), so this is not the alias-shadowing hazard
//     it might look like.
//   - Bucket-layout reprojection (levels 2-4) always runs, exactly as
//     RangeBucketGridNative's own rung-unnest always runs: a standalone
//     layout-stability probe was measured to cost nearly as much as just
//     doing the reprojection while buying nothing actionable (Task 1
//     point 3).
//   - The canonical bound set (level 2) is a per-SERIES superset — every
//     bound the series reported ANYWHERE in the bounded scan range, not
//     just in one anchor's own window — computed ONCE via an ordinary
//     GROUP BY (linear in row count) and JOINed onto every row of that
//     series, rather than as a per-row whole-partition window aggregate
//     (which would materialise the R-row array into every one of a
//     partition's R rows: O(rows^2) per series — a real quadratic blow-up
//     a code review caught before this shipped). Level 4's has-gated
//     cumulative/presence pair is what narrows this per-series superset
//     back down to the per-(series, anchor) set the fan-out's own
//     classicBucketUnionBoundsExpr computes directly: a bound the
//     WINDOW's own rows never reported gets presence 0 at that anchor
//     (regardless of whether some OTHER anchor's window in the same
//     series reported it) and level 6/7 filters it back out. This mirrors
//     bucketGridSeenFn's doc in range_bucket_grid_native.go: fabricating
//     a rung the window never carried is not harmless — the monotonic
//     envelope a naive reprojection applies would otherwise MOVE the
//     interpolation's bounds and change the answered quantile.
//   - Contribution rule. A row contributes to canonical bound rk's
//     cumulative sum ONLY when its OWN ExplicitBounds contains rk
//     EXACTLY (indexOf-based has-gating) — never via a monotone-envelope
//     "nearest own bound >= rk" reprojection. Reference PromQL models
//     each `le` value as its OWN scalar time series
//     (`<name>_bucket{le="X"}`); a row whose layout never reported X
//     contributes no sample to that series at all, so summing in the
//     cumulative domain with an exact-membership gate — THEN differencing
//     the SUMMED (not the per-row) ladder — is what reference's
//     `sum_over_time` actually computes. Differencing before summing (an
//     earlier version of this file did that) is only equivalent when
//     every row contributes at every position, which exact-membership
//     gating deliberately breaks.
//   - The +Inf rung is exempt from the has-gate by construction, not by a
//     special case: every row (real or sentinel) has `inf` appended to
//     its own bounds list before the indexOf lookup, so
//     `indexOf(ownBoundsInf, inf)` always resolves (to the row's own
//     total via arraySum for a real row, to index 1 of `[inf]` giving 0
//     for a sentinel) — matching reference's own "the +Inf rung folds
//     unconditionally over the whole window" rule.
//   - Level 5 uses exactly ONE window function, not two or three. A
//     second `sum(...) OVER (<identical PARTITION BY/ORDER BY/RANGE>)`
//     was the first shape tried for the MinSamples count, and it
//     reproduces a genuine ClickHouse bug when combined with this bound
//     source's two independent reads: `Code: 10. DB::Exception: Not found
//     column __table2._rws_reproj in block sumCount(__table2._rws_is_anchor)
//     ... (NOT_FOUND_COLUMN_IN_BLOCK)`, confirmed against real chDB.
//     Concatenating the marker (and, after the B2 fix, the presence
//     array too) onto the SAME array sumForEach already sums needs only
//     the one window function and sidesteps the bug entirely.
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
	// baseNS keeps every ms-encoded value non-negative — Task 1's proven
	// formula. Non-negativity is a CORRECTNESS precondition here, not mere
	// convenience: windowSlideMsEncodeFrag's ceil-to-ms trick is
	// `intDiv(x - baseNS + bias, scale)`, and ClickHouse's intDiv truncates
	// TOWARD ZERO — that equals floor (and, with the bias, ceil) only when
	// the dividend `x - baseNS` is non-negative; for a negative dividend
	// truncation rounds the WRONG direction. baseNS = Start-Offset-Range is
	// the first anchor's own lower edge, so BOTH inputs windowSlideMsEncodeFrag
	// ever sees satisfy x > baseNS by construction: real rows, because
	// maybePushRangeScanTimeBound (applied below) filters to exactly
	// `TimeUnix > Start-Offset-Range`; sentinel rows, because their
	// smallest possible encoded position is `Start-Offset` (Range's own
	// `r.Range > 0` validation above makes that strictly greater than
	// `Start-Offset-Range`). Neither holds by luck; both are consequences
	// of code already in this function, restated here so this expression's
	// own precondition is not a bare assertion.
	// the precondition holds for every real invocation, not by luck.
	baseNS := startNS - offsetNS - rangeNS

	inner, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	// groupKeyFrag returns a FRESH Frag on every call — see the emit doc's
	// "GROUP-BY / JOIN key rendering" note for why a single shared Frag
	// (chsql's usual collectGroupByFrags) is unsafe for this node's shape.
	groupKeyFrag := func(expr chplan.Expr) Frag {
		return func(b *Builder) { _ = b.Expr(expr) }
	}

	canonKeyAliases := make([]string, len(r.GroupByAliases))
	for i := range r.GroupByAliases {
		canonKeyAliases[i] = fmt.Sprintf("%s%d", rwsCanonKeyAliasPrefix, i)
	}

	// Level 2 — per-series canonical bound set: an ORDINARY (non-windowed)
	// GROUP BY, one row per series, linear in the bounded scan's row
	// count. Doubles as this node's discovery arm (one row per series
	// that has >= 1 row in the bounded scan) — the sentinel arm below
	// reads its keys directly from here instead of a separate `SELECT
	// DISTINCT` pass.
	canonBySeries := NewQuery().From(inner)
	for i := range r.GroupBy {
		canonBySeries.SelectAs(groupKeyFrag(r.GroupBy[i]), canonKeyAliases[i])
	}
	canonBySeries.SelectAs(
		Call("arraySort", Call("arrayDistinct", Call("arrayFlatten",
			Call("groupArray", Call("arrayPushBack", toFloat64ArrFrag(rwsBoundParam, Col(r.ExplicitBoundsCol)), BareIdent("inf")))))),
		rwsCanonBoundsAlias,
	)
	canonGroupBy := make([]Frag, len(canonKeyAliases))
	for i, a := range canonKeyAliases {
		canonGroupBy[i] = Col(a)
	}
	canonBySeries.GroupBy(canonGroupBy...)
	maybePushRangeScanTimeBound(canonBySeries, r.TimestampCol, r.Start, r.End, offsetNS, rangeNS)

	// Level 1 — real sample rows, ts unshifted (see the Offset note
	// above). BucketCounts/ExplicitBounds are cast to Array(Float64) here
	// so both UNION ALL arms carry identical column types.
	rawReal := NewQuery().From(inner)
	for i := range r.GroupBy {
		rawReal.SelectAs(groupKeyFrag(r.GroupBy[i]), r.GroupByAliases[i])
	}
	rawReal.SelectAs(windowSlideMsEncodeFrag(baseNS, Call("toUnixTimestamp64Nano", Col(r.TimestampCol))), rwsRawNsAlias)
	rawReal.SelectAs(toFloat64ArrFrag(rwsCountParam, Col(r.BucketCountsCol)), rwsCountsFAlias)
	rawReal.SelectAs(toFloat64ArrFrag(rwsBoundParam, Col(r.ExplicitBoundsCol)), rwsBoundsFAlias)
	rawReal.SelectAs(InlineLit(int64(0)), rwsIsAnchorAlias)
	// Cast to Int64 explicitly, matching anchorNsArrayFrag's own cast: the
	// bare literal 0 infers as UInt8, and left uncast this placeholder
	// would widen rwsAnchorNsAlias to a Variant(UInt8, Int64) column across
	// the UNION ALL — see anchorNsArrayFrag's doc for why that is fatal
	// (fromUnixTimestamp64Nano needs a concrete Int64, not a Variant).
	rawReal.SelectAs(Call("toInt64", InlineLit(int64(0))), rwsAnchorNsAlias)
	maybePushRangeScanTimeBound(rawReal, r.TimestampCol, r.Start, r.End, offsetNS, rangeNS)

	// Level 1b — JOIN each real row onto its series' canon-by-series row
	// to attach rwsCanonBoundsAlias. Both sides read the SAME bounded scan
	// (rawReal's own WHERE, and canonBySeries's own maybePushRangeScanTimeBound
	// above), so every series rawReal carries has a matching row here —
	// INNER JOIN is lossless.
	real := NewQuery().From(aliasedFrag(rawReal.Frag(), "u"))
	for _, a := range r.GroupByAliases {
		real.Select(As(Qual("u", a), a))
	}
	real.Select(As(Qual("u", rwsRawNsAlias), rwsRawNsAlias))
	real.Select(As(Qual("u", rwsCountsFAlias), rwsCountsFAlias))
	real.Select(As(Qual("u", rwsBoundsFAlias), rwsBoundsFAlias))
	real.Select(As(Qual("u", rwsIsAnchorAlias), rwsIsAnchorAlias))
	real.Select(As(Qual("u", rwsAnchorNsAlias), rwsAnchorNsAlias))
	real.Select(As(Qual("c", rwsCanonBoundsAlias), rwsCanonBoundsAlias))
	realOn := make([]Frag, len(r.GroupByAliases))
	for i, a := range r.GroupByAliases {
		realOn[i] = Eq(Qual("u", a), Qual("c", canonKeyAliases[i]))
	}
	real.Join(InnerJoin, aliasedFrag(canonBySeries.Frag(), "c"), And(realOn...))

	// Level 3a — the two parallel per-anchor axes: the ENCODED position
	// (anchor - Offset, the side that absorbs the membership shift) and
	// the REPORTED position (anchor, unshifted). Reads keys AND
	// rwsCanonBoundsAlias straight from canonBySeries — no join needed on
	// the sentinel side.
	sentinelArrays := NewQuery().From(canonBySeries.Frag())
	for i, a := range r.GroupByAliases {
		sentinelArrays.Select(As(Col(canonKeyAliases[i]), a))
	}
	sentinelArrays.Select(Col(rwsCanonBoundsAlias))
	sentinelArrays.SelectAs(
		msEncodeArrayFrag(baseNS, anchorNsArrayFrag(startNS-offsetNS, stepNS, numAnchors)),
		rwsAnchorNsArrAlias,
	)
	sentinelArrays.SelectAs(anchorNsArrayFrag(startNS, stepNS, numAnchors), rwsAnchorNsArr2Alias)

	// Level 3b — explode the two axes in lockstep (ArrayJoin zips
	// same-length arrays positionally, exactly as
	// emitRangeBucketGridNative's own explode.ArrayJoin does) and pad the
	// real arm's columns with each sentinel's empty/marker values. Column
	// order MUST match real's SELECT list above (positional UNION ALL).
	sentinel := NewQuery().From(sentinelArrays.Frag())
	for _, a := range r.GroupByAliases {
		sentinel.Select(Col(a))
	}
	sentinel.Select(Col(rwsRawNsAlias))
	sentinel.SelectAs(Call("emptyArrayFloat64"), rwsCountsFAlias)
	sentinel.SelectAs(Call("emptyArrayFloat64"), rwsBoundsFAlias)
	sentinel.SelectAs(InlineLit(int64(1)), rwsIsAnchorAlias)
	sentinel.Select(Col(rwsAnchorNsAlias))
	sentinel.Select(Col(rwsCanonBoundsAlias))
	sentinel.ArrayJoin(
		As(Col(rwsAnchorNsArrAlias), rwsRawNsAlias),
		As(Col(rwsAnchorNsArr2Alias), rwsAnchorNsAlias),
	)

	// Level 3 — UNION ALL, bounded on its own cost axis
	// (scanned_rows + series x anchors) — reuses lwrFanoutBoundedSourceFrag
	// (internal/chsql/lwr_fanout_bound.go) directly rather than a
	// second copy of its two-independent-reads LIMIT+throwIf shape; see
	// maxRangeBucketWindowSlideRows's own doc for the calibration note and
	// range_bucket_window_slide_bound.go for why this bound source is NOT
	// short-circuiting the way the pure-arrayJoin fan-out's is (a real,
	// documented trade-off, not an oversight).
	unionSrc := lwrFanoutBoundedSourceFrag(
		Paren(UnionAll(real.Frag(), sentinel.Frag())), rwsRawNsAlias,
		maxRangeBucketWindowSlideRows, RangeBucketWindowSlideBudgetMessage,
	)

	// Level 4 — per-row has-gated cumulative reading + presence indicator
	// + MinSamples marker, concatenated into one array. See the emit doc's
	// "Contribution rule" note for why has-gating (exact bound membership)
	// replaces the monotone-envelope reprojection an earlier version used.
	reproj := NewQuery().From(unionSrc)
	for _, a := range r.GroupByAliases {
		reproj.Select(Col(a))
	}
	reproj.Select(Col(rwsRawNsAlias))
	reproj.Select(Col(rwsIsAnchorAlias))
	reproj.Select(Col(rwsAnchorNsAlias))
	reproj.Select(Col(rwsCanonBoundsAlias))
	reproj.SelectAs(hasGatedExtendedArrayFrag(), rwsExtendedAlias)

	// Level 5 — the ONE sliding window aggregate.
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
		WindowFrame(Call("sumForEach", Col(rwsExtendedAlias)), partitionByFrags(r.GroupByAliases), orderBy, frame),
		rwsWindowedExtAlias,
	)

	// Level 6 — split rwsWindowedExtAlias into its three signals and
	// narrow the per-series canonical bound set down to what THIS
	// anchor's own window actually reported (sum-presence >= 1) — see the
	// emit doc's B2 note.
	canonLen := Call("length", Col(rwsCanonBoundsAlias))
	sumCum := Call("arraySlice", Col(rwsWindowedExtAlias), InlineLit(int64(1)), canonLen)
	sumPresence := Call("arraySlice", Col(rwsWindowedExtAlias), Add(canonLen, InlineLit(int64(1))), canonLen)
	markerExpr := Subscript(Col(rwsWindowedExtAlias), Add(Mul(InlineLit(int64(2)), canonLen), InlineLit(int64(1))))

	split := NewQuery().From(windowed.Frag())
	split.SelectAs(Call("fromUnixTimestamp64Nano", Col(rwsAnchorNsAlias)), r.AnchorAlias)
	for _, a := range r.GroupByAliases {
		split.Select(Col(a))
	}
	split.Select(Col(rwsIsAnchorAlias))
	split.SelectAs(
		Call("arrayFilter", Lambda2(rwsFilterValueParam, rwsFilterPresenceParm,
			Gte(BareIdent(rwsFilterPresenceParm), InlineLit(1.0))), sumCum, sumPresence),
		rwsFilteredCumAlias,
	)
	split.SelectAs(
		Call("arrayFilter", Lambda2(rwsFilterValueParam, rwsFilterPresenceParm,
			Gte(BareIdent(rwsFilterPresenceParm), InlineLit(1.0))), Col(rwsCanonBoundsAlias), sumPresence),
		rwsFilteredBoundsAlias,
	)
	split.SelectAs(markerExpr, rwsRealCountAlias)

	// Level 7 — difference the narrowed, windowed cumulative ladder back
	// into per-bucket counts, strip +Inf back out of the reported
	// ExplicitBounds, and keep only the anchor rows whose window covers
	// at least windowSlideMinSamples real samples.
	final := NewQuery().From(split.Frag())
	final.Select(Col(r.AnchorAlias))
	for _, a := range r.GroupByAliases {
		final.Select(Col(a))
	}
	final.SelectAs(differencedLadderFrag(Col(rwsFilteredCumAlias)), r.BucketCountsCol)
	final.SelectAs(Call("arrayFilter",
		Lambda1(rwsBoundParam, Call("isFinite", BareIdent(rwsBoundParam))),
		Col(rwsFilteredBoundsAlias)), r.ExplicitBoundsCol)
	final.Where(And(
		Eq(Col(rwsIsAnchorAlias), InlineLit(int64(1))),
		Gte(Col(rwsRealCountAlias), InlineLit(float64(windowSlideMinSamples))),
	))

	return e.emitSelect(final)
}

// partitionByFrags renders each of aliases as a bare Col Frag — the
// sliding window's PARTITION BY key list.
func partitionByFrags(aliases []string) []Frag {
	out := make([]Frag, 0, len(aliases))
	for _, a := range aliases {
		out = append(out, Col(a))
	}
	return out
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
//
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

// hasGatedExtendedArrayFrag renders one row's own contribution to every
// canonical bound — EXACT membership only, never a monotone-envelope
// "nearest own bound" reprojection (see the emit function doc's
// "Contribution rule" note) — concatenated with the same row's presence
// indicator and its windowSlideMinSamples marker:
//
//	ownBoundsInf = arrayPushBack(_rws_bounds_f, inf)
//	ownCum       = arrayPushBack(arrayCumSum(arraySlice(_rws_counts_f, 1, length(_rws_bounds_f))),
//	                             arraySum(_rws_counts_f))
//	idx(rk)      = indexOf(ownBoundsInf, rk)
//	cumArr       = arrayMap(rk -> if(idx(rk) > 0, ownCum[idx(rk)], 0.0), _rws_canon_bounds)
//	presenceArr  = arrayMap(rk -> if(idx(rk) > 0, 1.0, 0.0), _rws_canon_bounds)
//	arrayConcat(cumArr, presenceArr, [toFloat64(1 - _rws_is_anchor)])
//
// ownCum is the row's own CUMULATIVE-at-bound reading (the same
// monotone-envelope construction range_bucket_grid_native.go's
// bucketGridRungsFrag uses for the identical reason: OTLP's ExplicitBounds
// contract makes a prefix sum over the row's own counts exactly its
// cumulative reading at each of ITS OWN bounds). indexOf resolves to 0
// (no match) for any canonical bound rk this row's own layout never
// reported exactly — cumArr/presenceArr both read 0 there, so that row
// contributes NOTHING to rk's window sum, matching reference PromQL's
// per-`le` scalar-series model (a row whose layout omits `le=rk` has no
// SAMPLE for the `<name>_bucket{le=rk}` series at all).
//
// ownBoundsInf's trailing +Inf guarantees indexOf always finds a match on
// the +Inf position (every row, real or sentinel, has `inf` appended), so
// the +Inf rung folds unconditionally over every row exactly as reference
// requires — see the emit doc's own +Inf note.
//
// A sentinel row's own _rws_counts_f/_rws_bounds_f are both empty:
// ownBoundsInf degenerates to `[inf]`, ownCum to `[arraySum([])] = [0.0]`;
// indexOf matches ONLY the +Inf position (contributing 0), never any
// finite canonical bound — so a sentinel contributes nothing anywhere but
// its own trailing marker slot (0, since is_anchor=1).
func hasGatedExtendedArrayFrag() Frag {
	countsF := Col(rwsCountsFAlias)
	boundsF := Col(rwsBoundsFAlias)
	ownBoundsInf := Call("arrayPushBack", boundsF, BareIdent("inf"))
	ownCum := Call("arrayPushBack",
		Call("arrayCumSum", Call("arraySlice", countsF, InlineLit(int64(1)), Call("length", boundsF))),
		Call("arraySum", countsF))
	idx := func() Frag { return Call("indexOf", ownBoundsInf, BareIdent(rwsCanonParam)) }
	cumArr := Call("arrayMap",
		Lambda1(rwsCanonParam, If(Gt(idx(), InlineLit(int64(0))), Subscript(ownCum, idx()), InlineLit(0.0))),
		Col(rwsCanonBoundsAlias))
	presenceArr := Call("arrayMap",
		Lambda1(rwsCanonParam, If(Gt(idx(), InlineLit(int64(0))), InlineLit(1.0), InlineLit(0.0))),
		Col(rwsCanonBoundsAlias))
	marker := Call("array", Call("toFloat64", Sub(InlineLit(int64(1)), Col(rwsIsAnchorAlias))))
	return Call("arrayConcat", cumArr, presenceArr, marker)
}

// differencedLadderFrag renders `counts[i] = ladder[i] - ladder[i-1]`, with
// the first rung passing through unchanged — the same round trip out of the
// cumulative domain internal/promql's classicBucketWindowCountsExpr
// performs, and internal/chsql's own bucketGridDifferenceFrag
// (range_bucket_grid_native.go) mirrors for its sibling node. ladder is
// referenced multiple times in the rendered body; callers pass a cheap
// Col(...) reference (never a re-computed expression) — see the emit
// function's own level split, which materialises rwsFilteredCumAlias as
// its own column for exactly this reason.
func differencedLadderFrag(ladder Frag) Frag {
	pos := BareIdent(rwsLadderParam)
	return Call("arrayMap",
		Lambda1(rwsLadderParam, If(
			Eq(pos, InlineLit(int64(1))),
			Subscript(ladder, pos),
			Sub(Subscript(ladder, pos), Subscript(ladder, Sub(pos, InlineLit(int64(1))))),
		)),
		Call("arrayEnumerate", ladder))
}

package chsql

import (
	"fmt"
	"strconv"

	"github.com/tsouza/cerberus/internal/chplan"
)

// fanoutNoMinSampleFilter is the largest RangeBucketFanout.MinSamples that
// needs no HAVING: an anchor with zero samples in its window already
// contributes no fanned row, so "at least one" is free.
const fanoutNoMinSampleFilter = 1

// rangeBucketFanoutGroupKeyAlias names the synthetic fan-out-stage column
// [emitRangeBucketFanout] materializes for the i-th ALIASED GroupBy entry —
// see the fan-out SELECT's own doc comment (issue #3551) for why. Positional
// rather than content-derived because RangeBucketFanout.GroupBy is a small
// list scoped to one node's own SELECT, so no two entries in the same
// fan-out can collide on it, and the `_rbf_`-prefixed family this joins
// (rangeBucketFanoutGroupGuardedQuery's CTE name, foldCostElemsAlias,
// foldCostSamplesAlias) already establishes that prefix as reserved to this
// emitter.
func rangeBucketFanoutGroupKeyAlias(i int) string {
	return "_rbf_key_" + strconv.Itoa(i+1)
}

// emitRangeBucketFanout renders a chplan.RangeBucketFanout — the
// single-pass, bounded sample-side fan-out that supersedes the StepGrid
// CROSS JOIN + per-anchor lookback Filter + per-(series, anchor)
// Aggregate shape for the array-valued histogram-quantile /
// histogram-value-function range lowerings. It is the array-aggregate
// sibling of emitRangeLWR and reuses lwrAnchorFanoutFrag verbatim for the
// bounded anchor fan-out.
//
// SQL skeleton (N = (End-Start)/Step + 1 grid anchors, but the
// intermediate cardinality is rows × (Lookback/Step + 1), constant in N):
//
//	SELECT <user-key_1> AS <alias_1>, …, anchor_ts AS <AnchorAlias>,
//	       <resolve(AggFuncs[i].Fn)>(<args>) AS <AggFuncs[i].Alias>, …
//	FROM (
//	  SELECT *,
//	         arrayJoin(arrayMap(i -> <grid_base> - toIntervalNanosecond(i * <stepNS>),
//	                   range(greatest(0, floorIdx(dist - lookback)),
//	                         least(<N>, floorIdx(dist))))) AS anchor_ts
//	  FROM (<Input>)
//	)
//	GROUP BY <user-key_1>, …, anchor_ts
//	HAVING uniqExact(<TimestampCol>) >= <MinSamples>   -- MinSamples > 1 only
//
// where `dist = dateDiff('nanosecond', TimeUnix, <shift_base>)` is the
// sample's distance behind the newest OFFSET-SHIFTED anchor (identical to
// emitRangeLWR's window math). Each sample fans to only the
// ≤ Lookback/Step + 1 anchors whose half-open staleness window
// `(anchor - Offset - Lookback, anchor - Offset]` contains it; the
// `GROUP BY (<user-keys>, anchor_ts)` then collapses each (series,
// anchor) bucket with the configured AggFuncs. An anchor with no sample
// in its window receives no fanned row and so produces no GROUP BY row —
// preserving Prom's staleness gap, exactly as the old CROSS JOIN +
// lookback Filter did. RangeBucketFanout.MinSamples raises that floor for
// the `rate` / `increase` idiom, which needs two scrapes in the window
// before reference PromQL emits anything at an anchor.
//
// The fanout SELECT projects `*` so the inner Input's columns (the
// AggFunc source columns + the group-key source columns + TimeUnix) flow
// through unchanged to the collapse SELECT; the only added column is the
// computed `anchor_ts`. The collapse SELECT keeps the anchor under its
// own `anchor_ts` alias (no re-alias to TimestampCol) so an
// `argMax(<col>, TimeUnix)` AggFunc resolves its TimeUnix argument to the
// inner per-sample source column rather than a same-SELECT output alias —
// the same alias-shadowing trap emitRangeLWR documents. Because no
// AggFunc output alias collides with TimestampCol, no outer re-alias
// Project is needed; the wrapping chplan Project (added by the lowering)
// re-aliases anchor_ts → TimeUnix downstream.
//
// When r.OuterRange > 0 (cerberus issue #2726) the grid instead derives
// from (End, OuterRange, Step) — [End-OuterRange, End] spaced by Step,
// end-inclusive, with StepAlign's epoch-floor snap when set — mirroring
// [emitWindowedArrayMatrix]'s identical OuterRange arithmetic; see the
// branch below for the shift/unshift split that preserves.
func (e *emitter) emitRangeBucketFanout(r *chplan.RangeBucketFanout) error {
	if r.Step <= 0 {
		return fmt.Errorf("%w: RangeBucketFanout requires Step > 0", ErrUnsupported)
	}
	if r.Input == nil {
		return fmt.Errorf("%w: RangeBucketFanout.Input is nil", ErrUnsupported)
	}
	if r.TimestampCol == "" {
		return fmt.Errorf("%w: RangeBucketFanout requires TimestampCol", ErrUnsupported)
	}
	inputTimestamp, err := timestampChildColumn("RangeBucketFanout", r.Input)
	if err != nil {
		return err
	}
	if r.AnchorAlias == "" {
		return fmt.Errorf("%w: RangeBucketFanout requires AnchorAlias", ErrUnsupported)
	}
	if len(r.AggFuncs) == 0 {
		return fmt.Errorf("%w: RangeBucketFanout requires at least one AggFunc", ErrUnsupported)
	}

	stepNS := r.Step.Nanoseconds()
	lookbackNS := r.Lookback.Nanoseconds()

	numAnchors, gridBase, shiftBase, err := rangeBucketFanoutGrid(r, stepNS)
	if err != nil {
		return err
	}

	inner, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}

	tsIdent := func(b *Builder) { b.Ident(inputTimestamp) }

	// Sample-fanout SELECT: pass through every Input column (`*`) and add
	// the bounded grid anchor. `*` is required so the AggFunc source
	// columns + group-key source columns + TimeUnix all reach the collapse
	// SELECT without enumerating the (schema-dependent) column set here.
	//
	// Built twice from the same parts: `fanout` is the read the collapse
	// consumes, `probeFanout` is the row-count probe's copy of it (see
	// lwrFanoutBoundedSourceFrag). The probe only counts fanned rows, so it
	// carries none of the hoisted group-key materialisations below — they
	// are per-row projections that change no count, and leaving them out is
	// what keeps each hoisted key's expression rendered ONCE in the
	// statement rather than once per embedded copy of the fan-out.
	anchorFrag := lwrAnchorFanoutFrag(gridBase, shiftBase, tsIdent, stepNS, lookbackNS, numAnchors)
	newFanout := func() *QueryBuilder {
		sb := NewQuery().From(inner)
		sb.Select(Star())
		sb.Select(RawAs(anchorFrag, r.AnchorAlias))
		return sb
	}
	fanout := newFanout()
	probeFanout := newFanout()

	// Issue #3551: materialize every ALIASED GroupBy entry here too, under
	// its own synthetic per-index column ([rangeBucketFanoutGroupKeyAlias]),
	// instead of leaving collapse's own SELECT-list as the first (and only)
	// place that expression is ever rendered. ClickHouse 24.8 mis-resolves
	// a GROUP BY that references a SAME-SELECT alias standing for a
	// lambda-heavy expression (mapSort/mapConcat/mapFilter over the merged
	// series-identity Map — [histogramIdentityExpr]'s shape) once the query
	// nests six-plus subqueries deep: its aggregate-coverage check
	// re-derives the GROUP BY key from the alias and, at that depth, fails
	// to recognize the re-derived form as the same expression the
	// SELECT-list rendered (verified against a live 24.8 container; see
	// issue #3551 for the exact NOT_AN_AGGREGATE trace). Every
	// RangeBucketFanout composition with a groupArray-family AggFunc
	// (rangeBucketFanoutHasGrowingAccumulator) reaches exactly that depth
	// via [rangeBucketFanoutGroupGuardedQuery]'s CTE wrap, so this
	// materializes unconditionally rather than only for the guarded shape —
	// a future composition need not rediscover the same depth to hit it.
	// Moving the expression down turns collapse's own SELECT + GROUP BY
	// entries for that key into a bare column reference — the same trivial
	// column-rename shape no report has ever shown CH mis-resolving —
	// while the expression's TEXT still renders exactly once in the whole
	// query, same as the alias-reference form it replaces: no new
	// duplication for the emitted-SQL size bound (issue #2733) to absorb.
	// "Once" holds only because the hoist goes into `fanout` alone and not
	// into `probeFanout`: the fan-out is embedded twice by the row bound
	// below, and a hoist rendered into both copies doubles every aliased
	// key's expression — measured at +6,368 placeholder bytes on the
	// level-2 mixed subquery composition, enough to push that statement
	// past ClickHouse's default max_query_size once its args are inlined.
	hoistedGroupKeys := make([]string, len(r.GroupBy))
	for i, g := range r.GroupBy {
		if i >= len(r.GroupByAliases) || r.GroupByAliases[i] == "" {
			continue
		}
		expr := g
		hoisted := rangeBucketFanoutGroupKeyAlias(i)
		fanout.Select(RawAs(func(b *Builder) { _ = b.Expr(expr) }, hoisted))
		hoistedGroupKeys[i] = hoisted
	}

	// Prune the inner scan to the offset-shifted half-open grid span
	// `(Start - Offset - Lookback, End - Offset]` before the SELECT-list
	// arrayJoin fans each source row across its anchors — same granule-
	// prune contract as emitRangeLWR. Gated on Start/End so the
	// now64()/@-pinned/zero-grid fixtures stay byte-identical. The probe
	// reads the same pruned span, or its count would not be the read's.
	maybePushRangeScanTimeBound(fanout, inputTimestamp, r.Start, r.End, r.Offset.Nanoseconds(), lookbackNS)
	maybePushRangeScanTimeBound(probeFanout, inputTimestamp, r.Start, r.End, r.Offset.Nanoseconds(), lookbackNS)

	// #2447: cap how many (series, anchor) fanout rows can ever reach the
	// collapse GROUP BY below via a genuine LIMIT + truncation probe — that
	// GROUP BY is a blocking operator (it cannot emit any group until it
	// has consumed the entire fanned-out input), which some callers'
	// AggFuncs (classicBucketWindowAggs' groupArray trio, in particular)
	// accumulate over rather than reduce to a fixed size. See
	// lwr_fanout_bound.go's own doc comment for the full history and the
	// real calibration numbers.
	// #2667: e.rangeBucketFanoutRowBound() resolves the operator override
	// (or maxRangeBucketFanoutRows's own default) once per Emit call.
	fanoutSource := lwrFanoutBoundedSourceFrag(fanout.Frag(), probeFanout.Frag(), inputTimestamp, e.rangeBucketFanoutRowBound(), RangeBucketFanoutBudgetMessage)

	// Collapse SELECT: GROUP BY (<user-keys>, anchor) with the configured
	// AggFuncs. The user group keys are projected first (under their
	// aliases) then the anchor, matching the column order the replaced
	// Aggregate node emitted (anchor_ts came first there, but the
	// downstream reshape Project references every column by name, not
	// position, so the surface order is observationally identical).
	collapse := NewQuery().From(fanoutSource)
	collapse.Select(As(verbatim(r.AnchorAlias), r.AnchorAlias))
	for i, g := range r.GroupBy {
		alias := ""
		if i < len(r.GroupByAliases) {
			alias = r.GroupByAliases[i]
		}
		if hoisted := hoistedGroupKeys[i]; hoisted != "" {
			// The expression already rendered once, in the fan-out SELECT
			// above; this is a bare passthrough of that column under its
			// GroupByAliases name, not a second rendering of the expression.
			collapse.SelectAs(Col(hoisted), alias)
			continue
		}
		expr := g
		collapse.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	for _, af := range r.AggFuncs {
		af := af
		collapse.Select(aggFuncFrag(af))
	}

	// GROUP BY (anchor_ts, <user keys>). The anchor is referenced verbatim
	// because it is the fanout SELECT's output column, not a base-table
	// column. A hoisted key groups by its own fan-out column directly
	// (see the materialization above) rather than by the GroupByAliases
	// name [groupKeyFrags] would otherwise reference — collapse's
	// SELECT-list entry for that key is now a bare passthrough of the same
	// column, so the two name the identical value. An un-aliased key still
	// re-renders its raw expression, matching [groupKeyFrags]'s existing
	// fallback for that case.
	groupFrags := make([]Frag, 0, len(r.GroupBy)+1)
	groupFrags = append(groupFrags, verbatim(r.AnchorAlias))
	for i, g := range r.GroupBy {
		if hoisted := hoistedGroupKeys[i]; hoisted != "" {
			groupFrags = append(groupFrags, Col(hoisted))
			continue
		}
		expr := g
		groupFrags = append(groupFrags, func(b *Builder) { _ = b.Expr(expr) })
	}
	collapse.GroupBy(groupFrags...)

	// Per-function "no sample emitted" rule. An anchor whose window holds
	// fewer than MinSamples distinct sample timestamps produces no row at
	// all — reference PromQL's rate/increase need two points to span a
	// delta and emit nothing at the leading edge of a range where only one
	// scrape has landed. Distinct timestamps rather than raw row count: the
	// group may hold several series that share a scrape instant, and the
	// rule is about how many scrapes the window spans, not how many rows.
	if r.MinSamples > fanoutNoMinSampleFilter {
		collapse.Having(Gte(Call("uniqExact", Col(inputTimestamp)), InlineLit(int64(r.MinSamples))))
	}

	// Issue #3468: a SECOND, independent bound — this one on the collapse
	// OUTPUT's total fold cost, not the pre-collapse sample fanout
	// `fanoutSource` above already caps. Scoped to collapses whose AggFuncs
	// include a groupArray-family accumulator (classicBucketWindowAggs,
	// expHistogramWindowAggs — see maxRangeBucketFanoutFoldCostUnits' own
	// doc for why): an argMax/sumForEach collapse reduces every group to a
	// FIXED-size row regardless of group count, the same already-accepted
	// risk class as any ordinary Aggregate, so gating this guard on the
	// accumulator shape (rather than applying it unconditionally to every
	// RangeBucketFanout) keeps a wide, cheap, legitimately safe dashboard
	// query — thousands of anchors x series, fixed-size per-group state —
	// from a false-positive rejection this bound was never calibrated to
	// police.
	if rangeBucketFanoutHasGrowingAccumulator(r.AggFuncs) {
		payload, samples, err := rangeBucketFanoutFoldCostAliases(r.AggFuncs)
		if err != nil {
			return err
		}
		return e.emitSelect(rangeBucketFanoutGroupGuardedQuery(
			e, collapse, payload, samples, e.rangeBucketFanoutFoldCostBound(), RangeBucketFanoutGroupBudgetMessage,
		))
	}

	return e.emitSelect(collapse)
}

// rangeBucketFanoutGrid resolves the anchor count and the two grid bases for
// the fan-out: gridBase is the unshifted anchor the output reports, shiftBase
// the offset-shifted membership base the window arithmetic keys off.
func rangeBucketFanoutGrid(r *chplan.RangeBucketFanout, stepNS int64) (numAnchors int64, gridBase, shiftBase Frag, err error) {
	// Membership base (offset-shifted newest anchor) and value base
	// (unshifted grid anchor). Offset folds onto the membership base only.
	// Reassigned below in the (Start, End) branch to the Start-anchored
	// grid end — see [startAnchoredGridEnd].
	shiftBase = offsetShiftedBaseFrag(timeOrNowFrag(r.End), r.Offset)
	if r.OuterRange > 0 {
		// Independent-subquery-grid mode (cerberus issue #2726): the anchor
		// grid is derived from (End, OuterRange, Step) — mirrors
		// emitWindowedArrayMatrix's own OuterRange arithmetic exactly,
		// including StepAlign's epoch-floor snap, rather than the
		// (Start, End) span above. shiftBase is ALREADY the offset-shifted
		// base stepAlignGridFor expects to align; the aligned result IS the
		// membership base used below, and the reported gridBase un-shifts it
		// back by Offset — the same shift/unshift split the (Start, End)
		// branch keeps below, just applied AFTER alignment instead of
		// before.
		numAnchors = r.OuterRange.Nanoseconds()/stepNS + 1
		shiftBase, numAnchors = stepAlignGridFor(r.StepAlign, shiftBase, r.End, r.Offset, r.OuterRange, stepNS, numAnchors)
		gridBase = shiftBase
		if r.Offset != 0 {
			gridBase = offsetUnshiftAnchorFrag(shiftBase, r.Offset.Nanoseconds())
		}
	} else {
		// End-inclusive anchor count across the [Start, End] grid. When the
		// grid bounds are absent (the now64(9) fixture shape) a single
		// anchor is the only deterministic choice; the bounded fanout still
		// applies.
		numAnchors = 1
		if !r.Start.IsZero() && !r.End.IsZero() {
			span := r.End.Sub(r.Start).Nanoseconds()
			if span < 0 {
				return 0, nil, nil, fmt.Errorf("%w: RangeBucketFanout.Start > End", ErrUnsupported)
			}
			numAnchors = span/stepNS + 1
		}
		gridEnd := startAnchoredGridEnd(r.Start, r.End, stepNS, numAnchors)
		shiftBase = offsetShiftedBaseFrag(timeOrNowFrag(gridEnd), r.Offset)
		gridBase = timeOrNowFrag(gridEnd)
	}

	return numAnchors, gridBase, shiftBase, nil
}

// rangeBucketFanoutGroupGuardedQuery wraps collapse in the SAME LIMIT-plus-
// independent-truncation-probe shape lwrFanoutBoundedSourceFrag renders, but
// through a named CTE rather than embedding collapse's own SQL text twice.
//
// Issue #3471 (found reviewing #3468's own PR): collapse is not a leaf — for
// a mixed float/histogram composition stacking several range-vector levels,
// each level's own lowering already names its input relation more than once
// (splitMixedRelByDiscriminator's own doc explains why), so collapse can
// ALREADY be a large, several-times-duplicated subtree by the time this
// guard wraps it. lwrFanoutBoundedSourceFrag's literal embedding (bounded +
// an independently re-embedded probe, mirroring rate_window_fanout_bound.go
// / lwr_fanout_bound.go's own established design) is fine at THAT layer
// because it wraps a comparatively small pre-collapse fanout SELECT — but
// applied a second time, here, on top of an already-multiplied collapse, it
// compounds: a real query the emitted-SQL size bound (issue #2733) had
// already proven fits under ClickHouse's max_query_size grew past it
// (test/e2e mixed-subquery composition tests, level 2/query_range:
// 229,839 bytes before this function existed, 402,526+ bytes with the
// literal-embedding version — see this repo's PR history for the numbers).
//
// QueryBuilder.With's own doc names exactly this trade: "buys emitted-TEXT
// linearity and never fewer reads" — collapse's SQL is registered ONCE as a
// CTE and referenced by name from both the guarded read and the probe's
// inner read, so ClickHouse still evaluates it twice (the identical
// execution-cost profile every sibling fanout guard already accepts) while
// the RENDERED TEXT carries it only once. This is deliberately NOT applied
// to lwrFanoutBoundedSourceFrag's own two existing call sites (the
// pre-collapse fanout wrap above, and RangeLWR's identical wrap) — neither
// has ever been observed compounding with an ALREADY-duplicated subtree the
// way this second, later-added guard does, and widening the change risks
// unrelated golden churn for a problem that has not been shown to exist
// there.
//
// Unlike lwrFanoutBoundedSourceFrag the guarded read carries NO `LIMIT`.
// That helper's LIMIT is a real short-circuit: nothing blocking sits between
// it and the scan, so ClickHouse stops pulling once it is satisfied. Here it
// never could be — `collapse` is a GROUP BY, a blocking operator whose whole
// input is consumed before it emits its first row, so a LIMIT above it saves
// nothing and only adds a truncation the probe then has to rule out. The
// probe (rangeBucketFanoutFoldCostProbe) reads the same already-computed
// aggregate result exactly, as a scalar subquery ClickHouse evaluates BEFORE
// the guarded read streams, so the fold downstream of this node still never
// sees a single row of an over-budget collapse.
func rangeBucketFanoutGroupGuardedQuery(
	e *emitter, collapse *QueryBuilder, payloadAliases []string, samplesAlias string, maxCostUnits int64, message string,
) *QueryBuilder {
	cteName := "_rbf_group_" + strconv.Itoa(e.nextCTESeq())
	cteRef := func() Frag { return verbatim(cteName) }

	guarded := NewQuery().With(cteName, collapse.Frag())
	guarded.From(cteRef())
	guarded.Select(Star())
	guarded.Where(lwrFanoutGuardFrag(
		rangeBucketFanoutFoldCostProbe(cteRef, payloadAliases, samplesAlias), maxCostUnits, message,
	))
	return guarded
}

// foldCostElemsAlias / foldCostSamplesAlias name the two per-group
// quantities [rangeBucketFanoutFoldCostProbe]'s inner SELECT derives before
// its outer SELECT sums them into one cost. They are emitter-chosen
// synthetic names in the same `_rbf_`-prefixed family as the CTE above, so
// they cannot collide with a collapse output alias any lowering produces.
const (
	foldCostElemsAlias   = "_rbf_elems"
	foldCostSamplesAlias = "_rbf_samples"
)

// foldCostBytesPerElement is the stored width of one bucket-ladder element —
// `Array(UInt64)` / `Array(Float64)` on every histogram payload column in
// internal/schema's OTel layout — which is what turns ClickHouse's
// `byteSize` (a BYTE count over whatever the accumulators happen to hold)
// into the ELEMENT count maxRangeBucketFanoutFoldCostUnits is calibrated in.
const foldCostBytesPerElement int64 = 8

// foldCostMinSamples floors the per-group sample count the width estimate
// divides by. A collapse group exists only because at least one row fanned
// into it, so `length(<a groupArray>)` is already >= 1 in every group
// ClickHouse can produce; the clamp exists so the emitted arithmetic cannot
// divide by zero even if a future accumulator shape made an empty array
// reachable.
const foldCostMinSamples int64 = 1

// rangeBucketFanoutFoldCostProbe renders the scalar subquery
// [lwrFanoutGuardFrag] compares against the fold-cost ceiling: the sum, over
// every group the collapse produced, of that group's own fold cost in the
// `E + W^2` units maxRangeBucketFanoutFoldCostUnits is calibrated in.
//
//	SELECT sum(`_rbf_elems` + intDiv(`_rbf_elems`, `_rbf_samples`)
//	                       * intDiv(`_rbf_elems`, `_rbf_samples`)) AS `n`
//	FROM (SELECT intDiv(byteSize(<payload aliases…>), 8) AS `_rbf_elems`,
//	             greatest(length(<samples alias>), 1)    AS `_rbf_samples`
//	      FROM <cte>)
//
// E — `_rbf_elems` — is the group's accumulated payload in bucket-ladder
// elements, read straight off the materialised accumulators with `byteSize`
// rather than modelled from the plan. That keeps this bound TYPE-AGNOSTIC:
// it needs no knowledge of which accumulator holds the bucket arrays, which
// is exactly the knowledge chsql does not have and internal/promql would
// have had to thread down to give it.
//
// W — `intDiv(E, S)` with S = `_rbf_samples`, the group's in-window row
// count — recovers the group's bucket-ladder WIDTH from the same two
// numbers, because the accumulators hold one W-wide ladder per in-window
// sample. `length()` over ANY groupArray answers S regardless of what that
// array's elements are, which is why the samples alias may be any one of
// them.
//
// The two-level shape (a per-group projection, then one aggregate over it)
// is what keeps `byteSize` rendered ONCE while the cost expression above it
// reads E three times and S twice.
func rangeBucketFanoutFoldCostProbe(cteRef func() Frag, payloadAliases []string, samplesAlias string) *QueryBuilder {
	payload := make([]Frag, 0, len(payloadAliases))
	for _, alias := range payloadAliases {
		payload = append(payload, Col(alias))
	}

	perGroup := NewQuery().From(cteRef())
	perGroup.Select(As(
		Call("intDiv", Call("byteSize", payload...), InlineLit(foldCostBytesPerElement)),
		foldCostElemsAlias,
	))
	perGroup.Select(As(
		Call("greatest", Call("length", Col(samplesAlias)), InlineLit(foldCostMinSamples)),
		foldCostSamplesAlias,
	))

	width := func() Frag { return Call("intDiv", Col(foldCostElemsAlias), Col(foldCostSamplesAlias)) }
	total := NewQuery().From(perGroup.Frag())
	total.Select(As(Call("sum", Add(Col(foldCostElemsAlias), Mul(width(), width()))), "n"))
	return total
}

// rangeBucketFanoutFoldCostAliases picks the two collapse output aliases
// [rangeBucketFanoutFoldCostProbe] reads: every accumulator's alias, whose
// combined `byteSize` is the group's payload, and one groupArray alias whose
// `length` is the group's in-window row count.
//
// Either alias being unavailable is a hard emit error rather than a
// silently unguarded query: this is a resource bound, and "the plan was
// shaped unusually, so nothing was enforced" is the failure mode a guard
// exists to prevent. Every lowering in this tree already aliases every
// aggregate it puts in a RangeBucketFanout collapse — the downstream reshape
// Projects reference them all by name — so neither branch is reachable from
// a production plan, and returning an error is what pins that.
func rangeBucketFanoutFoldCostAliases(aggFuncs []chplan.AggFunc) (payload []string, samples string, err error) {
	payload = make([]string, 0, len(aggFuncs))
	for _, af := range aggFuncs {
		if af.Alias == "" {
			return nil, "", fmt.Errorf(
				"%w: RangeBucketFanout collapse with a groupArray accumulator requires every AggFunc to carry an Alias", ErrUnsupported,
			)
		}
		payload = append(payload, af.Alias)
		if samples == "" && af.Fn == chplan.FnGroupArray {
			samples = af.Alias
		}
	}
	if samples == "" {
		return nil, "", fmt.Errorf(
			"%w: RangeBucketFanout fold-cost bound requires a groupArray AggFunc to read the group's sample count from", ErrUnsupported,
		)
	}
	return payload, samples, nil
}

// rangeBucketFanoutHasGrowingAccumulator reports whether aggFuncs includes a
// groupArray-family accumulator — the shape whose per-row state GROWS with
// in-window row count (classicBucketWindowAggs' ExplicitBounds/BucketCounts/
// TimeUnix trio, expHistogramWindowAggs' bucket-array + timestamp groupArrays
// — see histogram_quantile_window.go / histogram_quantile_native_window.go),
// as opposed to a FIXED-size accumulator (argMax, sumForEach, sum, count,
// …). See maxRangeBucketFanoutFoldCostUnits' own doc for why only the former
// needs a bound on what the collapse's whole output costs to fold.
func rangeBucketFanoutHasGrowingAccumulator(aggFuncs []chplan.AggFunc) bool {
	for _, af := range aggFuncs {
		if af.Fn == chplan.FnGroupArray {
			return true
		}
	}
	return false
}

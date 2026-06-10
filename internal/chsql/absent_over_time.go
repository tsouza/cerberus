package chsql

import (
	"strconv"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitAbsentOverTime renders the SQL for PromQL `absent_over_time(
// <vector-selector>[<range>])`. Mirrors the instant `absent(...)`
// lowering (see internal/promql/absent.go) but threads a per-anchor
// lookback-window check: emit one row PER STEP ANCHOR whose
// `(anchor - Range, anchor]` window has zero matching samples, with
// the matcher-derived synthesised labels lifted onto the output series.
//
// SQL shape (range mode — Step > 0):
//
//	SELECT '' AS `MetricName`,
//	       <synthesised-attributes-map> AS `Attributes`,
//	       `anchor_ts` AS `TimeUnix`,
//	       toFloat64(?) AS `Value`
//	FROM (
//	    SELECT `anchor_ts`
//	    FROM (
//	        SELECT arrayJoin(arrayMap(i -> '<start>' + i * <step_ns>,
//	                                  range(0, <N>))) AS `anchor_ts`
//	        FROM (SELECT 1)
//	    )
//	    WHERE `anchor_ts` NOT IN (
//	        SELECT arrayJoin(arrayMap(i -> '<start>' + i * <step_ns>,
//	                                  range(<covered-anchor index bounds>))) AS `anchor_ts`
//	        FROM (<matcher-filtered scan>)
//	        WHERE `TimeUnix` > '<global-start - range>'
//	          AND `TimeUnix` <= '<global-end>'
//	    )
//	)
//
// The anchor grid fans out of a synthetic 1-row `SELECT 1` (no scan
// payload), and each scanned sample contributes its ≤ range/step + 1
// COVERED anchors to the NOT IN set via the sample-side forward fanout
// (see absentOverTimeCoveredAnchorFrag) — an anchor is absent exactly
// when no sample's `(anchor - range, anchor]` window contains it, i.e.
// when it is not in the covered set. CH materialises the NOT IN
// subquery once as a hash set, so peak memory is O(anchors + samples).
// The previous shape carried a `groupArray(TimeUnix)` of EVERY sample
// in the global window on each of the N fanned anchor rows and
// arrayCount-scanned it per anchor — the same O(anchors ×
// window_samples) blowup the matrix emitters dropped (run
// 27277793810). An empty scan yields an empty covered set and every
// anchor survives — the desired "absent at every anchor" signal.
//
// SQL shape (instant mode — Step == 0): the previous single-anchor
// structure is kept (one `groupArray` row, one anchor at `<end>`, a
// single arrayCount check) — it is already bounded, with no grid to
// fan across.
//
// The output is the canonical 4-column Sample shape (MetricName,
// Attributes, TimeUnix, Value) so it streams through the cursor and
// matrix pivot like any other PromQL plan. MetricName is always the
// empty string (Prom's funcAbsentOverTime drops `__name__`).
//
// All bound args ride positional `?` placeholders; the inline literals
// (anchor base, step / range / offset in nanoseconds, anchor count)
// are part of the query SHAPE — CH's sort-key pruning needs them
// visible to the planner and parameterising them would force CH to
// re-plan per request.
func (e *emitter) emitAbsentOverTime(a *chplan.AbsentOverTime) error {
	inner, err := e.subqueryFrag(a.Input)
	if err != nil {
		return err
	}

	rangeNS := a.Range.Nanoseconds()
	offsetNS := a.Offset.Nanoseconds()

	// `endFrag` is the upper bookend of the global window (latest
	// anchor); `prefilterStartFrag` is the LOWER bookend the global
	// prefilter uses to bound the inner scan to a range relevant to
	// any anchor's lookback. In range mode that's `a.Start` (the
	// earliest anchor); in instant mode there is only one anchor at
	// `a.End`, so the prefilter's lower bound is `a.End - Range`.
	endFrag := absentOverTimeBookendFrag(a.End, offsetNS)
	prefilterStartFrag := endFrag
	if a.Step > 0 {
		prefilterStartFrag = absentOverTimeBookendFrag(a.Start, offsetNS)
	}

	// Global prefilter `(<prefilterStart> - Range, <end>]` — bounds the
	// matcher scan to timestamps relevant to any anchor's lookback.
	prefilterWhere := And(
		Gt(Col(a.TimestampColumn),
			Sub(prefilterStartFrag, Call("toIntervalNanosecond", InlineLit(rangeNS)))),
		Lte(Col(a.TimestampColumn), endFrag),
	)

	var emptyWindow *QueryBuilder
	if a.Step > 0 {
		// Range mode: anchor grid fanned from a synthetic 1-row source,
		// anti-filtered against the sample-side covered-anchor set. See
		// the function comment for the O(anchors + samples) rationale.
		stepNS := a.Step.Nanoseconds()
		numAnchors := a.End.Sub(a.Start).Nanoseconds()/stepNS + 1
		if numAnchors < 1 {
			numAnchors = 1
		}
		// The arrayJoin fanout walks the step grid starting from
		// `a.Start` (offset-adjusted by prefilterStartFrag); see
		// absentOverTimeAnchorRangeFrag.
		gridSrc := NewQuery().Select(InlineLit(int64(1)))
		fanout := NewQuery().
			From(gridSrc.Frag()).
			Select(As(absentOverTimeAnchorRangeFrag(prefilterStartFrag, stepNS, numAnchors), "anchor_ts"))

		// Covered set: each scanned sample fans to the anchors whose
		// `(anchor - range, anchor]` window contains it.
		covered := NewQuery().
			From(inner).
			Select(As(
				absentOverTimeCoveredAnchorFrag(
					prefilterStartFrag, Col(a.TimestampColumn), stepNS, rangeNS, numAnchors,
				),
				"anchor_ts",
			)).
			Where(prefilterWhere)

		emptyWindow = NewQuery().
			From(fanout.Frag()).
			Select(BareIdent("anchor_ts")).
			Where(notInSubqueryFrag(BareIdent("anchor_ts"), covered.Frag()))
	} else {
		// Instant mode: single anchor at End — already bounded, keep the
		// 1-row groupArray + arrayCount shape.
		//
		// Innermost: groupArray of the per-sample timestamps, prefiltered
		// to the global window. The 1-row Aggregate (no GROUP BY) is
		// emitted directly here rather than going through chplan.Aggregate
		// because we want CH's default 1-row-of-empty-array shape on an
		// empty input (groupArray over no rows = `[]`).
		innermost := NewQuery().
			From(inner).
			Select(As(Call("groupArray", Col(a.TimestampColumn)), "sample_ts_arr")).
			Where(prefilterWhere)

		// Single-anchor projection alongside the 1-row `sample_ts_arr`.
		fanout := NewQuery().
			From(innermost.Frag()).
			Select(As(endFrag, "anchor_ts")).
			Select(BareIdent("sample_ts_arr"))

		// Outer filter: keep the anchor iff its lookback window has zero
		// matching samples. The lambda body is `t > anchor_ts -
		// toIntervalNanosecond(<rangeNS>) AND t <= anchor_ts`.
		windowLambda := Lambda1("t", And(
			Gt(BareIdent("t"),
				Sub(BareIdent("anchor_ts"),
					Call("toIntervalNanosecond", InlineLit(rangeNS)))),
			Lte(BareIdent("t"), BareIdent("anchor_ts")),
		))
		emptyWindow = NewQuery().
			From(fanout.Frag()).
			Select(BareIdent("anchor_ts")).
			Where(Eq(
				Call("arrayCount", windowLambda, BareIdent("sample_ts_arr")),
				InlineLit(int64(0)),
			))
	}

	// Synth Project: re-shape to the canonical Sample 4-column output.
	// MetricName is bound as a `?` placeholder so the driver sees a
	// String column on the wire (Prom drops `__name__` from
	// funcAbsentOverTime). The Attributes map is built from the
	// matcher-derived synth-labels; an empty list renders as the
	// canonical `CAST(map(), 'Map(String,String)')` shape.
	outer := NewQuery().
		From(emptyWindow.Frag()).
		Select(As(Lit(""), a.MetricNameColumn)).
		Select(As(synthAttrsMapFrag(a.SynthLabels), a.AttributesColumn)).
		Select(As(BareIdent("anchor_ts"), a.TimestampColumn)).
		Select(As(Call("toFloat64", Lit(float64(1))), a.ValueColumn))

	e.emitSelect(outer)
	return nil
}

// absentOverTimeBookendFrag returns a Frag rendering the eval-grid
// bookend `t` with the PromQL `offset` modifier folded in. Zero `t`
// falls back to `now64(9)`. A non-zero offsetNS wraps the bookend in
// `(<bookend> - toIntervalNanosecond(<offsetNS>))`.
func absentOverTimeBookendFrag(t time.Time, offsetNS int64) Frag {
	base := timeOrNowFrag(t)
	if offsetNS == 0 {
		return base
	}
	return func(b *Builder) {
		b.sb.WriteByte('(')
		base(b)
		b.sb.WriteString(" - toIntervalNanosecond(")
		b.sb.WriteString(strconv.FormatInt(offsetNS, 10))
		b.sb.WriteString("))")
	}
}

// absentOverTimeAnchorRangeFrag returns a Frag rendering
// `arrayJoin(arrayMap(i -> <start> + i * <stepNS>, range(0, <N>)))`
// — the step-grid fan-out for the range-mode absent_over_time
// emission. The anchor base is `<start>` (NOT `<end>`), matching the
// Prom range-query convention where anchors walk forward from the
// query's `start` to its `end`.
func absentOverTimeAnchorRangeFrag(start Frag, stepNS, numAnchors int64) Frag {
	return func(b *Builder) {
		b.sb.WriteString("arrayJoin(arrayMap(i -> ")
		start(b)
		b.sb.WriteString(" + toIntervalNanosecond(i * ")
		b.sb.WriteString(strconv.FormatInt(stepNS, 10))
		b.sb.WriteString("), range(0, ")
		b.sb.WriteString(strconv.FormatInt(numAnchors, 10))
		b.sb.WriteString(")))")
	}
}

// absentOverTimeCoveredAnchorFrag returns a Frag rendering the
// sample-side COVERED-anchor fanout for the range-mode absent shape —
// the forward-grid sibling of sampleAnchorFanoutFrag (range_window.go):
//
//	arrayJoin(arrayMap(i -> <start> + toIntervalNanosecond(i * <stepNS>),
//	          range(greatest(0, <floorDiv(dist - 1, stepNS) + 1>),
//	                least(<N>, <floorDiv(dist + rangeNS - 1, stepNS) + 1>))))
//
// with `dist = dateDiff('nanosecond', <start>, <ts>)`. A sample at ts
// covers exactly the grid anchors a_i = start + i*step whose lookback
// window `(a_i - range, a_i]` contains it:
//
//	ts <= a_i          ⇔  i*step >= dist          ⇔  i >= ceil(dist / step)        = floor((dist - 1) / step) + 1
//	ts >  a_i - range  ⇔  i*step <  dist + range  ⇔  i <= floor((dist + range - 1) / step)
//
// (integer dist, strict edges folded into the ±1 shifts) — at most
// range/step + 1 anchors per sample. The greatest/least clamps and the
// truncate-toward-zero intDiv correction follow the same contract as
// sampleAnchorFanoutFrag; see writeAnchorGridFloorIdx.
func absentOverTimeCoveredAnchorFrag(start, ts Frag, stepNS, rangeNS, numAnchors int64) Frag {
	dist := func(b *Builder) {
		b.sb.WriteString("dateDiff('nanosecond', ")
		start(b)
		b.sb.WriteString(", ")
		ts(b)
		b.sb.WriteByte(')')
	}
	return func(b *Builder) {
		b.sb.WriteString("arrayJoin(arrayMap(i -> ")
		start(b)
		b.sb.WriteString(" + toIntervalNanosecond(i * ")
		b.sb.WriteString(strconv.FormatInt(stepNS, 10))
		b.sb.WriteString("), range(greatest(0, ")
		writeAnchorGridFloorIdx(b, dist, -1, stepNS)
		b.sb.WriteString("), least(")
		b.sb.WriteString(strconv.FormatInt(numAnchors, 10))
		b.sb.WriteString(", ")
		writeAnchorGridFloorIdx(b, dist, rangeNS-1, stepNS)
		b.sb.WriteString("))))")
	}
}

// notInSubqueryFrag renders `<left> NOT IN (<sub>)` — the anti-set
// predicate the range-mode absent shape uses to keep only anchors no
// sample covers. Mirrors the `NOT IN (SELECT …)` idiom in
// vector_set_op.go's setOpInSubqueryFrag, kept local because the
// left-hand side here is a bare alias rather than a match-key
// expression.
func notInSubqueryFrag(left, sub Frag) Frag {
	return func(b *Builder) {
		left(b)
		b.sb.WriteString(" NOT IN (")
		sub(b)
		b.sb.WriteByte(')')
	}
}

// synthAttrsMapFrag renders the matcher-derived label set as a CH
// `Map(String, String)` value. Mirrors `internal/promql/lower.go`'s
// `emptyAttrsMap` shape: an empty list yields `CAST(map(), ?)` with
// the type name `'Map(String,String)'` parameterised as a `?`-bound
// string (chDB accepts the two-arg `CAST(value, type)` shape; the
// SQL-standard `CAST(value AS type)` syntax requires the type-name
// to be an unquoted identifier which the parameter binding path
// can't supply). Non-empty labels emit `map(?, ?, ?, ?, ...)` with
// each key + value parameterised as `?`.
func synthAttrsMapFrag(labels []chplan.SynthLabel) Frag {
	if len(labels) == 0 {
		return Call("CAST", Call("map"), Lit("Map(String,String)"))
	}
	args := make([]Frag, 0, len(labels)*2)
	for _, kv := range labels {
		args = append(args, Lit(kv.Key), Lit(kv.Value))
	}
	return Call("map", args...)
}

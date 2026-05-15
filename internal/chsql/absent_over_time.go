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
//	                                  range(0, <N>))) AS `anchor_ts`,
//	               `sample_ts_arr`
//	        FROM (
//	            SELECT groupArray(`TimeUnix`) AS `sample_ts_arr`
//	            FROM (<matcher-filtered scan>)
//	            WHERE `TimeUnix` > '<global-start - range>'
//	              AND `TimeUnix` <= '<global-end>'
//	        )
//	    )
//	    WHERE arrayCount(t -> t > `anchor_ts` - toIntervalNanosecond(<range_ns>)
//	                            AND t <= `anchor_ts`,
//	                     `sample_ts_arr`) = 0
//	)
//
// The inner `groupArray + GROUP BY ()` always emits exactly one row —
// even when the matcher scan returns zero rows, in which case
// `sample_ts_arr = []` and every anchor in the outer arrayJoin
// survives the WHERE clause (the desired "absent at every anchor"
// signal).
//
// SQL shape (instant mode — Step == 0): same structure but the
// `anchor_ts` projection is the single `<end>` literal (no arrayJoin
// fanout) and the WHERE filters one row at most.
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

	// Step grid: in range mode (Step > 0) emit one row per anchor via
	// arrayJoin(arrayMap(i -> start + i*step, range(0, N))); in
	// instant mode (Step == 0) emit a single anchor at End.
	var anchorFrag Frag
	if a.Step > 0 {
		stepNS := a.Step.Nanoseconds()
		numAnchors := a.End.Sub(a.Start).Nanoseconds()/stepNS + 1
		if numAnchors < 1 {
			numAnchors = 1
		}
		// The arrayJoin fanout walks the step grid starting from
		// `a.Start` (offset-adjusted by prefilterStartFrag); see
		// absentOverTimeAnchorRangeFrag.
		anchorFrag = absentOverTimeAnchorRangeFrag(prefilterStartFrag, stepNS, numAnchors)
	} else {
		anchorFrag = endFrag
	}

	// Innermost: groupArray of the per-sample timestamps, prefiltered
	// to the global window `(<prefilterStart> - Range, <end>]`. The
	// 1-row Aggregate (no GROUP BY) is emitted directly here rather
	// than going through chplan.Aggregate because we want CH's default
	// 1-row-of-empty-array shape on an empty input (groupArray over no
	// rows = `[]`).
	innermost := NewQuery().
		From(inner).
		Select(As(Call("groupArray", Col(a.TimestampColumn)), "sample_ts_arr")).
		Where(And(
			Gt(Col(a.TimestampColumn),
				Sub(prefilterStartFrag, Call("toIntervalNanosecond", InlineLit(rangeNS)))),
			Lte(Col(a.TimestampColumn), endFrag),
		))

	// Anchor fanout: project anchor_ts (literal in instant mode,
	// arrayJoin'd step grid in range mode) alongside the single-row
	// `sample_ts_arr`.
	fanout := NewQuery().
		From(innermost.Frag()).
		Select(As(anchorFrag, "anchor_ts")).
		Select(BareIdent("sample_ts_arr"))

	// Outer filter: keep only anchors whose lookback window has zero
	// matching samples. The lambda body is `t > anchor_ts -
	// toIntervalNanosecond(<rangeNS>) AND t <= anchor_ts`.
	windowLambda := Lambda1("t", And(
		Gt(BareIdent("t"),
			Sub(BareIdent("anchor_ts"),
				Call("toIntervalNanosecond", InlineLit(rangeNS)))),
		Lte(BareIdent("t"), BareIdent("anchor_ts")),
	))
	emptyWindow := NewQuery().
		From(fanout.Frag()).
		Select(BareIdent("anchor_ts")).
		Where(Eq(
			Call("arrayCount", windowLambda, BareIdent("sample_ts_arr")),
			InlineLit(int64(0)),
		))

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

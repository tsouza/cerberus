package chsql

import (
	"fmt"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitRangeLWR renders a chplan.RangeLWR — the single-pass, bounded
// sample-side fan-out that supersedes the StepGrid CROSS JOIN +
// per-anchor argMax shape for a bare instant-vector selector evaluated
// over a PromQL `query_range` window.
//
// SQL skeleton (N = (End-Start)/Step + 1 grid anchors, but the
// intermediate cardinality is rows × (Lookback/Step + 1), constant in
// N):
//
//	SELECT MetricName, Attributes, anchor_ts AS TimeUnix,
//	       argMax(Value, TimeUnix) AS Value
//	FROM (
//	  SELECT MetricName, Attributes, TimeUnix, Value,
//	         arrayJoin(arrayMap(i -> <grid_base> - toIntervalNanosecond(i * <stepNS>),
//	                   range(greatest(0, floorIdx(dist - lookback)),
//	                         least(<N>, floorIdx(dist))))) AS anchor_ts
//	  FROM (<Input>)
//	)
//	GROUP BY MetricName, Attributes, anchor_ts
//
// where `dist = dateDiff('nanosecond', TimeUnix, <shift_base>)` is the
// sample's distance behind the newest OFFSET-SHIFTED anchor. The two
// bases differ only by the offset:
//
//   - <shift_base> = End - Offset  — drives window membership: a sample
//     at ts belongs to anchor i iff `ts <= shiftBase - i*step` (the
//     `(t - Offset - Lookback, t - Offset]` window's right edge) and
//     `ts > shiftBase - i*step - Lookback` (its left edge). This is the
//     EXACT half-open window the StepGrid Filter applied
//     (`TimeUnix <= anchor_ts - Offset AND TimeUnix > anchor_ts - Offset
//   - Lookback`).
//   - <grid_base>  = End          — the value emitted as anchor_ts /
//     TimeUnix. The Offset shifts the membership window but NOT the
//     reported sample timestamp, so the emitted anchor stays on the
//     unshifted `[Start, End]` grid (matching the StepGrid's anchor_ts).
//
// Because the index `i` is identical for both bases (they differ by the
// constant Offset, which cancels in `shiftBase - i*step` vs the membership
// inequalities), a single `range(lo, hi)` drives both: each sample fans
// to the same ≤ Lookback/Step + 1 anchors and the arrayMap body emits the
// unshifted grid anchor for each. The `argMax(Value, TimeUnix)` per
// (series, anchor) bucket then collapses to the newest in-window sample —
// the LWR-canonical "latest sample in the staleness window". An anchor
// with no sample in its window receives no fanned row and so produces no
// GROUP BY row — preserving Prom's staleness gap.
//
// Zero Start/End (the deterministic fixture shape) falls back to
// `now64(9)` for both bases via timeOrNowFrag.
func (e *emitter) emitRangeLWR(r *chplan.RangeLWR) error {
	if r.Step <= 0 {
		return fmt.Errorf("%w: RangeLWR requires Step > 0", ErrUnsupported)
	}
	if r.Input == nil {
		return fmt.Errorf("%w: RangeLWR.Input is nil", ErrUnsupported)
	}
	if r.TimestampCol == "" || r.ValueCol == "" || r.MetricNameCol == "" || r.AttributesCol == "" {
		return fmt.Errorf("%w: RangeLWR requires MetricName/Attributes/Timestamp/Value column names", ErrUnsupported)
	}

	stepNS := r.Step.Nanoseconds()
	lookbackNS := r.Lookback.Nanoseconds()

	// End-inclusive anchor count across the [Start, End] grid. When the
	// grid bounds are absent (the now64(9) fixture shape) a single anchor
	// is the only deterministic choice; the bounded fanout still applies.
	var numAnchors int64 = 1
	if !r.Start.IsZero() && !r.End.IsZero() {
		span := r.End.Sub(r.Start).Nanoseconds()
		if span < 0 {
			return fmt.Errorf("%w: RangeLWR.Start > End", ErrUnsupported)
		}
		numAnchors = span/stepNS + 1
	}

	// Membership base (offset-shifted newest anchor) and value base
	// (unshifted grid anchor). Offset folds onto the membership base only.
	shiftBase := offsetShiftedBaseFrag(timeOrNowFrag(r.End), r.Offset)
	gridBase := timeOrNowFrag(r.End)

	inner, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}

	tsIdent := func(b *Builder) { b.Ident(r.TimestampCol) }

	// Sample-fanout SELECT: project the series-identity columns + the raw
	// (TimeUnix, Value) pair, then fan each sample across only the anchors
	// whose staleness window contains it. The arrayMap body emits the
	// UNSHIFTED grid anchor; the index bounds are computed against the
	// SHIFTED membership base.
	fanout := NewQuery().From(inner)
	fanout.Select(Col(r.MetricNameCol))
	fanout.Select(Col(r.AttributesCol))
	fanout.Select(Col(r.TimestampCol))
	fanout.Select(Col(r.ValueCol))
	fanout.Select(rawAs(
		lwrAnchorFanoutFrag(gridBase, shiftBase, tsIdent, stepNS, lookbackNS, numAnchors),
		"anchor_ts",
	))

	// Prune the inner scan to the offset-shifted half-open grid span
	// `(Start - Offset - Lookback, End - Offset]` so ClickHouse can skip
	// granules outside the window instead of arrayJoin-fanning every
	// retained sample of every matching series (the query_range
	// O(rows × anchors) re-scan class). The WHERE is evaluated on the
	// source rows BEFORE the SELECT-list arrayJoin expands them, so it
	// only narrows the scan and never drops an in-window anchor. Gated on
	// Start/End being set so the now64()/@-pinned/zero-grid fixtures stay
	// byte-identical.
	maybePushRangeScanTimeBound(fanout, r.TimestampCol, r.Start, r.End, r.Offset.Nanoseconds(), lookbackNS)

	// Collapse SELECT: collapse each (series, anchor) bucket to its newest
	// in-window sample via argMax(Value, TimeUnix). The anchor stays under
	// its own `anchor_ts` alias here — NOT re-aliased to TimeUnix — so the
	// `TimeUnix` reference inside argMax resolves to the INNER per-sample
	// timestamp column rather than the SELECT-list output alias. (CH's
	// analyzer resolves a same-SELECT output alias ahead of a source
	// column of the same name; aliasing anchor_ts → TimeUnix in this
	// SELECT would shadow the argMax's TimeUnix argument with the constant
	// per-group anchor, collapsing argMax to an arbitrary sample. The
	// re-alias is deferred to the outer Project below.)
	const lwrValueAlias = "lwr_value"
	collapse := NewQuery().From(fanout.Frag())
	collapse.Select(Col(r.MetricNameCol))
	collapse.Select(Col(r.AttributesCol))
	collapse.Select(Col("anchor_ts"))
	collapse.Select(rawAs(
		aggFuncFrag(chplan.AggFunc{
			Name: "argMax",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: r.ValueCol},
				&chplan.ColumnRef{Name: r.TimestampCol},
			},
		}),
		lwrValueAlias,
	))
	collapse.GroupBy(Col(r.MetricNameCol), Col(r.AttributesCol), Col("anchor_ts"))

	// Outer SELECT: re-alias anchor_ts → TimeUnix and lwr_value → Value so
	// the canonical 4-column Sample contract holds for downstream
	// consumers. Splitting the re-alias into its own SELECT keeps the
	// collapse SELECT's argMax(Value, TimeUnix) reference unshadowed.
	outer := NewQuery().From(collapse.Frag())
	outer.Select(As(Col(r.MetricNameCol), r.MetricNameCol))
	outer.Select(As(Col(r.AttributesCol), r.AttributesCol))
	outer.Select(As(verbatim("anchor_ts"), r.TimestampCol))
	outer.Select(As(verbatim(lwrValueAlias), r.ValueCol))

	e.emitSelect(outer)
	return nil
}

// lwrAnchorFanoutFrag renders the LWR sample-side anchor fan-out:
//
//	arrayJoin(arrayMap(i -> <gridBase> - toIntervalNanosecond(i * <stepNS>),
//	          range(greatest(0, floorIdx(dist - lookback)),
//	                least(<N>, floorIdx(dist)))))
//
// where `dist = dateDiff('nanosecond', <ts>, <shiftBase>)` is the
// sample's distance behind the newest offset-shifted anchor. A sample at
// ts belongs to exactly the anchors a_i = shiftBase - i*step whose
// left-open / right-closed staleness window `(a_i - lookback, a_i]`
// contains it:
//
//	ts <= a_i             ⇔  i*step <= dist             ⇔  i <= floor(dist / step)
//	ts >  a_i - lookback  ⇔  i*step >  dist - lookback  ⇔  i >= floor((dist - lookback)/step) + 1
//
// — at most lookback/step + 1 indices per sample, independent of the grid
// width N. The clamps map both raw bounds through the same monotone
// greatest/least into [0, N], so out-of-grid samples degenerate to an
// empty `range(lo, hi)` (`arrayJoin([])` drops the row). The emitted
// anchor value uses <gridBase> (unshifted) while the index math uses
// <shiftBase> — the offset shifts the membership window, not the reported
// timestamp.
//
// This is the LWR sibling of sampleAnchorFanoutFrag: same bounded-index
// machinery (writeAnchorGridFloorIdx), but the arrayMap body emits the
// unshifted grid anchor so an offset query reports the grid timestamp,
// and the window width is the staleness lookback rather than a PromQL
// range-vector `[range]`.
func lwrAnchorFanoutFrag(gridBase, shiftBase, ts Frag, stepNS, lookbackNS, numAnchors int64) Frag {
	dist := distBehindAnchorFrag(ts, shiftBase)
	return Call(
		"arrayJoin",
		Call(
			"arrayMap",
			Lambda1("i", anchorBaseAtIdxFrag(gridBase, stepNS)),
			Call(
				"range",
				Call("greatest", InlineLit(int64(0)), anchorGridFloorIdxFrag(dist, -lookbackNS, stepNS)),
				Call("least", InlineLit(numAnchors), anchorGridFloorIdxFrag(dist, 0, stepNS)),
			),
		),
	)
}

// offsetShiftedBaseFrag renders an anchor base shifted back by a PromQL
// `offset`: `(<base> - toIntervalNanosecond(<offsetNS>))` when offset is
// non-zero, or the bare base otherwise. The parens match the membership
// base the StepGrid Filter applied. Shared by emitRangeLWR and
// emitRangeBucketFanout.
func offsetShiftedBaseFrag(base Frag, offset time.Duration) Frag {
	if offset == 0 {
		return base
	}
	return Paren(Sub(base, Call("toIntervalNanosecond", InlineLit(offset.Nanoseconds()))))
}

// maybePushRangeScanTimeBound pushes the offset-shifted half-open scan
// bound `(start - offset - spanNS, end - offset]` onto `sb` (a SELECT
// reading the inner Scan/Filter subquery) so ClickHouse prunes granules
// outside the eval grid instead of fanning every retained sample over
// every anchor. It is the raw-time-arg sibling of
// maybePushInnerScanTimeBounds (which takes a *chplan.RangeWindow): the
// RangeLWR / RangeBucketFanout / native-resample / native-rate nodes
// carry Start/End/Offset directly, not a RangeWindow, so they pass the
// times through here.
//
// spanNS is the grid's backward reach from each anchor — the staleness
// Lookback for the LWR/bucket fanout shapes, the range `[range]` for the
// native rate shape. It widens the lower edge so a sample that belongs to
// the earliest in-grid anchor's window survives the scan prune.
//
// Gated on BOTH start and end being set: the now64()/@-pinned/zero-grid
// fixture shapes leave them zero and rely on the bound being suppressed
// to stay byte-stable against pinned goldens. The bound reuses
// innerScanTsBoundsFrags so the offset-sign and strict-lower/inclusive-
// upper semantics match the matrix path exactly.
func maybePushRangeScanTimeBound(sb *QueryBuilder, tsCol string, start, end time.Time, offsetNS, spanNS int64) {
	if start.IsZero() || end.IsZero() {
		return
	}
	lo, hi := innerScanTsBoundsFrags(tsCol, start, end, offsetNS, spanNS)
	sb.Where(lo, hi)
}

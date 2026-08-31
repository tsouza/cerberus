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
//
// chplan.RangeLWR.SampleTimestamp adds ONE column to both the collapse and
// the outer SELECT — `max(TimeUnix) AS lwr_sample_ts` — carrying the own
// timestamp of the sample argMax selected, which the collapse otherwise
// discards. The four canonical columns are untouched: TimeUnix remains the
// step anchor.
//
// When SampleTimestamp AND ArgAndMaxFusion are both set (chopt.
// FeatureArgAndMaxFusion, cerberus issue #2764), the collapse fuses that
// pair into ONE `argAndMax(Value, TimeUnix)` call instead — see
// rangeLWRCollapseFrag — and this outer SELECT destructures the resulting
// tuple via `tupleElement` rather than re-aliasing two separately-named
// collapse columns.
func (e *emitter) emitRangeLWR(r *chplan.RangeLWR) error {
	collapse, err := e.rangeLWRCollapseFrag(r)
	if err != nil {
		return err
	}

	// Outer SELECT: re-alias anchor_ts → TimeUnix and lwr_value → Value so
	// the canonical 4-column Sample contract holds for downstream
	// consumers. Splitting the re-alias into its own SELECT keeps the
	// collapse SELECT's argMax(Value, TimeUnix) reference unshadowed.
	outer := NewQuery().From(collapse)
	outer.Select(As(Col(r.MetricNameCol), r.MetricNameCol))
	outer.Select(As(Col(r.AttributesCol), r.AttributesCol))
	outer.Select(As(verbatim(rangeLWRAnchorColumn), r.TimestampCol))
	if r.SampleTimestamp && r.ArgAndMaxFusion {
		// argAndMax(arg, val) returns Tuple(arg, val) — element 1 is the
		// picked Value, element 2 is the max(TimeUnix) that selected it.
		outer.Select(As(tupleElemFrag(Col(rangeLWRArgAndMaxAlias), 1), r.ValueCol))
		outer.Select(As(
			tupleElemFrag(Col(rangeLWRArgAndMaxAlias), 2),
			chplan.RangeLWRSampleTimestampColumn,
		))
		return e.emitSelect(outer)
	}
	outer.Select(As(verbatim(rangeLWRValueAlias), r.ValueCol))
	// The requested fifth column rides through under its own name — it is
	// NOT re-aliased over TimestampCol, which stays the step anchor.
	if r.SampleTimestamp {
		outer.Select(As(
			verbatim(chplan.RangeLWRSampleTimestampColumn),
			chplan.RangeLWRSampleTimestampColumn,
		))
	}

	return e.emitSelect(outer)
}

// rangeLWRAnchorColumn is the fan-out stage's synthetic per-sample anchor
// column — the `arrayJoin` output every fanned row carries under this
// bare name before [emitRangeLWR]'s outer SELECT re-aliases it onto the
// canonical TimestampCol. Exported at package scope (rather than a
// string literal repeated at each call site) because
// emitAggregateRangeLWRFused (aggregate_range_lwr_fusion.go) reads it
// directly off both the fan-out and collapse Frags it reuses from
// [emitter.rangeLWRFanoutFrag] / [emitter.rangeLWRCollapseFrag].
const rangeLWRAnchorColumn = "anchor_ts"

// rangeLWRValueAlias is the collapse stage's synthetic argMax-picked
// value column — see rangeLWRAnchorColumn's doc for why this is a named
// package constant rather than a repeated literal.
const rangeLWRValueAlias = "lwr_value"

// rangeLWRArgAndMaxAlias is the collapse stage's synthetic argAndMax-picked
// (Value, TimeUnix) tuple column, emitted in place of the separate
// rangeLWRValueAlias / chplan.RangeLWRSampleTimestampColumn columns when
// [chplan.RangeLWR.ArgAndMaxFusion] is set (chopt.FeatureArgAndMaxFusion,
// cerberus issue #2764). emitRangeLWR's outer SELECT destructures it via
// tupleElemFrag.
const rangeLWRArgAndMaxAlias = "lwr_argandmax"

// rangeLWRFanoutFrag renders RangeLWR's sample-side fan-out stage ONLY —
// the SELECT that projects the series-identity columns plus the raw
// (TimestampCol, ValueCol) pair and fans each sample across the bounded
// set of anchors whose staleness window contains it (see
// [lwrAnchorFanoutFrag]'s doc for the index math). Each fanned row still
// carries its OWN per-sample TimestampCol untouched, alongside the
// synthetic [rangeLWRAnchorColumn] the fan-out computed.
//
// Shared by [emitter.rangeLWRCollapseFrag] (which layers the argMax
// per-series collapse on top) and emitAggregateRangeLWRFused's count()
// fusion fast path (aggregate_range_lwr_fusion.go), which reads the
// fan-out rows DIRECTLY — bypassing the per-series collapse entirely,
// because "does series X have >=1 sample in this anchor's window" does
// not require picking WHICH of X's samples landed there, only whether
// any fanned row for X exists. See that file's doc comment for the full
// soundness argument.
//
// The returned Frag is already wrapped by lwrFanoutBoundedSourceFrag
// (#2447) — both callers read the bounded, guarded source, never the raw
// arrayJoin fan-out, so neither can forget to opt into the resource bound.
func (e *emitter) rangeLWRFanoutFrag(r *chplan.RangeLWR) (Frag, error) {
	if r.Step <= 0 {
		return nil, fmt.Errorf("%w: RangeLWR requires Step > 0", ErrUnsupported)
	}
	if r.Input == nil {
		return nil, fmt.Errorf("%w: RangeLWR.Input is nil", ErrUnsupported)
	}
	if r.TimestampCol == "" || r.ValueCol == "" || r.MetricNameCol == "" || r.AttributesCol == "" {
		return nil, fmt.Errorf("%w: RangeLWR requires MetricName/Attributes/Timestamp/Value column names", ErrUnsupported)
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
			return nil, fmt.Errorf("%w: RangeLWR.Start > End", ErrUnsupported)
		}
		numAnchors = span/stepNS + 1
	}

	// Membership base (offset-shifted newest anchor) and value base
	// (unshifted grid anchor). Offset folds onto the membership base only.
	shiftBase := offsetShiftedBaseFrag(timeOrNowFrag(r.End), r.Offset)
	gridBase := timeOrNowFrag(r.End)

	inner, err := e.subqueryFrag(r.Input)
	if err != nil {
		return nil, err
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
	fanout.Select(RawAs(
		lwrAnchorFanoutFrag(gridBase, shiftBase, tsIdent, stepNS, lookbackNS, numAnchors),
		rangeLWRAnchorColumn,
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

	// #2447/#2470: see lwrFanoutBoundedSourceFrag's own doc comment.
	// #2667: e.rangeLWRFanoutRowBound() resolves the operator override (or
	// maxRangeLWRFanoutRows's own default) once per Emit call.
	return lwrFanoutBoundedSourceFrag(fanout.Frag(), r.TimestampCol, e.rangeLWRFanoutRowBound(), RangeLWRFanoutBudgetMessage), nil
}

// rangeLWRCollapseFrag renders RangeLWR's fan-out + per-series collapse
// stages — the [Frag] up through the `argMax(Value, TimeUnix) GROUP BY
// (MetricName, Attributes, anchor_ts)` that picks each (series, anchor)
// bucket's newest in-window sample — WITHOUT the trailing re-alias
// SELECT [emitRangeLWR] layers on top to publish the canonical Sample
// contract.
//
// Two callers share it: emitRangeLWR itself (which still needs the
// re-alias to publish MetricName/Attributes/TimeUnix/Value under their
// canonical names) and emitAggregateRangeLWRFused's sum()/count()-Value
// fusion fast path (aggregate_range_lwr_fusion.go), which reads the
// collapse stage's OWN column names — MetricNameCol/AttributesCol
// unchanged, [rangeLWRAnchorColumn], [rangeLWRValueAlias] — directly, so
// the outer aggregation's GROUP BY + reducer land in the SAME SELECT that
// does the re-alias, instead of paying for a whole extra opaque-subquery
// pass over the already-collapsed rows just to rename two columns.
func (e *emitter) rangeLWRCollapseFrag(r *chplan.RangeLWR) (Frag, error) {
	fanout, err := e.rangeLWRFanoutFrag(r)
	if err != nil {
		return nil, err
	}

	// Collapse SELECT: collapse each (series, anchor) bucket to its newest
	// in-window sample via argMax(Value, TimeUnix). The anchor stays under
	// its own `anchor_ts` alias here — NOT re-aliased to TimeUnix — so the
	// `TimeUnix` reference inside argMax resolves to the INNER per-sample
	// timestamp column rather than the SELECT-list output alias. (CH's
	// analyzer resolves a same-SELECT output alias ahead of a source
	// column of the same name; aliasing anchor_ts → TimeUnix in this
	// SELECT would shadow the argMax's TimeUnix argument with the constant
	// per-group anchor, collapsing argMax to an arbitrary sample. The
	// re-alias happens in the outer Project above instead.)
	collapse := NewQuery().From(fanout)
	collapse.Select(Col(r.MetricNameCol))
	collapse.Select(Col(r.AttributesCol))
	collapse.Select(Col(rangeLWRAnchorColumn))
	if r.SampleTimestamp && r.ArgAndMaxFusion {
		// Fused form (chopt.FeatureArgAndMaxFusion, cerberus issue #2764):
		// one argAndMax(Value, TimeUnix) state replaces the argMax(Value,
		// TimeUnix) + max(TimeUnix) pair below. The tuple carries both
		// values emitRangeLWR's outer SELECT needs, so there is nothing
		// left for a SampleTimestamp-less fused call to add — the fusion
		// only fires when SampleTimestamp is requested (see
		// [chplan.RangeLWR.ArgAndMaxFusion]'s own doc).
		collapse.Select(RawAs(
			Call("argAndMax", Col(r.ValueCol), Col(r.TimestampCol)),
			rangeLWRArgAndMaxAlias,
		))
	} else {
		collapse.Select(RawAs(
			aggFuncFrag(chplan.AggFunc{
				Fn: chplan.FnArgMax,
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: r.ValueCol},
					&chplan.ColumnRef{Name: r.TimestampCol},
				},
			}),
			rangeLWRValueAlias,
		))
		// The selecting sample's OWN timestamp, on request. `argMax(Value,
		// TimeUnix)` above keeps the value and throws the timestamp that picked
		// it away, so it has to be aggregated separately or it is unrecoverable
		// once this SELECT closes. `max(TimeUnix)` over the bucket IS that
		// timestamp: every fanned row in a (series, anchor) bucket is in the same
		// staleness window and argMax picks the newest of them, so the newest
		// timestamp and the argMax-selecting timestamp are the same value.
		// Its own alias keeps it clear of the inner TimeUnix column that
		// argMax's second argument must resolve to (see the note above).
		if r.SampleTimestamp {
			collapse.Select(RawAs(
				aggFuncFrag(chplan.AggFunc{
					Fn:   chplan.FnMax,
					Args: []chplan.Expr{&chplan.ColumnRef{Name: r.TimestampCol}},
				}),
				chplan.RangeLWRSampleTimestampColumn,
			))
		}
	}
	collapse.GroupBy(Col(r.MetricNameCol), Col(r.AttributesCol), Col(rangeLWRAnchorColumn))

	return collapse.Frag(), nil
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

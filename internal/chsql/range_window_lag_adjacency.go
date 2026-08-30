package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// range_window_lag_adjacency.go implements chplan.RangeWindow.LagAdjacency
// (cerberus issue #2759): a single sorted lagInFrame/leadInFrame annotation
// pass plus fixed-size per-anchor accumulators, in place of the
// groupArray + arraySort + arrayPopBack/arrayPopFront array-fold fan-out
// (emitRangeWindowChanges/Resets/IRate/IDelta in range_window.go), for the
// query_range MATRIX shape (OuterRange > 0) of changes / resets / irate /
// idelta.
//
// The array-fold path re-derives every adjacent (prev, curr) sample pair
// once per COVERING ANCHOR: sampleAnchorFanoutFrag fans each sample to up to
// Range/Step+1 anchors, and the per-anchor groupArray + arraySort +
// arrayPopBack/arrayPopFront fold re-walks the whole resulting array for
// each of those anchors independently. This file's shape instead computes
// each raw sample's (prev_ts, prev_val) exactly ONCE — via a
// `PARTITION BY <series key> ORDER BY ts, value ROWS BETWEEN UNBOUNDED
// PRECEDING AND CURRENT ROW` window — and carries the pair through the SAME
// sample-anchor fan-out unchanged (identical window membership, identical
// row count), so the per-(series, anchor) reduction becomes a fixed-size
// accumulator (sumIf for changes/resets, argMaxIf for irate/idelta) instead
// of an O(window) array walk.
//
// Slice-invariance: see internal/chplan/sliceinvariant.go's RangeWindow
// entry for the full argument. In short, every pair this shape counts is
// gated on lagAdjacencyValidPrevFrag (`prev_ts > anchor - range`), and that
// check is what makes a shard's lagInFrame seed unobservable — a pair a
// shard seeds wrong always fails the check, and a pair the check admits
// always has its true predecessor physically present in that shard's own
// (Offset+Range-widened) scan.
//
// Kernels are carried over UNCHANGED from the array-fold emitters: resets'
// `curr < prev`, changes' `curr != prev AND NOT (isNaN(curr) AND
// isNaN(prev))` (the #1489 NaN carve-out), and irate's
// CounterOrDeltaPairDelta DELTA/CUMULATIVE branch (with
// AggregationTemporality riding the argMax tuple so the branch survives the
// annotation stage). Proven bit-identical to the array-fold path by
// dual-emit parity — range_window_lag_adjacency_chdb_test.go — not merely
// asserted.

// Column aliases the annotation/fan-out/regroup layers thread between
// themselves. Unexported and local to this file's own SELECT lists, so a
// collision with an upstream column name is not a concern (the layers above
// this shape only ever reference the group-by keys, anchor_ts, and
// r.ValueColumn under its own name).
const (
	lagAdjPrevTsAlias      = "prev_ts"
	lagAdjPrevValAlias     = "prev_val"
	lagAdjIsLastOfRunAlias = "is_last_of_run"
	lagAdjCountAlias       = "n"
	lagAdjCurrTupleAlias   = "curr"
	lagAdjSumAlias         = "lag_adj_sum"
)

// lagAdjacencyMatrixShapeCheck validates the two RangeWindow fields every
// LagAdjacency emitter needs, mirroring the windowed-array emitters' own
// TimestampColumn/ValueColumn guards. The OuterRange/Step matrix-shape check
// is a defensive belt-and-braces: promql.lagAdjacencyEligible already gates
// this at lowering time, so a plan reaching here with OuterRange <= 0 means
// LagAdjacency was set by something other than the boot-wired strategies —
// fail loudly rather than emit an instant-shape query through a
// matrix-only code path.
func lagAdjacencyMatrixShapeCheck(r *chplan.RangeWindow) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset", ErrUnsupported)
	}
	if r.ValueColumn == "" {
		return fmt.Errorf("%w: RangeWindow.ValueColumn unset", ErrUnsupported)
	}
	if r.OuterRange <= 0 || r.Step <= 0 {
		return fmt.Errorf("%w: RangeWindow.LagAdjacency requires a matrix (OuterRange > 0, Step > 0) window", ErrUnsupported)
	}
	return nil
}

// lagAdjacencyAnnotateLayer builds the shape's first layer: one row per raw
// sample from innerSub, tagged with (prev_ts, prev_val) via lagInFrame and,
// when needsSurvivor is set (the irate/idelta pairs shape), an
// is_last_of_run flag via leadInFrame.
//
// All three window-function calls share ONE `PARTITION BY <groupFrags>
// ORDER BY <srcTs>, <r.ValueColumn>` — the explicit compound ORDER BY (not
// CH's default RANGE-frame peer-grouping) numbers every row individually
// even when it shares its ORDER BY key with a duplicate-timestamp
// neighbour; the (ts, value) order matches groupArrayPairFrag's own
// `arraySort(groupArray(Tuple(ts, value)))` tuple order exactly, so a
// duplicate-timestamp run's max-VALUE row is always the run's LAST row
// here too — the identical tie-break the array-fold's
// `window_pairs[length]` / `window_vals[length]` positional pick applies.
// The explicit ROWS frame differs by DIRECTION: lagInFrame looks behind, so
// it needs RowsUnboundedPrecedingToCurrentRow (admitting the preceding
// rows); leadInFrame looks ahead, so it needs the complementary
// RowsCurrentRowToUnboundedFollowing — a window function can only see rows
// its own frame admits, and under the backward-only frame a lead offset
// always falls outside it and returns the out-of-frame default on every
// row (see that frame constructor's doc).
//
// lagInFrame's own default (no explicit `default` argument) is the column's
// zero value for a row with no predecessor in its partition — DateTime64's
// epoch, ClickHouse's own documented behaviour, not a cerberus literal.
// Every real sample timestamp is far past 1970, so a defaulted prev_ts
// always fails lagAdjacencyValidPrevFrag by construction; no separate
// has-predecessor flag is needed.
//
// The scan is bounded by maybePushInnerScanTimeBounds — gated on Start/End
// being set, exactly like the array-fold matrix emitters, and the SAME
// Offset+Range widening the slice-invariance argument in
// internal/chplan/sliceinvariant.go rests on.
func lagAdjacencyAnnotateLayer(
	e *emitter,
	r *chplan.RangeWindow,
	innerSub Frag,
	groupFrags []Frag,
	srcTs string,
	needsSurvivor bool,
) *QueryBuilder {
	orderBy := []OrderKey{{Expr: Col(srcTs)}, {Expr: Col(r.ValueColumn)}}
	// lagInFrame looks BEHIND the current row, so its frame's lower bound must
	// admit the preceding rows — RowsUnboundedPrecedingToCurrentRow.
	// leadInFrame looks AHEAD, so it needs the complementary forward frame
	// (RowsCurrentRowToUnboundedFollowing); under the backward frame a lead
	// offset always falls outside the frame and returns the out-of-frame
	// default on every row, never the actual next row (see that frame
	// constructor's own doc — verified against chDB).
	lagFrame := RowsUnboundedPrecedingToCurrentRow()
	leadFrame := RowsCurrentRowToUnboundedFollowing()
	hasTemporality := windowTemporalityProjected(r)

	annotate := NewQuery().From(innerSub)
	annotate.Select(groupFrags...)
	annotate.Select(Col(srcTs))
	annotate.Select(Col(r.ValueColumn))
	if hasTemporality {
		annotate.Select(Col(r.TemporalityColumn))
	}
	annotate.Select(As(
		WindowFrame(Call("lagInFrame", Col(srcTs)), groupFrags, orderBy, lagFrame),
		lagAdjPrevTsAlias,
	))
	annotate.Select(As(
		WindowFrame(Call("lagInFrame", Col(r.ValueColumn)), groupFrags, orderBy, lagFrame),
		lagAdjPrevValAlias,
	))
	if needsSurvivor {
		annotate.Select(As(
			Neq(WindowFrame(Call("leadInFrame", Col(srcTs)), groupFrags, orderBy, leadFrame), Col(srcTs)),
			lagAdjIsLastOfRunAlias,
		))
	}
	maybePushInnerScanTimeBounds(annotate, r, srcTs, r.Range.Nanoseconds())
	return annotate
}

// lagAdjacencyValidPrevFrag renders `prev_ts > anchor_ts - <rangeNS>` — the
// per-row validity check that excludes, BY CONSTRUCTION, any pair whose
// prev sample falls outside the anchor's own `(anchor-range, anchor]`
// window. This is both the correctness gate that keeps a pair's kernel
// evaluation from ever firing on a stale/out-of-window prev, AND the
// mechanism that neutralises the lagInFrame shard-seed hazard (see
// internal/chplan/sliceinvariant.go's RangeWindow entry).
func lagAdjacencyValidPrevFrag(rangeNS int64) Frag {
	return Gt(Col(lagAdjPrevTsAlias), Sub(Col(RangeWindowAnchorAlias), Call("toIntervalNanosecond", InlineLit(rangeNS))))
}

// lagAdjResetsKernel is the per-pair indicator for resets(): `curr < prev`,
// Prometheus's counter-reset condition. Byte-identical semantics to
// emitRangeWindowResets's arrayMap lambda body.
func lagAdjResetsKernel(curr, prev Frag) Frag {
	return Lt(curr, prev)
}

// lagAdjChangesKernel is the per-pair indicator for changes():
// `curr != prev AND NOT (isNaN(curr) AND isNaN(prev))` — the #1489
// both-NaN carve-out. Byte-identical semantics to emitRangeWindowChanges's
// arrayMap lambda body.
func lagAdjChangesKernel(curr, prev Frag) Frag {
	return And(
		Neq(curr, prev),
		Not(Paren(And(Call("isNaN", curr), Call("isNaN", prev)))),
	)
}

// emitRangeWindowChangesLagAdjacency emits SQL for `changes(v[range])`
// under chplan.RangeWindow.LagAdjacency. See emitLagAdjacencyChangesResets.
func (e *emitter) emitRangeWindowChangesLagAdjacency(r *chplan.RangeWindow) error {
	return e.emitLagAdjacencyChangesResets(r, lagAdjChangesKernel)
}

// emitRangeWindowResetsLagAdjacency emits SQL for `resets(v[range])`
// under chplan.RangeWindow.LagAdjacency. See emitLagAdjacencyChangesResets.
func (e *emitter) emitRangeWindowResetsLagAdjacency(r *chplan.RangeWindow) error {
	return e.emitLagAdjacencyChangesResets(r, lagAdjResetsKernel)
}

// emitLagAdjacencyChangesResets renders the lagInFrame annotation shape
// shared by changes/resets:
//
//  1. annotate — one row per raw sample, tagged with (prev_ts, prev_val)
//     via lagInFrame (lagAdjacencyAnnotateLayer).
//  2. fan-out — arrayJoin each annotated row across the anchors it covers
//     (sampleAnchorFanoutFrag, unchanged from the array-fold path — same
//     membership, same row count as window_vals/window_pairs).
//  3. regroup — GROUP BY (series, anchor): `count()` (the raw in-window
//     sample count, replacing `length(window_vals)`) and
//     `sumIf(1, <kernel> AND prev_ts > anchor - range)` (replacing the
//     arrayPopBack/arrayPopFront fold's arraySum).
//  4. outer — `WHERE count() >= 1` (matches `length(window_vals) >= 1`),
//     Cast the sum to Float64 (matches the array-fold emitters' own Cast).
func (e *emitter) emitLagAdjacencyChangesResets(r *chplan.RangeWindow, kernel func(curr, prev Frag) Frag) error {
	if err := lagAdjacencyMatrixShapeCheck(r); err != nil {
		return err
	}

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

	annotate := lagAdjacencyAnnotateLayer(e, r, innerSub, groupFrags, srcTs, false)

	fanout := NewQuery().From(annotate.Frag())
	fanout.Select(groupFrags...)
	fanout.Select(Col(r.ValueColumn))
	fanout.Select(Col(lagAdjPrevValAlias))
	fanout.Select(Col(lagAdjPrevTsAlias))
	fanout.Select(RawAs(
		sampleAnchorFanoutFrag(end, Col(srcTs), stepNS, rangeNS, numAnchors),
		RangeWindowAnchorAlias,
	))

	regroup := NewQuery().From(fanout.Frag())
	regroup.Select(groupFrags...)
	regroup.Select(Col(RangeWindowAnchorAlias))
	regroup.Select(As(Call("count"), lagAdjCountAlias))
	regroup.Select(As(
		Call("sumIf", InlineLit(int64(1)), And(
			lagAdjacencyValidPrevFrag(rangeNS),
			kernel(Col(r.ValueColumn), Col(lagAdjPrevValAlias)),
		)),
		lagAdjSumAlias,
	))
	regroupKeys := make([]Frag, 0, len(groupFrags)+1)
	regroupKeys = append(regroupKeys, groupFrags...)
	regroupKeys = append(regroupKeys, Col(RangeWindowAnchorAlias))
	regroup.GroupBy(regroupKeys...)

	outer := NewQuery().From(regroup.Frag())
	outer.Select(groupFrags...)
	outer.Select(Col(RangeWindowAnchorAlias))
	projectAnchorAsTimestampColumn(outer, r)
	outer.Select(As(Cast(Col(lagAdjSumAlias), "Float64"), r.ValueColumn))
	outer.Where(Gte(Col(lagAdjCountAlias), InlineLit(int64(1))))

	return e.emitSelect(outer)
}

// emitRangeWindowIRateLagAdjacency emits SQL for `irate(v[range])` under
// chplan.RangeWindow.LagAdjacency. See emitLagAdjacencyPairs.
func (e *emitter) emitRangeWindowIRateLagAdjacency(r *chplan.RangeWindow) error {
	return e.emitLagAdjacencyPairs(r, true)
}

// emitRangeWindowIDeltaLagAdjacency emits SQL for `idelta(v[range])` under
// chplan.RangeWindow.LagAdjacency. See emitLagAdjacencyPairs.
func (e *emitter) emitRangeWindowIDeltaLagAdjacency(r *chplan.RangeWindow) error {
	return e.emitLagAdjacencyPairs(r, false)
}

// emitLagAdjacencyPairs renders the lagInFrame annotation shape shared by
// irate/idelta:
//
//  1. annotate — one row per raw sample, tagged with (prev_ts, prev_val)
//     AND an is_last_of_run survivor flag (lagAdjacencyAnnotateLayer).
//     Both functions read the last TWO samples of the window — positions
//     window_pairs[length] / window_pairs[length-1] in the array-fold path
//     — so both need the survivor tie-break, unlike changes/resets (which
//     sum over every adjacent pair, duplicates included).
//  2. fan-out — arrayJoin each annotated row across the anchors it covers,
//     same as emitLagAdjacencyChangesResets.
//  3. regroup — GROUP BY (series, anchor): `count()` (the raw in-window
//     sample count, replacing `length(window_pairs)`) and
//     `argMaxIf((val, prev_val, ts, prev_ts[, temporality]), ts,
//     is_last_of_run)` — the survivor-filtered pick of the anchor's
//     chronologically-last sample, carrying its own (prev_val, prev_ts)
//     pair (and, for irate, its series' AggregationTemporality) along in
//     the SAME tuple so the value expression reads one internally
//     consistent row. `n >= 2` (checked in the outer WHERE) is exactly
//     equivalent to "this survivor's prev is in-window" — if two or more
//     raw samples fall in the anchor's window, the second-highest
//     (ts, value) among them IS the survivor's true lagInFrame
//     predecessor (nothing can sort between them), and if only one does,
//     that predecessor is necessarily out of window.
//  4. outer — `WHERE count() >= 2` (matches
//     `length(window_pairs) >= 2` / `length(window_vals) >= 2`), then the
//     per-function value: irate divides the temporality-aware pair delta
//     (chsql.CounterOrDeltaPairDelta, unchanged) by the nanosecond gap
//     (guarded against a zero-second interval — a duplicate-timestamp
//     survivor pair); idelta is the plain `curr - prev` (no
//     counter-reset arithmetic, matching emitRangeWindowIDelta).
func (e *emitter) emitLagAdjacencyPairs(r *chplan.RangeWindow, isIrate bool) error {
	if err := lagAdjacencyMatrixShapeCheck(r); err != nil {
		return err
	}

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

	hasTemporality := windowTemporalityProjected(r)
	annotate := lagAdjacencyAnnotateLayer(e, r, innerSub, groupFrags, srcTs, true)

	fanout := NewQuery().From(annotate.Frag())
	fanout.Select(groupFrags...)
	fanout.Select(Col(srcTs))
	fanout.Select(Col(r.ValueColumn))
	fanout.Select(Col(lagAdjPrevTsAlias))
	fanout.Select(Col(lagAdjPrevValAlias))
	fanout.Select(Col(lagAdjIsLastOfRunAlias))
	if hasTemporality {
		fanout.Select(Col(r.TemporalityColumn))
	}
	fanout.Select(RawAs(
		sampleAnchorFanoutFrag(end, Col(srcTs), stepNS, rangeNS, numAnchors),
		RangeWindowAnchorAlias,
	))

	currFields := []Frag{Col(r.ValueColumn), Col(lagAdjPrevValAlias), Col(srcTs), Col(lagAdjPrevTsAlias)}
	if hasTemporality {
		currFields = append(currFields, Col(r.TemporalityColumn))
	}

	regroup := NewQuery().From(fanout.Frag())
	regroup.Select(groupFrags...)
	regroup.Select(Col(RangeWindowAnchorAlias))
	regroup.Select(As(Call("count"), lagAdjCountAlias))
	regroup.Select(As(
		Call("argMaxIf", Tuple(currFields...), Col(srcTs), Col(lagAdjIsLastOfRunAlias)),
		lagAdjCurrTupleAlias,
	))
	regroupKeys := make([]Frag, 0, len(groupFrags)+1)
	regroupKeys = append(regroupKeys, groupFrags...)
	regroupKeys = append(regroupKeys, Col(RangeWindowAnchorAlias))
	regroup.GroupBy(regroupKeys...)

	curr := BareIdent(lagAdjCurrTupleAlias)
	lastVal := tupleElemFrag(curr, 1)
	prevVal := tupleElemFrag(curr, 2)
	lastTs := tupleElemFrag(curr, 3)
	prevTs := tupleElemFrag(curr, 4)
	var temporality Frag
	if hasTemporality {
		temporality = tupleElemFrag(curr, 5)
	}

	var value Frag
	if isIrate {
		dt := Call("dateDiff", InlineLit("nanosecond"), prevTs, lastTs)
		delta := CounterOrDeltaPairDelta(func() Frag { return prevVal }, func() Frag { return lastVal }, temporality)
		value = If(
			Gt(dt, InlineLit(int64(0))),
			Div(Paren(delta), Paren(Div(Paren(dt), BareIdent("1e9")))),
			BareIdent("nan"),
		)
	} else {
		value = Sub(lastVal, prevVal)
	}

	outer := NewQuery().From(regroup.Frag())
	outer.Select(groupFrags...)
	outer.Select(Col(RangeWindowAnchorAlias))
	projectAnchorAsTimestampColumn(outer, r)
	outer.Select(As(value, r.ValueColumn))
	outer.Where(Gte(Col(lagAdjCountAlias), InlineLit(int64(2))))

	return e.emitSelect(outer)
}

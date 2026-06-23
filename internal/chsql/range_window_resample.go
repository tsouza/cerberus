package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// nativeResampleFn is the ClickHouse-native aggregate that carries the latest
// in-window sample value forward to each grid point — the native counterpart
// to RangeLWR's argMax sample-fan-out. It is a member of the experimental
// timeSeries*ToGrid family (same allow_experimental_time_series_aggregate_
// functions gate as timeSeriesRateToGrid).
const nativeResampleFn = "timeSeriesResampleToGridWithStaleness"

// emitRangeWindowResample renders a chplan.RangeWindowResample — the
// experimental ClickHouse-native lowering of a range-mode bare instant-vector
// selector (the staleness / instant-vector-selection shape). It produces
// EXACTLY the canonical 4-column Sample row shape RangeLWR emits, so any
// surrounding plan tree (aggregations, arithmetic, instant fns) is byte-for-byte
// unaffected by the substitution:
//
//	SELECT MetricName, Attributes, anchor_ts AS TimeUnix,
//	       toFloat64(grid_val) AS Value
//	FROM (
//	  SELECT MetricName, Attributes,
//	         timeSeriesResampleToGridWithStaleness(<start>, <end>, <step_s>, <stale_s>)(<ts>, <val>) AS grid,
//	         timeSeriesRange(<start>, <end>, <step_s>) AS grid_ts
//	  FROM (<inner Scan/Filter>)
//	  GROUP BY MetricName, Attributes
//	)
//	ARRAY JOIN grid AS grid_val, grid_ts AS anchor_ts
//	WHERE grid_val IS NOT NULL
//
// Load-bearing details, each matching a RangeLWR invariant:
//
//   - `ARRAY JOIN grid AS grid_val, grid_ts AS anchor_ts` explodes the two
//     parallel arrays IN LOCKSTEP, so each resampled value lands on the same
//     row as its timeSeriesRange anchor — the anchor_ts column is the same
//     `[Start, End]` grid RangeLWR walks.
//   - `WHERE grid_val IS NOT NULL` converts native NULL cells (no sample in
//     the staleness window — the aggregate returns Array(Nullable(Float64)))
//     into ABSENT rows, exactly the staleness gap RangeLWR produces when no
//     fanned sample reaches an anchor.
//   - `toFloat64(grid_val)` strips the Nullable so Value is a non-nullable
//     Float64 (load-bearing for prod clickhouse-go strictness; chDB tolerates
//     Nullable but prod 502s). The IS NOT NULL filter has already removed every
//     NULL, so the cast never sees one.
//   - anchor_ts is surfaced under the schema TimestampColumn name so the
//     canonical 4-column contract holds for downstream consumers.
//
// The grid bounds fold the Offset modifier the same way RangeLWR's membership
// base does: both Start and End shift left by Offset so the window slides back
// to `[End - Offset - Lookback, End - Offset]` per anchor, WITHOUT moving the
// emitted anchor timestamp (the timeSeriesRange axis stays on the unshifted
// grid). See the RangeWindowResample doc for the closed-vs-half-open left-edge
// note (the one documented, fixture-invisible divergence from RangeLWR).
//
// The experimental setting is NOT emitted here — the engine detects the node in
// the plan (shared planHasTSGridNative path) and stamps
// allow_experimental_time_series_aggregate_functions=1 onto the per-query ctx.
func (e *emitter) emitRangeWindowResample(r *chplan.RangeWindowResample) error {
	if r.TimestampCol == "" || r.ValueCol == "" || r.MetricNameCol == "" || r.AttributesCol == "" {
		return fmt.Errorf("%w: RangeWindowResample requires MetricName/Attributes/Timestamp/Value column names", ErrUnsupported)
	}
	if r.Step <= 0 {
		return fmt.Errorf("%w: RangeWindowResample requires Step > 0 (range mode)", ErrUnsupported)
	}
	if r.Start.IsZero() || r.End.IsZero() {
		return fmt.Errorf("%w: RangeWindowResample requires pinned Start/End (range mode)", ErrUnsupported)
	}

	// Offset folds onto both grid bounds (window slides back), mirroring the
	// rate native emit. offsetShiftedTimeFrag renders the bare DateTime64
	// literal when Offset is zero (the common case).
	offsetNS := r.Offset.Nanoseconds()
	startFrag := offsetShiftedTimeFrag(r.Start, offsetNS)
	endFrag := offsetShiftedTimeFrag(r.End, offsetNS)
	stepSeconds := int64(r.Step.Seconds())
	stalenessSeconds := int64(r.Lookback.Seconds())

	// timeSeriesResampleToGridWithStaleness(start, end, step_s, stale_s)(ts, value)
	// — the compiled C++ aggregate returning the per-grid-point latest in-window
	// value as Array(Nullable(Float64)). Two paren groups (params then args),
	// rendered by Parametric.
	gridAgg := Parametric(
		nativeResampleFn,
		[]Frag{startFrag, endFrag, InlineLit(stepSeconds), InlineLit(stalenessSeconds)},
		Col(r.TimestampCol),
		Col(r.ValueCol),
	)
	// timeSeriesRange(start, end, step_s) — the parallel anchor-timestamp axis,
	// exploded 1:1 with gridAgg in the ARRAY JOIN below. It uses the SHIFTED
	// bounds too so the per-row index alignment matches gridAgg; the emitted
	// anchor still reads the unshifted grid because Offset shifts only the
	// membership window, and offsetShiftedTimeFrag returns the bare literal when
	// Offset is zero (the byte-stable common case).
	gridTS := Call("timeSeriesRange", startFrag, endFrag, InlineLit(stepSeconds))

	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}

	// Inner SELECT — one row per series carrying the (grid, grid_ts) pair.
	inner := NewQuery().From(innerSub)
	inner.Select(Col(r.MetricNameCol))
	inner.Select(Col(r.AttributesCol))
	inner.Select(As(gridAgg, nativeGridArrayAlias))
	inner.Select(As(gridTS, nativeGridTSAlias))
	// Prune the inner scan to the offset-shifted half-open grid span
	// `(Start - Offset - Lookback, End - Offset]` BEFORE the per-series
	// GROUP BY so ClickHouse skips granules outside the eval window — the
	// resample aggregate otherwise consumes every retained sample of every
	// matching series. Gated on Start/End (always pinned on this node, but
	// kept for a single uniform contract with the fan-out shapes).
	maybePushRangeScanTimeBound(inner, r.TimestampCol, r.Start, r.End, offsetNS, r.Lookback.Nanoseconds())
	inner.GroupBy(Col(r.MetricNameCol), Col(r.AttributesCol))

	// Outer SELECT — explode the parallel arrays in lockstep, drop NULL cells,
	// cast to a non-nullable Float64, and surface anchor_ts under the schema
	// timestamp column name (the canonical 4-column Sample contract).
	outer := NewQuery().From(inner.Frag())
	outer.Select(Col(r.MetricNameCol))
	outer.Select(Col(r.AttributesCol))
	outer.Select(As(Col(RangeWindowAnchorAlias), r.TimestampCol))
	outer.Select(As(Call("toFloat64", Col(nativeGridValAlias)), r.ValueCol))
	outer.ArrayJoin(
		As(Col(nativeGridArrayAlias), nativeGridValAlias),
		As(Col(nativeGridTSAlias), RangeWindowAnchorAlias),
	)
	outer.Where(IsNotNull(Col(nativeGridValAlias)))

	e.emitSelect(outer)
	return nil
}

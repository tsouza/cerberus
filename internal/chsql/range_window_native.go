package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// nativeGridArrayAlias / nativeGridTSAlias / nativeGridValAlias name the
// inner-subquery columns the native timeSeriesRateToGrid emit threads
// through the ARRAY JOIN explosion. Lifting them to named constants keeps
// the producer (inner SELECT) and consumer (ARRAY JOIN + outer SELECT)
// referring to the exact same identifier — a typo in one would otherwise
// surface only as a CH `Unknown identifier` at execution time.
const (
	// nativeGridArrayAlias is the per-series Array(Nullable(Float64)) of
	// per-grid-point rate values produced by timeSeriesRateToGrid.
	nativeGridArrayAlias = "grid"
	// nativeGridTSAlias is the parallel Array(DateTime) anchor axis
	// produced by timeSeriesRange over the same (start, end, step) — its
	// i-th element is the anchor of the i-th grid value, so the two
	// arrays explode in lockstep in the ARRAY JOIN.
	nativeGridTSAlias = "grid_ts"
	// nativeGridValAlias is the per-row exploded rate value (one element
	// of `grid`) after the ARRAY JOIN. NULL cells (< 2 samples in the
	// window) are filtered to absent rows by the WHERE below.
	nativeGridValAlias = "grid_val"
)

// nativeTSGridFn maps a PromQL range function to its ClickHouse-native
// timeSeries*ToGrid aggregate. The map is the single extension point for the
// rest of the family (timeSeriesDeltaToGrid, timeSeriesDerivToGrid, …) once
// each is differentially proven equivalent to its PromQL counterpart.
//
// Every entry renders through the IDENTICAL emitRangeWindowNative shape — the
// `(start, end, step_s, Range_s)(ts, value)` parametric aggregate paired with a
// lockstep timeSeriesRange axis — because the whole family shares one paren/arg
// signature. The per-func difference is purely the aggregate NAME and the
// ClickHouse version floor at which it first shipped:
//
//   - rate    -> timeSeriesRateToGrid    (family floor v25.6, >= 2 samples/window)
//   - changes -> timeSeriesChangesToGrid (v25.9 — PR #86010, >= 1 sample/window)
//   - resets  -> timeSeriesResetsToGrid  (v25.9 — PR #86010, >= 1 sample/window)
//
// changes/resets are COUNT functions (Array(Nullable(Float64)) one count per
// grid point, NULL where no in-window sample), so the same
// `WHERE grid_val IS NOT NULL` filter and `toFloat64` cast apply verbatim. The
// 25.9 floor is enforced upstream by the chopt feature gate (FeatureTSGridChanges
// / FeatureTSGridResets, MinVersion 25.9) — the emitter is version-agnostic and
// only needs the name.
var nativeTSGridFn = map[string]string{
	"rate":    "timeSeriesRateToGrid",
	"changes": "timeSeriesChangesToGrid",
	"resets":  "timeSeriesResetsToGrid",
}

// emitRangeWindowNative renders a chplan.RangeWindowNative — the
// experimental ClickHouse-native lowering of an eligible matrix range
// function (`rate` / `changes` / `resets`) over a query_range expression.
// The aggregate NAME is selected per r.Func via nativeTSGridFn; the SQL
// SHAPE is identical across the family. It produces EXACTLY the
// per-(series, anchor) row shape the matching fan-out matrix path produces
// (emitWindowedArrayExtrapolatedMatrix for rate; emitRangeWindowChanges /
// emitRangeWindowResets for changes / resets), so the wrapping outer
// Aggregate is byte-for-byte unaffected by the substitution. Shown for
// rate; changes / resets swap only the aggregate name (and emit a per-window
// COUNT rather than an extrapolated rate):
//
//	SELECT <group cols>, anchor_ts, anchor_ts AS <TimestampColumn>,
//	       toFloat64(grid_val) AS <ValueColumn>
//	FROM (
//	  SELECT <group cols>,
//	         timeSeriesRateToGrid(<start>, <end>, <step_s>, <window_s>)(<ts>, <val>) AS grid,
//	         timeSeriesRange(<start>, <end>, <step_s>) AS grid_ts
//	  FROM (<inner Scan/Filter>)
//	  GROUP BY <group cols>
//	)
//	ARRAY JOIN grid AS grid_val, grid_ts AS anchor_ts
//	WHERE grid_val IS NOT NULL
//
// Load-bearing details, each matching a fan-out invariant:
//
//   - The `ARRAY JOIN grid AS grid_val, grid_ts AS anchor_ts` explodes
//     the two parallel arrays IN LOCKSTEP, so each rate value lands on
//     the same row as its timeSeriesRange anchor — guaranteeing the
//     anchor_ts column is the same grid the fan-out walks.
//   - `WHERE grid_val IS NOT NULL` converts the native NULL cells into
//     ABSENT rows, exactly what the fan-out's `WHERE length(window_vals)`
//     guard does. The per-func NULL threshold matches the fan-out: rate's
//     timeSeriesRateToGrid NULLs a window with < 2 samples (mirroring the
//     fan-out's `>= 2`); changes/resets' timeSeriesChangesToGrid /
//     timeSeriesResetsToGrid require only >= 1 sample (mirroring the
//     fan-out's `>= 1`), so a single-sample window emits a 0 count rather
//     than NULL. Without this filter, NULLs would flow into the outer
//     aggregate and diverge from Prom's drop-series semantics.
//   - `toFloat64(grid_val)` strips the Nullable so the Value column is a
//     non-nullable Float64 — load-bearing for prod clickhouse-go
//     strictness (chDB tolerates Nullable; prod 502s). The IS NOT NULL
//     filter has already removed every NULL, so the cast never sees one.
//   - anchor_ts is surfaced BOTH bare (RangeWindowAnchorAlias) and under
//     the schema TimestampColumn name, mirroring
//     emitWindowedArrayExtrapolatedMatrix so the wrapping Aggregate's
//     per-step GROUP BY (ColumnRef{TimestampColumn}) resolves.
//
// The experimental setting `allow_experimental_time_series_aggregate_functions=1`
// is NOT emitted here (it is not SQL the plan carries) — the engine
// detects the RangeWindowNative node in the plan and stamps the setting
// onto the per-query ClickHouse context (see internal/engine +
// internal/chclient).
func (e *emitter) emitRangeWindowNative(r *chplan.RangeWindowNative) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindowNative.TimestampColumn unset", ErrUnsupported)
	}
	if r.ValueColumn == "" {
		return fmt.Errorf("%w: RangeWindowNative.ValueColumn unset", ErrUnsupported)
	}
	if r.Step <= 0 {
		return fmt.Errorf("%w: RangeWindowNative requires Step > 0 (range mode)", ErrUnsupported)
	}
	fnName, ok := nativeTSGridFn[r.Func]
	if !ok {
		return fmt.Errorf("%w: RangeWindowNative func %q (supported: rate, changes, resets)", ErrUnsupported, r.Func)
	}

	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	// The grid bounds fold the Offset modifier the same way the fan-out
	// does: both Start and End shift left by Offset so the window becomes
	// [End - Offset - Range, End - Offset]. offsetShiftedTimeFrag renders
	// the bare DateTime64 literal when Offset is zero (the common case).
	offsetNS := r.Offset.Nanoseconds()
	startFrag := offsetShiftedTimeFrag(r.Start, offsetNS)
	endFrag := offsetShiftedTimeFrag(r.End, offsetNS)
	stepSeconds := int64(r.Step.Seconds())
	windowSeconds := int64(r.Range.Seconds())

	// timeSeriesRateToGrid(start, end, step_s, window_s)(ts, value) — the
	// compiled C++ aggregate that returns the per-grid-point
	// extrapolatedRate as Array(Nullable(Float64)). Note the TWO paren
	// groups (params then args), rendered by Parametric.
	gridAgg := Parametric(
		fnName,
		[]Frag{startFrag, endFrag, InlineLit(stepSeconds), InlineLit(windowSeconds)},
		Col(r.TimestampColumn),
		Col(r.ValueColumn),
	)
	// timeSeriesRange(start, end, step_s) — the parallel anchor-timestamp
	// axis. Its i-th element is the anchor of gridAgg's i-th value, so the
	// ARRAY JOIN below pairs them 1:1.
	gridTS := Call("timeSeriesRange", startFrag, endFrag, InlineLit(stepSeconds))

	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}

	// Inner SELECT — one row per series carrying the (grid, grid_ts) pair.
	inner := NewQuery().From(innerSub)
	for _, g := range groupFrags {
		inner.Select(g)
	}
	inner.Select(As(gridAgg, nativeGridArrayAlias))
	inner.Select(As(gridTS, nativeGridTSAlias))
	// Prune the inner scan to the offset-shifted half-open grid span
	// `(Start - Offset - Range, End - Offset]` BEFORE the per-series GROUP
	// BY so ClickHouse skips granules outside the eval window — the
	// timeSeries*ToGrid aggregate otherwise consumes every retained sample
	// of every matching series. Gated on Start/End (always pinned on this
	// node, but kept for a single uniform contract with the fan-out shapes).
	maybePushRangeScanTimeBound(inner, r.TimestampColumn, r.Start, r.End, offsetNS, r.Range.Nanoseconds())
	// GroupBy is a no-op on an empty slice, so no length guard is needed.
	inner.GroupBy(groupFrags...)

	// Outer SELECT — explode the parallel arrays in lockstep, drop NULL
	// cells, cast to a non-nullable Float64, and surface anchor_ts under
	// both the bare alias and the schema timestamp column name.
	outer := NewQuery().From(inner.Frag())
	for _, g := range groupFrags {
		outer.Select(g)
	}
	outer.Select(Col(RangeWindowAnchorAlias))
	if r.TimestampColumn != RangeWindowAnchorAlias {
		outer.Select(As(Col(RangeWindowAnchorAlias), r.TimestampColumn))
	}
	outer.Select(As(Call("toFloat64", Col(nativeGridValAlias)), r.ValueColumn))
	outer.ArrayJoin(
		As(Col(nativeGridArrayAlias), nativeGridValAlias),
		As(Col(nativeGridTSAlias), RangeWindowAnchorAlias),
	)
	outer.Where(IsNotNull(Col(nativeGridValAlias)))

	e.emitSelect(outer)
	return nil
}

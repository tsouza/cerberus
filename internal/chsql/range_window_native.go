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
// rest of the family (timeSeriesDeltaToGrid, …) once each is differentially
// proven equivalent to its PromQL counterpart.
//
// Every entry renders through the IDENTICAL emitRangeWindowNative shape — the
// `(start, end, step_s, Range_s[, predict_offset_s])(ts, value)` parametric
// aggregate paired with a lockstep timeSeriesRange axis — because the whole
// family shares one paren/arg signature. The per-func difference is the
// aggregate NAME, the ClickHouse version floor at which it first shipped, and
// (predict_linear only) one extra trailing parametric arg:
//
//   - rate           -> timeSeriesRateToGrid          (shipped v25.6, >= 2 samples/window)
//   - changes        -> timeSeriesChangesToGrid       (v25.9 — PR #86010, >= 1 sample/window)
//   - resets         -> timeSeriesResetsToGrid        (v25.9 — PR #86010, >= 1 sample/window)
//   - deriv          -> timeSeriesDerivToGrid         (v25.8 — PR #84328, >= 2 samples/window)
//   - predict_linear -> timeSeriesPredictLinearToGrid (v25.8 — PR #84328, >= 2 samples/window, +predict_offset)
//
// changes/resets are COUNT functions (Array(Nullable(Float64)) one count per
// grid point, NULL where no in-window sample); rate/deriv/predict_linear return
// one Nullable(Float64) value per grid point (NULL where the window has < 2
// samples, mirroring PromQL's drop-series). Either way the same
// `WHERE grid_val IS NOT NULL` filter and `toFloat64` cast apply verbatim. The
// whole family is gated to a 25.9 floor by the chopt registry: changes/resets
// because the aggregates only ship at 25.9, rate because its membership window
// was CLOSED until 25.9 (PR #86588 made it left-open / right-closed to match
// PromQL — see internal/chopt FeatureTSGridRange), and deriv/predict_linear
// (which shipped a quarter earlier at 25.8) pinned to the same 25.9 floor for
// one uniform capability verdict. The emitter is version-agnostic and only
// needs the name (plus predict_linear's offset scalar).
var nativeTSGridFn = map[string]string{
	"rate":           "timeSeriesRateToGrid",
	"changes":        "timeSeriesChangesToGrid",
	"resets":         "timeSeriesResetsToGrid",
	"deriv":          "timeSeriesDerivToGrid",
	"predict_linear": "timeSeriesPredictLinearToGrid",
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
//	       toFloat64(assumeNotNull(grid_val)) AS <ValueColumn>
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
//   - `toFloat64(assumeNotNull(grid_val))` strips the Nullable so the Value
//     column is a non-nullable Float64 — load-bearing for prod clickhouse-go
//     strictness (chDB tolerates Nullable; prod 502s, including when a wrapper
//     like count_values lifts Value into a Map(String, Nullable(String)) label).
//     `toFloat64` ALONE does NOT strip Nullable — toFloat64(Nullable(Float64))
//     is still Nullable(Float64) — so assumeNotNull is required; the IS NOT NULL
//     filter has already removed every NULL, so it never drops a real value.
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
// nativeGridTsAxisFrag renders the timestamp axis fed as the FIRST aggregate
// argument to the timeSeries*ToGrid family, selecting the unit the aggregate
// must see per function.
//
// The regression pair (deriv / predict_linear) needs a WHOLE-SECOND axis;
// everything else keeps the schema's native DateTime64(9) column.
//
//   - deriv -> timeSeriesDerivToGrid returns the least-squares slope in
//     value-per-(timestamp tick). Fed the raw DateTime64(9) column the tick is a
//     NANOSECOND, so the slope comes out 1e9x too small vs Prometheus's
//     per-second deriv (empirically 1.8e-10 where the fan-out yields 0.18).
//     Truncating the axis to whole seconds via toDateTime makes the tick a
//     second, so the slope is per-second — byte-identical to the fan-out.
//   - predict_linear -> the same whole-second axis makes it byte-match the
//     fan-out too. The fan-out computes BOTH regression functions through
//     windowPairsSLRFrag, whose x-axis is dateDiff('second', anchor, ts) — a
//     whole-second grid. Matching that exact quantisation (rather than the raw
//     sub-second column) is what makes native == fan-out == Prometheus for
//     whole-second-aligned samples; on the raw axis predict_linear only diverged
//     by float-order noise, but the whole-second axis makes it exact.
//   - rate is window-normalised (its result is an increase divided by the
//     window seconds param, not a raw per-tick slope) and changes/resets are
//     integer counts, so all three are timestamp-tick-invariant and keep the
//     native DateTime64(9) column untouched — no golden churn, no change to
//     their sub-second sample-membership behaviour.
//
// LIMITATION (regression path only) — the sub-second membership gap. The
// returned axis is the aggregate's ts argument, and the aggregate accepts only a
// DateTime/DateTime64 timestamp (it rejects Float64/Decimal), so that ONE
// argument drives BOTH the least-squares x-axis AND the window-membership
// bucketing. There is therefore no way to keep a whole-second x-axis while
// bucketing membership on the raw timestamp: whole-second (toDateTime) floors
// both, and any sub-second type (DateTime64) makes both sub-second. Truncating to
// whole seconds quantises membership — a sub-second sample straddling a
// grid-window boundary buckets by its floored second here, whereas the fan-out
// (and Prometheus) decide membership on the raw timestamp — so such a boundary
// sample can land in a different window between the two paths. The gap is pinned
// by TestNativeTSGrid{Deriv,PredictLinear}_SubSecondMembershipPin.
//
// Why whole-second is nonetheless the chosen axis, and why the family stays
// experimental (CERBERUS_EXPERIMENTAL_TS_GRID_RANGE, default-off):
//
//   - deriv: feeding the raw DateTime64(9) axis and scaling the slope by 1e9
//     (per-nanosecond -> per-second) actually yields the MORE correct answer —
//     raw-ts membership + fractional-second x, i.e. exactly Prometheus's deriv —
//     and it is numerically sound at production ns magnitude (~1.77e18), because
//     the least-squares slope is a centered difference, not an absolute-magnitude
//     sum, so it does NOT overrun float64's exact range (empirically raw-ns * 1e9
//     equals the closed-form slope to full precision; see the sub-second pin).
//     The whole-second axis is kept only because it stays BIT-IDENTICAL to the
//     fan-out, whose own x-axis is the floored dateDiff('second', anchor, ts) —
//     the raw-ns form diverges from that fan-out by ~2 ULP (the 1e9 multiply
//     rounds). Switching deriv to raw-ns would thus improve correctness but
//     require relaxing the bit-identical guard, and would not promote the family
//     on its own (see predict_linear).
//   - predict_linear: raw-ns is genuinely BROKEN, not merely a guard mismatch.
//     Its result is an ABSOLUTE forecast (intercept + slope*(anchor + offset)),
//     evaluated at ~1.77e18 ns, where catastrophic cancellation destroys all
//     precision (empirically ~6.6e11 for a true value in the hundreds). There is
//     no scale trick to recover it, so predict_linear cannot be made
//     sub-second-correct via the native aggregate at all. Because the flag gates
//     the whole regression family, this inherent predict_linear limitation is
//     what keeps the family default-off.
//
// The dual-emit parity tests prove bit-identity on whole-second-aligned seeds
// only; the sub-second pins characterise the divergence beyond that.
func nativeGridTsAxisFrag(fn, tsColumn string) Frag {
	if fn == "deriv" || fn == "predict_linear" {
		return Call("toDateTime", Col(tsColumn))
	}
	return Col(tsColumn)
}

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
		return fmt.Errorf("%w: RangeWindowNative func %q (supported: rate, changes, resets, deriv, predict_linear)", ErrUnsupported, r.Func)
	}
	// predict_linear threads its future-offset horizon t (whole seconds) as the
	// 5th parametric arg of timeSeriesPredictLinearToGrid. The PromQL lowering
	// only wires the native path for a single whole-second literal t (computed /
	// fractional horizons stay on the fan-out), so a predict_linear node must
	// carry exactly one Scalar; any other func must carry none.
	if r.Func == "predict_linear" {
		if len(r.Scalars) != 1 {
			return fmt.Errorf("%w: RangeWindowNative predict_linear requires exactly 1 scalar (t), got %d", ErrUnsupported, len(r.Scalars))
		}
	} else if len(r.Scalars) != 0 {
		return fmt.Errorf("%w: RangeWindowNative func %q takes no scalar, got %d", ErrUnsupported, r.Func, len(r.Scalars))
	}

	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	// The grid bounds fold the Offset modifier the same way the fan-out
	// does: both Start and End shift left by Offset so the window becomes
	// [End - Offset - Range, End - Offset]. nativeGridTimeBoundFrag renders
	// the whole-second DateTime literal this timeSeries*ToGrid aggregate's
	// start_timestamp/end_timestamp parameters are documented as accepting
	// (see its doc comment) — NOT the DateTime64(9) offsetShiftedTimeFrag
	// produces, which this family's argument coercion cannot always digest.
	offsetNS := r.Offset.Nanoseconds()
	startFrag := nativeGridTimeBoundFrag(r.Start, offsetNS)
	endFrag := nativeGridTimeBoundFrag(r.End, offsetNS)
	stepSeconds := int64(r.Step.Seconds())
	windowSeconds := int64(r.Range.Seconds())

	// timeSeriesRateToGrid(start, end, step_s, window_s)(ts, value) — the
	// compiled C++ aggregate that returns the per-grid-point value as
	// Array(Nullable(Float64)). Note the TWO paren groups (params then args),
	// rendered by Parametric. predict_linear appends its whole-second horizon t
	// as a 5th param: timeSeriesPredictLinearToGrid(start, end, step_s,
	// window_s, predict_offset_s)(ts, value).
	gridParams := []Frag{startFrag, endFrag, InlineLit(stepSeconds), InlineLit(windowSeconds)}
	if r.Func == "predict_linear" {
		gridParams = append(gridParams, InlineLit(int64(r.Scalars[0])))
	}
	gridAgg := Parametric(
		fnName,
		gridParams,
		nativeGridTsAxisFrag(r.Func, r.TimestampColumn),
		Col(r.ValueColumn),
	)
	// timeSeriesRange(start, end, step_s) — the parallel anchor-timestamp
	// axis. Its i-th element is the anchor of gridAgg's i-th value, so the
	// ARRAY JOIN below pairs them 1:1. It MUST render the UNSHIFTED query grid
	// [Start, End]: the anchor axis becomes the emitted Timestamp column, and
	// Offset must NOT move the reported timestamps — it shifts only the
	// aggregate's membership window (gridAgg's start/end), mirroring the
	// fan-out, which reports the query-grid anchor while selecting from the
	// (anchor-Offset-Range, anchor-Offset] span. nativeGridTimeBoundFrag(_, 0)
	// renders the same whole-second literal shape as startFrag/endFrag above
	// (offset 0), keeping this axis and gridAgg's in lockstep.
	gridStartFrag := nativeGridTimeBoundFrag(r.Start, 0)
	gridEndFrag := nativeGridTimeBoundFrag(r.End, 0)
	gridTS := Call("timeSeriesRange", gridStartFrag, gridEndFrag, InlineLit(stepSeconds))

	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}

	// Inner SELECT — one row per series carrying the (grid, grid_ts) pair.
	inner := NewQuery().From(innerSub)
	inner.Select(groupFrags...)
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
	outer.Select(groupFrags...)
	outer.Select(As(nativeAnchorTimestampFrag(), RangeWindowAnchorAlias))
	if r.TimestampColumn != RangeWindowAnchorAlias {
		outer.Select(As(nativeAnchorTimestampFrag(), r.TimestampColumn))
	}
	outer.Select(As(Call("toFloat64", Call("assumeNotNull", Col(nativeGridValAlias))), r.ValueColumn))
	outer.ArrayJoin(
		As(Col(nativeGridArrayAlias), nativeGridValAlias),
		As(Col(nativeGridTSAlias), RangeWindowAnchorAlias),
	)
	outer.Where(IsNotNull(Col(nativeGridValAlias)))

	e.emitSelect(outer)
	return nil
}

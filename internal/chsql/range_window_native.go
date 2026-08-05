package chsql

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/tsouza/cerberus/internal/chplan"
)

// nativeGridArrayAlias / nativeGridTSAlias / nativeGridValAlias name the
// inner-subquery columns the native timeSeriesRateToGrid emit threads
// through the ARRAY JOIN explosion; nativeGridStateAlias and
// nativeShapedKeyAliasPrefix name the two extra columns the deferred
// label-shaping (chplan.RangeWindowNative.Recollapse) shape adds beneath
// them. Lifting them to named constants keeps the producer (inner SELECT)
// and consumer (ARRAY JOIN + outer SELECT) referring to the exact same
// identifier — a typo in one would otherwise surface only as a CH
// `Unknown identifier` at execution time.
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
	// nativeGridStateAlias is the per-RAW-series partial aggregation state
	// (AggregateFunction(timeSeries*ToGrid, …)) the inner level of the
	// deferred-shaping shape emits and the middle level merges. It exists
	// only in that shape — the two-level shape's inner level emits the
	// finished nativeGridArrayAlias grid directly.
	nativeGridStateAlias = "grid_state"
	// nativeShapedKeyAliasPrefix + <i> names the middle level's i-th SHAPED
	// series key. The alias is emitter-owned and synthetic: the plan carries
	// only the FINAL output name (Projection.Alias), which the outer level
	// renames the key to. It deliberately is NOT that output name — aliasing
	// the shaped map back to `Attributes` on the level that carries the GROUP
	// BY would rebind the GROUP BY key to the shaped column, so ClickHouse
	// would compute the pre-hoist thing at the pre-hoist cost while still
	// answering correctly: a rewrite that is silently inert.
	nativeShapedKeyAliasPrefix = "shaped_key_"
)

// nativeShapedKeyAlias names the middle level's i-th deferred-shaping key.
func nativeShapedKeyAlias(i int) string {
	return nativeShapedKeyAliasPrefix + strconv.Itoa(i)
}

// nativeTSGridAgg names one member of the timeSeries*ToGrid family: the plain
// aggregate the two-level shape emits, plus the -State / -Merge combinator
// pair the deferred-shaping shape needs. Fn is always set; the pair is set
// only for functions proven exact under a merge of partial states.
type nativeTSGridAgg struct {
	Fn      string
	StateFn string
	MergeFn string
}

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
//
// StateFn / MergeFn name the aggregate's partial-state combinator pair, which
// the deferred label-shaping shape (chplan.RangeWindowNative.Recollapse)
// needs: the inner level emits <fn>ToGridState per RAW series and the middle
// level <fn>ToGridMerge's the states of every raw series that shapes onto the
// same output series. An EMPTY MergeFn declares the function NOT
// re-collapse-eligible, and a node that carries Recollapse anyway is REJECTED
// (ErrUnsupported) rather than emitted without the re-collapse: dropping it
// would group on the RAW attribute map and answer with the wrong series
// identity — two half-series where the query asks for one pooled series — a
// silent wrong answer rather than a perf regression. Only rate is populated in
// this first cut because only rate is wired at the lowering seam; the rest of
// the family gains its pair when its lowering does.
var nativeTSGridFn = map[string]nativeTSGridAgg{
	"rate": {
		Fn:      "timeSeriesRateToGrid",
		StateFn: "timeSeriesRateToGridState",
		MergeFn: "timeSeriesRateToGridMerge",
	},
	"changes":        {Fn: "timeSeriesChangesToGrid"},
	"resets":         {Fn: "timeSeriesResetsToGrid"},
	"deriv":          {Fn: "timeSeriesDerivToGrid"},
	"predict_linear": {Fn: "timeSeriesPredictLinearToGrid"},
}

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
// A node carrying r.Recollapse renders the SAME outer level over a THREE-level
// body — the deferred label shaping. The OTel → Prometheus shaping tower moves
// ABOVE the aggregate, so it evaluates once per raw SERIES instead of once per
// raw ROW (the dominant cost of the pre-hoist shape):
//
//	SELECT <pass-through keys>, shaped_key_0 AS <Recollapse[0].Alias>, anchor_ts, …
//	FROM (
//	  SELECT <pass-through keys>,
//	         <shaping tower> AS shaped_key_0,
//	         timeSeriesRateToGridMerge(<start>, <end>, <step_s>, <window_s>)(grid_state) AS grid,
//	         timeSeriesRange(<start>, <end>, <step_s>) AS grid_ts
//	  FROM (
//	    SELECT <group cols>,
//	           timeSeriesRateToGridState(<start>, <end>, <step_s>, <window_s>)(<ts>, <val>) AS grid_state
//	    FROM (<inner Scan/Filter>)
//	    GROUP BY <group cols>
//	  )
//	  GROUP BY <pass-through keys>, shaped_key_0
//	)
//	ARRAY JOIN … WHERE … (verbatim the two-level tail)
//
// Three details there are load-bearing, and each one is a SILENT wrong answer
// rather than an error when it is dropped:
//
//   - The shaped key carries the synthetic shaped_key_<i> alias on the level
//     that groups by it and is renamed to its output name only at the outer
//     level. Aliasing it back to `Attributes` on the middle level rebinds that
//     level's GROUP BY to the shaped column, so ClickHouse computes the
//     pre-hoist thing at the pre-hoist cost and still answers correctly.
//   - The re-collapse merges partial aggregation STATES, never finished grids.
//     The shaping is non-injective (distinct raw series shape onto one output
//     series) and the collapsing raw series are time-disjoint, so a grid point
//     whose window straddles the splice depends on the POOLED sample set;
//     element-wise arithmetic over two finished grids answers a different
//     number there. Merging states IS pooling samples, which is why
//     eligibility is per aggregate function (nativeTSGridAgg.MergeFn) rather
//     than per plan shape.
//   - GroupBy keys that no shaping expression reads (MetricName and friends)
//     are projected VERBATIM at every level. Projecting only the shaped keys
//     drops MetricName from the output series identity, pooling two different
//     metrics into one series.
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
	agg, ok := nativeTSGridFn[r.Func]
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

	// Every deferred-shaping guard runs BEFORE anything is rendered: a node
	// the emitter cannot re-collapse must fail loudly, never fall back to the
	// two-level shape, which would group on the RAW attribute map and answer
	// with a split, unshaped series identity.
	var (
		passIdx         []int
		shapedInputIdx  []int
		groupFrags      []Frag
		recollapseFrags []Frag
		err             error
	)
	if len(r.Recollapse) == 0 {
		if groupFrags, err = e.collectGroupByFrags(r.GroupBy); err != nil {
			return err
		}
	} else {
		var groupCols []string
		if groupCols, err = requireRecollapseEmittable(r, agg); err != nil {
			return err
		}
		passIdx, shapedInputIdx = r.PartitionRecollapseGroupBy(groupCols)
		if groupFrags, recollapseFrags, err = e.collectRecollapseFrags(r, passIdx, shapedInputIdx); err != nil {
			return err
		}
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
	tsAxis := nativeGridTsAxisFrag(r.Func, r.TimestampColumn)
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

	// arrayLevel is the sub-SELECT that hands the outer level its (grid,
	// grid_ts) pair — the inner level in the two-level shape, the middle
	// (merge) level in the three-level one. outerKeyFrags is the series
	// identity the outer level projects alongside it.
	var arrayLevel *QueryBuilder
	var outerKeyFrags []Frag

	if len(r.Recollapse) == 0 {
		// Inner SELECT — one row per series carrying the (grid, grid_ts) pair.
		inner := NewQuery().From(innerSub)
		inner.Select(groupFrags...)
		inner.Select(As(Parametric(agg.Fn, gridParams, tsAxis, Col(r.ValueColumn)), nativeGridArrayAlias))
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

		arrayLevel = inner
		outerKeyFrags = groupFrags
	} else {
		// Inner (state) SELECT — one PARTIAL AGGREGATION STATE per RAW series,
		// grouped on the raw keys. Identical to the two-level inner level but
		// for the -State combinator and the state alias; the scan bound is the
		// same one, on the same level, so the hoist changes no granule pruning.
		state := NewQuery().From(innerSub)
		state.Select(groupFrags...)
		state.Select(As(Parametric(agg.StateFn, gridParams, tsAxis, Col(r.ValueColumn)), nativeGridStateAlias))
		maybePushRangeScanTimeBound(state, r.TimestampColumn, r.Start, r.End, offsetNS, r.Range.Nanoseconds())
		state.GroupBy(groupFrags...)

		// Middle (merge) SELECT — evaluate the shaping tower once per raw
		// series and merge the states of every raw series that shapes onto the
		// same output key. The pass-through identity keys are projected and
		// grouped VERBATIM; only the shaped keys change identity here.
		merge := NewQuery().From(state.Frag())
		mergeKeys := make([]Frag, 0, len(passIdx)+len(r.Recollapse))
		for _, i := range passIdx {
			merge.Select(groupFrags[i])
			mergeKeys = append(mergeKeys, groupFrags[i])
		}
		for i := range r.Recollapse {
			merge.SelectAs(recollapseFrags[i], nativeShapedKeyAlias(i))
			mergeKeys = append(mergeKeys, Col(nativeShapedKeyAlias(i)))
		}
		// The -Merge form takes ONE argument, the state column — the (ts,
		// value) pair was already consumed by the -State level. Its parametric
		// tuple is the SAME (start, end, step, window) the state carries: the
		// two must describe the same grid or the merged states land on
		// mismatched anchors.
		merge.Select(As(Parametric(agg.MergeFn, gridParams, Col(nativeGridStateAlias)), nativeGridArrayAlias))
		merge.Select(As(gridTS, nativeGridTSAlias))
		merge.GroupBy(mergeKeys...)

		arrayLevel = merge
		outerKeyFrags = make([]Frag, 0, len(passIdx)+len(r.Recollapse))
		for _, i := range passIdx {
			outerKeyFrags = append(outerKeyFrags, groupFrags[i])
		}
		for i := range r.Recollapse {
			outerKeyFrags = append(outerKeyFrags, As(Col(nativeShapedKeyAlias(i)), r.Recollapse[i].Alias))
		}
	}

	// Outer SELECT — explode the parallel arrays in lockstep, drop NULL
	// cells, cast to a non-nullable Float64, and surface anchor_ts under
	// both the bare alias and the schema timestamp column name.
	outer := NewQuery().From(arrayLevel.Frag())
	outer.Select(outerKeyFrags...)
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

	return e.emitSelect(outer)
}

// requireRecollapseEmittable rejects every deferred-shaping node the emitter
// cannot render into a sound three-level shape, and returns the validated
// GroupBy key names the partition is taken over. Each rejection is a wrong
// ANSWER the shape would otherwise produce silently, so all of them are hard
// errors rather than a fallback to the two-level shape:
//
//   - no -State / -Merge pair registered for the function: the re-collapse
//     would have to be dropped, leaving the query grouped on the raw attribute
//     map — two half-series where the caller asked for one pooled series;
//   - an empty alias: the shaped key would reach the outer level unnamed, so
//     the output carries the emitter's synthetic shaped_key_<i> name;
//   - a duplicate alias: two shaped keys would collapse onto one output
//     column, silently discarding one of them;
//   - a computed GroupBy key: the levels above the state one reference every
//     key BY NAME, and a computed key has no name to reference.
func requireRecollapseEmittable(r *chplan.RangeWindowNative, agg nativeTSGridAgg) ([]string, error) {
	if agg.StateFn == "" || agg.MergeFn == "" {
		return nil, fmt.Errorf("%w: RangeWindowNative func %q has no -State/-Merge pair, so Recollapse cannot be deferred past the aggregate",
			ErrUnsupported, r.Func)
	}
	seen := make(map[string]struct{}, len(r.Recollapse))
	for i, p := range r.Recollapse {
		if p.Alias == "" {
			return nil, fmt.Errorf("%w: RangeWindowNative.Recollapse[%d] has an empty Alias", ErrUnsupported, i)
		}
		if _, dup := seen[p.Alias]; dup {
			return nil, fmt.Errorf("%w: RangeWindowNative.Recollapse alias %q is not unique", ErrUnsupported, p.Alias)
		}
		seen[p.Alias] = struct{}{}
	}
	groupCols, computed := r.GroupByColumns()
	if computed != nil {
		return nil, fmt.Errorf("%w: RangeWindowNative.Recollapse requires plain column GroupBy keys, got %T", ErrUnsupported, computed)
	}
	if err := requireRecollapseColumnsGrouped(r, groupCols); err != nil {
		return nil, err
	}
	return groupCols, nil
}

// requireRecollapseColumnsGrouped enforces the containment invariant of the
// three-level shape: every column a deferred shaping expression reads is
// projected by the inner (state) level, which projects exactly the GroupBy
// keys. A shaping expression reading a column the inner GROUP BY does not
// carry renders SQL ClickHouse rejects with UNKNOWN_IDENTIFIER (a 502 in
// production). It is also what makes ProjectionPushdown's column enumeration
// sufficient — nativeRangeWindowColumns can reach every read through GroupBy
// alone.
//
// The read set comes from chplan.RangeWindowNative.RecollapseReadColumns, the
// same walk chplan.RangeWindowNative.PartitionRecollapseGroupBy answers from, so
// this guard and the partition cannot disagree about which GroupBy key is a
// shaping input. Its first-read ordering is what keeps the message deterministic
// when several reads are ungrouped.
func requireRecollapseColumnsGrouped(r *chplan.RangeWindowNative, groupCols []string) error {
	for _, name := range r.RecollapseReadColumns() {
		if !slices.Contains(groupCols, name) {
			return fmt.Errorf("%w: RangeWindowNative.Recollapse reads column %q which is not a GroupBy key", ErrUnsupported, name)
		}
	}
	return nil
}

// collectRecollapseFrags renders the three-level shape's GroupBy keys and
// deferred shaping expressions in the order the emitted SQL first references
// each of them: the pass-through keys are written first by the OUTER SELECT
// list, the shaping expressions next by the MIDDLE (merge) SELECT list, and
// the remaining raw keys last by the INNER (state) SELECT list.
//
// The order matters because collectGroupByFrags appends the args it captures
// to e.args AT CALL TIME (see its doc), so a call sequence that does not match
// the rendered-SQL sequence binds those args at the wrong `?` positions. Every
// expression the lowering puts in either slot is arg-free today (bare
// ColumnRefs, InlineStrings), which makes the ordering unobservable — it is
// honoured anyway so a future parameterised shaping expression is a correct
// query rather than a production 502.
//
// The returned groupFrags stay positionally aligned with r.GroupBy (the merge
// and outer levels index it by GroupBy position); only the order in which they
// were rendered differs.
func (e *emitter) collectRecollapseFrags(r *chplan.RangeWindowNative, passIdx, shapingInputIdx []int) (groupFrags, recollapseFrags []Frag, err error) {
	groupFrags = make([]Frag, len(r.GroupBy))
	collectInto := func(idx []int) error {
		exprs := make([]chplan.Expr, 0, len(idx))
		for _, i := range idx {
			exprs = append(exprs, r.GroupBy[i])
		}
		frags, err := e.collectGroupByFrags(exprs)
		if err != nil {
			return err
		}
		for n, i := range idx {
			groupFrags[i] = frags[n]
		}
		return nil
	}
	if err = collectInto(passIdx); err != nil {
		return nil, nil, err
	}
	shaped := make([]chplan.Expr, len(r.Recollapse))
	for i := range r.Recollapse {
		shaped[i] = r.Recollapse[i].Expr
	}
	if recollapseFrags, err = e.collectGroupByFrags(shaped); err != nil {
		return nil, nil, err
	}
	if err = collectInto(shapingInputIdx); err != nil {
		return nil, nil, err
	}
	return groupFrags, recollapseFrags, nil
}

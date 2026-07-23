package chplan

import "time"

// RangeWindowNative is the experimental, ClickHouse-native lowering of a
// `rate(<counter>[<Range>])` range query (query_range, Step > 0). It is
// the opt-in counterpart to RangeWindow's arrayJoin fan-out: instead of
// fanning every sample across its covered anchors and re-grouping into
// per-window arrays, the emitter renders ClickHouse's compiled
// `timeSeriesRateToGrid` aggregate, which computes the Prometheus
// `extrapolatedRate` value at every grid point in one C++ pass.
//
// The node is produced by the PromQL lowering ONLY when ALL of:
//
//   - The boot-wired native-rate strategy is active (the ts_grid_range
//     feature, resolved once at boot from chopt.EnabledSet and injected into
//     the lowering as a RangeLowerers strategy — never read per query). Default
//     off.
//   - Func == "rate" (the first cut; increase / delta have no dedicated
//     timeSeries*ToGrid aggregate proven equivalent to Prom's
//     funcIncrease / funcDelta yet, so they stay on the fan-out).
//   - The query is in range mode: Step > 0 and both Start and End are
//     pinned (the materialised query_range grid). Instant queries
//     (Step == 0) have no grid and are never eligible.
//   - The inner relation is a plain Scan / Filter (a row-shape relation
//     carrying the per-sample (TimestampColumn, ValueColumn) pair) — the
//     same shape RangeWindow's row-shape matrix emitter handles. Inputs
//     that route through MetricsAggregate / MetricsHistogramOverTime /
//     MetricsCompare keep their own emit branches and never lower here.
//
// Every other shape lowers to RangeWindow, so the default fan-out is
// structurally untouched when the flag is off.
//
// Row-shape contract. The emitter (internal/chsql/range_window_native.go)
// produces EXACTLY the row shape RangeWindow's matrix path
// (emitWindowedArrayExtrapolatedMatrix) produces: one row per
// (series, anchor_ts) with columns [GroupBy..., anchor_ts (under both the
// RangeWindowAnchorAlias and the schema TimestampColumn name), Value]. The
// wrapping outer-sum Aggregate is therefore byte-for-byte unaffected by
// the substitution. Grid cells with < 2 samples are NULL in
// timeSeriesRateToGrid's Array(Nullable(Float64)) result and are filtered
// to ABSENT rows (matching PromQL's drop-series semantics, exactly what
// the fan-out's `WHERE length(window_vals) >= 2` does).
//
// Required ClickHouse setting. `timeSeriesRateToGrid` is experimental: a
// query carrying this node must run with
// `allow_experimental_time_series_aggregate_functions=1`. The engine
// detects the node in the emitted plan and stamps that setting onto the
// per-query ClickHouse context (see internal/engine + internal/chclient),
// so unrelated queries never carry the experimental knob.
type RangeWindowNative struct {
	Input Node

	// Func is the PromQL range function. The first cut supports "rate"
	// only; the field is retained (rather than implied) so the emitter's
	// per-Func aggregate-name map can generalise to the rest of the
	// timeSeries*ToGrid family behind the same flag later without an IR
	// change.
	Func string

	// Range is the [duration] window from the PromQL source — the
	// staleness / window parameter of timeSeriesRateToGrid (param 4).
	Range time.Duration

	// Step is the query_range grid resolution — param 3 of
	// timeSeriesRateToGrid and the spacing of timeSeriesRange. Always > 0
	// on this node (instant queries are not eligible).
	Step time.Duration

	// Start / End define the query_range eval grid: params 1 and 2 of
	// timeSeriesRateToGrid (and the bounds of the parallel
	// timeSeriesRange ts axis). Both are pinned (non-zero) on this node —
	// the lowering only builds it in range mode.
	Start time.Time
	End   time.Time

	// Offset is the PromQL `offset` modifier folded onto the grid: it
	// shifts both Start and End back at emit time so the window becomes
	// [End - Offset - Range, End - Offset]. Zero means no offset.
	Offset time.Duration

	// TimestampColumn / ValueColumn name the per-sample timestamp / value
	// columns on Input (typically "TimeUnix" / "Value" for OTel-CH) — the
	// two positional arguments of timeSeriesRateToGrid's second paren
	// group.
	TimestampColumn string
	ValueColumn     string

	// GroupBy lists the per-series grouping expressions (typically
	// `[ColumnRef("Attributes")]` for OTel-CH). May be empty, in which
	// case all rows form one series. Same semantics as RangeWindow.GroupBy.
	GroupBy []Expr

	// Scalars carries the literal scalar arguments the native aggregate takes
	// beyond the shared (start, end, step, window) parametric prefix. Only
	// predict_linear uses it today: a single whole-second horizon t threaded
	// into timeSeriesPredictLinearToGrid's 5th parametric arg. Empty for
	// rate / changes / resets / deriv, which take no extra parameter. Mirrors
	// RangeWindow.Scalars; the PromQL lowering gates predict_linear to a
	// whole-second literal before populating it (computed / fractional horizons
	// stay on the fan-out RangeWindow).
	Scalars []float64
}

func (*RangeWindowNative) planNode() {}

func (r *RangeWindowNative) Children() []Node { return []Node{r.Input} }

func (r *RangeWindowNative) Equal(other Node) bool {
	o, ok := other.(*RangeWindowNative)
	if !ok {
		return false
	}
	if r.Func != o.Func || r.Range != o.Range || r.Step != o.Step || r.Offset != o.Offset {
		return false
	}
	if !r.Start.Equal(o.Start) || !r.End.Equal(o.End) {
		return false
	}
	if r.TimestampColumn != o.TimestampColumn || r.ValueColumn != o.ValueColumn {
		return false
	}
	if len(r.Scalars) != len(o.Scalars) {
		return false
	}
	for i := range r.Scalars {
		if r.Scalars[i] != o.Scalars[i] {
			return false
		}
	}
	if len(r.GroupBy) != len(o.GroupBy) {
		return false
	}
	for i := range r.GroupBy {
		if !r.GroupBy[i].Equal(o.GroupBy[i]) {
			return false
		}
	}
	if r.Input == nil || o.Input == nil {
		return r.Input == o.Input
	}
	return r.Input.Equal(o.Input)
}

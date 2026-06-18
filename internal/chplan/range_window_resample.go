package chplan

import "time"

// RangeWindowResample is the experimental, ClickHouse-native lowering of a
// range-mode (query_range, Step > 0) BARE instant-vector selector — the
// staleness / instant-vector-selection shape. It is the opt-in counterpart
// to RangeLWR's bounded argMax sample-fan-out: instead of fanning every
// sample across the anchors whose staleness window covers it and collapsing
// each (series, anchor) bucket with argMax, the emitter renders ClickHouse's
// compiled `timeSeriesResampleToGridWithStaleness` aggregate, which carries
// the latest in-window sample value forward to every grid point in one C++
// pass.
//
// The node is produced by the PromQL lowering ONLY when ALL of:
//
//   - The boot-wired resample strategy is active (the ts_grid_resample
//     feature, resolved once at boot from chopt.EnabledSet and injected into
//     the lowering as a strategy — never read per query). Default off.
//   - The query is in range mode: Step > 0 and both Start and End are pinned
//     (the materialised query_range grid). Instant queries (Step == 0) have
//     no grid and lower to the simple wrapInstantLatestPerSeries Aggregate.
//   - The selector is NOT pinned by an absolute `@` modifier (those route to
//     wrapRangeAbsoluteAtBroadcast upstream, so anchor.End is zero here and
//     only the offset shifts the window).
//
// Every other shape lowers to RangeLWR, so the default fan-out is
// structurally untouched when the feature is off.
//
// Row-shape contract. The emitter (internal/chsql/range_window_resample.go)
// produces EXACTLY the row shape RangeLWR's emitter produces: one row per
// (series, anchor_ts) that had an in-window sample, with columns
// [MetricNameCol, AttributesCol, TimeUnix = anchor_ts, ValueCol]. Grid cells
// with no sample in the staleness window are NULL in
// timeSeriesResampleToGridWithStaleness's Array(Nullable(Float64)) result and
// are filtered to ABSENT rows (matching PromQL's staleness-gap semantics,
// exactly what RangeLWR's "no fanned row -> no GROUP BY row" does).
//
// Window-edge note (the one documented divergence from RangeLWR). RangeLWR's
// membership window is the half-open `(anchor - Offset - Lookback,
// anchor - Offset]` (strict left edge). The native function uses the CLOSED
// left edge `[anchor - Offset - Lookback, anchor - Offset]` — a sample landing
// EXACTLY on the left boundary is included natively but excluded by the
// fan-out. The closed-left form matches reference Prometheus's `t >= refTime -
// lookbackDelta` staleness selection, so the native node is, if anything, the
// MORE Prometheus-faithful of the two; the divergence is a measure-zero,
// nanosecond-exact boundary coincidence that no Prometheus-pinned spec fixture
// exercises. It is called out here (rather than masked) so a future fixture
// that does hit the boundary is read as intended, not a regression.
//
// Required ClickHouse setting. `timeSeriesResampleToGridWithStaleness` is a
// member of the experimental timeSeries*ToGrid family: a query carrying this
// node must run with `allow_experimental_time_series_aggregate_functions=1`.
// The engine detects the node in the emitted plan and stamps that setting onto
// the per-query ClickHouse context (shared with RangeWindowNative via
// planHasTSGridNative), so unrelated queries never carry the experimental knob.
type RangeWindowResample struct {
	Input Node

	// Start / End define the query_range eval grid: params 1 and 2 of
	// timeSeriesResampleToGridWithStaleness (and the bounds of the parallel
	// timeSeriesRange ts axis). Both are pinned (non-zero) on this node — the
	// lowering only builds it in range mode.
	Start time.Time
	End   time.Time

	// Step is the query_range grid resolution — param 3 of
	// timeSeriesResampleToGridWithStaleness and the spacing of timeSeriesRange.
	// Always > 0 on this node (instant queries are not eligible).
	Step time.Duration

	// Lookback is the staleness horizon (instantLookback, default 5m) — param 4
	// of timeSeriesResampleToGridWithStaleness. The per-anchor window is
	// `[anchor - Offset - Lookback, anchor - Offset]` (see the window-edge note
	// above for the closed-vs-half-open distinction from RangeLWR).
	Lookback time.Duration

	// Offset is the PromQL `offset` modifier folded onto the grid: it shifts
	// both Start and End back at emit time so the membership window slides back
	// by Offset WITHOUT moving the emitted anchor timestamp. Zero means no
	// offset.
	Offset time.Duration

	// Column names on Input (canonical OTel-CH: MetricName / Attributes /
	// TimeUnix / Value). TimestampCol / ValueCol are the two positional
	// arguments of the aggregate's second paren group; MetricNameCol /
	// AttributesCol are the per-series GROUP BY identity.
	MetricNameCol string
	AttributesCol string
	TimestampCol  string
	ValueCol      string
}

func (*RangeWindowResample) planNode() {}

func (r *RangeWindowResample) Children() []Node { return []Node{r.Input} }

// Equal compares two RangeWindowResample nodes field-by-field. It is written as
// a single scalar-fields conjunction plus a recursive Input compare (rather than
// RangeLWR's early-return ladder) — the two nodes carry the identical data shape
// (Start/End/Step/Lookback/Offset + the canonical column names), so the compact
// form keeps the comparison total without the boilerplate parallel that couples
// the native and fan-out leaves.
func (r *RangeWindowResample) Equal(other Node) bool {
	o, ok := other.(*RangeWindowResample)
	if !ok {
		return false
	}
	scalarsEqual := r.Start.Equal(o.Start) && r.End.Equal(o.End) &&
		r.Step == o.Step && r.Lookback == o.Lookback && r.Offset == o.Offset &&
		r.MetricNameCol == o.MetricNameCol && r.AttributesCol == o.AttributesCol &&
		r.TimestampCol == o.TimestampCol && r.ValueCol == o.ValueCol
	if !scalarsEqual {
		return false
	}
	if r.Input == nil || o.Input == nil {
		return r.Input == nil && o.Input == nil
	}
	return r.Input.Equal(o.Input)
}

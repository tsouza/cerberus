package chplan

import "time"

// SynthLabel is a single (key, value) entry on the synthesised label
// set emitted by AbsentOverTime for the per-anchor absence rows. The
// pair is rendered as two consecutive arguments to ClickHouse's `map(
// k1, v1, k2, v2, ...)` constructor.
type SynthLabel struct {
	Key   string
	Value string
}

// AbsentOverTime is the dedicated plan node for PromQL
// `absent_over_time(<vector-selector>[<range>])`. It mirrors the
// instant-vector `absent(...)` lowering (see internal/promql/absent.go)
// but operates per evaluation anchor: for every step anchor that has
// zero matching samples in the lookback window `(anchor - Range,
// anchor]`, emit one row with the matcher-derived synthesised label
// set, the anchor timestamp on TimestampColumn, and a constant value
// of 1 on ValueColumn.
//
// Per Prometheus's `funcAbsentOverTime` (see
// prometheus/promql/functions.go), the output is a SINGLE synthesised
// series (not one per input series): the label set is constructed
// only from the equality matchers explicitly named on the input
// selector (mirroring `createLabelsForAbsentFunction`). The samples
// of that single series are placed at exactly the anchors where the
// underlying selector has zero matching samples in its lookback
// window. Anchors with any sample contribute no output.
//
// Cerberus's previous implementation routed `absent_over_time` through
// the regular per-series RangeWindow path which emitted one row per
// (input series, anchor) with `if(length(window_vals) > 0, NaN, 1.0)`
// as the value — carrying the original series labels instead of the
// matcher-derived synthesised labels. That produced wrong labels AND
// extra per-series NaN rows the matrix pivot didn't drop.
//
// The shape this node renders:
//
//	SELECT '' AS MetricName,
//	       <synth-labels-map> AS Attributes,
//	       anchor_ts AS TimeUnix,
//	       toFloat64(1) AS Value
//	FROM (
//	    SELECT arrayJoin(<step grid>) AS anchor_ts, sample_ts_arr
//	    FROM (
//	        SELECT groupArray(`TimeUnix`) AS sample_ts_arr
//	        FROM (<matcher-filtered scan>)
//	        WHERE TimeUnix > '<start - range>' AND TimeUnix <= '<end>'
//	    )
//	) WHERE arrayCount(t -> t > anchor_ts - toIntervalNanosecond(<range_ns>)
//	                        AND t <= anchor_ts,
//	                   sample_ts_arr) = 0
//
// The inner `groupArray(TimeUnix) FROM (<scan>) GROUP BY ()` always
// produces exactly one row — including the case where the matcher
// scan returns zero rows, which yields `sample_ts_arr = []` and lets
// every step anchor in the outer arrayJoin survive the WHERE clause.
// Without that 1-row guarantee, a totally absent metric would CROSS
// JOIN-collapse to zero output rows and we'd lose the "absent at
// every anchor" signal Prom synthesises.
type AbsentOverTime struct {
	// Input is the matcher-filtered scan (Filter wrapping Scan in the
	// canonical case). The emitter wraps it in a `SELECT groupArray(
	// TimestampColumn) FROM (<Input>) WHERE TimestampColumn IN [Start
	// - Range, End]` and projects the resulting `sample_ts_arr` array
	// across the step grid.
	Input Node

	// SynthLabels is the matcher-derived label set rendered onto every
	// emitted output row. Built by the lowering via the same rule
	// `absentAttrsMap` uses for instant `absent(...)`: include only
	// equality matchers, skip `__name__`, drop labels appearing more
	// than once. Empty SynthLabels render as `CAST(map(), 'Map(String,
	// String)')` — the canonical empty-attrs map shape.
	SynthLabels []SynthLabel

	// Range is the PromQL `[range]` window. The per-anchor lookback
	// is `(anchor - Range, anchor]`.
	Range time.Duration

	// Start / End define the query evaluation grid. For range queries
	// the emitter walks anchors `Start, Start+Step, …, End` (inclusive);
	// for instant queries Start == End == eval-anchor and Step is 0.
	Start, End time.Time

	// Step is the eval-grid spacing. Zero means instant mode — the
	// emitter emits a single anchor at End.
	Step time.Duration

	// Offset is the PromQL `offset` modifier shifted onto the inner
	// VectorSelector. Subtracted from each anchor at emit time so the
	// window becomes `(anchor - Offset - Range, anchor - Offset]`.
	Offset time.Duration

	// TimestampColumn names the column carrying the per-sample
	// timestamp on Input (typically `TimeUnix` for OTel-CH).
	TimestampColumn string

	// ValueColumn names the value column emitted on the output row
	// (typically `Value` for OTel-CH). The value itself is the
	// constant 1.0, wrapped in `toFloat64(...)` so the driver projects
	// Float64 on the wire (mirrors the instant `absent` lowering).
	ValueColumn string

	// MetricNameColumn names the column carrying the metric name on
	// the output sample shape (typically `MetricName` for OTel-CH).
	// Always projected as the empty string — Prom's funcAbsentOverTime
	// drops `__name__` from the synthesised output.
	MetricNameColumn string

	// AttributesColumn names the column carrying the label map on
	// the output sample shape (typically `Attributes` for OTel-CH).
	AttributesColumn string
}

func (*AbsentOverTime) planNode() {}

func (a *AbsentOverTime) Children() []Node { return []Node{a.Input} }

func (a *AbsentOverTime) Equal(other Node) bool {
	o, ok := other.(*AbsentOverTime)
	if !ok {
		return false
	}
	if a.Range != o.Range || a.Step != o.Step || a.Offset != o.Offset {
		return false
	}
	if !a.Start.Equal(o.Start) || !a.End.Equal(o.End) {
		return false
	}
	if a.TimestampColumn != o.TimestampColumn ||
		a.ValueColumn != o.ValueColumn ||
		a.MetricNameColumn != o.MetricNameColumn ||
		a.AttributesColumn != o.AttributesColumn {
		return false
	}
	if len(a.SynthLabels) != len(o.SynthLabels) {
		return false
	}
	for i := range a.SynthLabels {
		if a.SynthLabels[i] != o.SynthLabels[i] {
			return false
		}
	}
	return a.Input.Equal(o.Input)
}

package chplan

import "time"

// RangeWindow is a PromQL-style range-vector aggregation: for each step
// across [Start, End] (inclusive), compute Func over the rows whose
// timestamp lies within [step-Range, step]. Used to lower expressions like
// `rate(metric[5m])` and `sum_over_time(metric[1h])`.
//
// The emitter (internal/chsql/range_window.go) produces ClickHouse SQL
// using the windowed-array idiom: GROUP BY series, build a sorted
// (ts, value) array via groupArray + arraySort, arrayFilter to the
// per-step window, then apply the function-specific aggregation.
type RangeWindow struct {
	Input Node

	// Func is the PromQL range function: "rate", "increase", "delta",
	// "avg_over_time", "sum_over_time", "min_over_time", "max_over_time",
	// "count_over_time", "last_over_time", ...
	Func string

	// Range is the [duration] window from the PromQL source.
	Range time.Duration

	// Step is the evaluation step (the resolution of the produced series).
	// Zero means instant query (a single step at End).
	Step time.Duration

	// Start / End define the eval grid the function is evaluated at.
	// Both zero means the emitter substitutes ClickHouse `now64()` for the
	// query-time anchor, which keeps test fixtures deterministic (the SQL
	// text is the same regardless of wall-clock).
	Start time.Time
	End   time.Time

	// Offset is the PromQL `offset` modifier shifted onto the inner
	// VectorSelector. Subtracted from End at emit time so the window
	// becomes [End - Offset - Range, End - Offset]. Zero means no offset.
	Offset time.Duration

	// TimestampColumn names the column carrying the per-sample timestamp
	// on Input (typically "TimeUnix" for OTel-CH).
	TimestampColumn string

	// ValueColumn names the column carrying the per-sample float value
	// on Input (typically "Value" for OTel-CH).
	ValueColumn string

	// GroupBy lists the expressions that identify a series for grouping
	// (typically `[ColumnRef("Attributes")]` for OTel-CH, since the map
	// column carries all the labels). May be nil/empty, in which case
	// the emitter does not group — all rows are treated as one series.
	GroupBy []Expr
}

func (*RangeWindow) planNode() {}

func (r *RangeWindow) Children() []Node { return []Node{r.Input} }

func (r *RangeWindow) Equal(other Node) bool {
	o, ok := other.(*RangeWindow)
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
	if len(r.GroupBy) != len(o.GroupBy) {
		return false
	}
	for i := range r.GroupBy {
		if !r.GroupBy[i].Equal(o.GroupBy[i]) {
			return false
		}
	}
	return r.Input.Equal(o.Input)
}

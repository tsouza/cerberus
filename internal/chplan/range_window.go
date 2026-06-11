package chplan

import "time"

// RangeWindow is a PromQL-style range-vector aggregation: for each step
// across [Start, End] (inclusive), compute Func over the rows whose
// timestamp lies within [step-Range, step]. Used to lower expressions like
// `rate(metric[5m])` and `sum_over_time(metric[1h])`.
//
// Input shapes (the emitter discriminates at render time):
//
//   - Row-shape relation (PromQL / LogQL): every row carries the
//     per-sample (TimestampColumn, ValueColumn) pair plus the GroupBy
//     series identity. The emitter (internal/chsql/range_window.go)
//     produces ClickHouse SQL using the windowed-array idiom: GROUP BY
//     series, build a sorted (ts, value) array via groupArray +
//     arraySort, arrayFilter to the per-step window, then apply the
//     function-specific aggregation. Func names the PromQL operator
//     (`rate`, `*_over_time`, …); TimestampColumn / ValueColumn are
//     required.
//
//   - MetricsAggregate input (TraceQL): the underlying relation is a
//     chplan.MetricsAggregate whose Inner is a per-span Scan/Filter
//     tree. Func is ignored — MetricsAggregate.Op carries the
//     per-bucket reducer. The emitter renders a time-bucketed matrix
//     via arrayJoin(range(...)) over [Start, End] spaced by Step,
//     applying the Op-specific CH aggregate per bucket. TimestampColumn
//     is required (it names the per-span Timestamp on Inner);
//     ValueColumn is unused (the metric value is the reduce of Attr).
//     Step must be > 0 in this mode.
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

	// OuterRange enables PromQL subquery emission: when non-zero the
	// emitter produces one row per anchor across [End - OuterRange, End]
	// spaced by Step (end-inclusive), rather than the instant single-anchor
	// shape. Set by the subquery lowering for `<expr>[<OuterRange>:<Step>]`.
	// Zero (the default) preserves today's instant semantics.
	//
	// Step must be > 0 whenever OuterRange > 0 — number of anchors is
	// OuterRange/Step + 1.
	OuterRange time.Duration

	// Identity reports whether the range function is the no-op
	// "evaluate the last sample in window" path used by bare-vector
	// subqueries (`up[5m:1m]`). When true, Func is ignored and the
	// emitter renders `if(length(window_vals) > 0,
	// window_vals[length(window_vals)], nan)`. Cleaner than overloading
	// Func with an "identity" sentinel.
	Identity bool

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

	// Scalars carries the scalar arguments threaded onto the range
	// function by the lowering layer. Used by `predict_linear(v, t)`
	// (single scalar — predict horizon in seconds) and
	// `holt_winters(v, sf, tf)` (smoothing factor + trend factor).
	// Empty for the simpler range functions (rate / increase /
	// *_over_time / log_rate) that take no extra parameters.
	Scalars []float64

	// ScalarExprs is the computed-scalar sibling of Scalars: when
	// non-empty it carries one Expr per scalar argument (typically a
	// ScalarSubquery built from a `scalar(<vector>)` argument, possibly
	// composed with literals through Binary nodes) and takes precedence
	// over Scalars at emit time. Set by the PromQL lowering for
	// `predict_linear(v[r], scalar(x))` and
	// `quantile_over_time(scalar(x), v[r])` — the shapes whose scalar
	// parameter the reference engine computes per evaluation. Mutually
	// exclusive with Scalars: a lowering populates one or the other.
	ScalarExprs []Expr
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
	if r.OuterRange != o.OuterRange || r.Identity != o.Identity {
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
	if len(r.Scalars) != len(o.Scalars) {
		return false
	}
	for i := range r.Scalars {
		if r.Scalars[i] != o.Scalars[i] {
			return false
		}
	}
	if len(r.ScalarExprs) != len(o.ScalarExprs) {
		return false
	}
	for i := range r.ScalarExprs {
		if !r.ScalarExprs[i].Equal(o.ScalarExprs[i]) {
			return false
		}
	}
	return r.Input.Equal(o.Input)
}

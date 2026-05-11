package chplan

import "time"

// RangeWindow is a PromQL-style range-vector aggregation: for each step
// across the eval range, compute Func over the rows whose timestamp lies
// within [step-Range, step]. Used to lower expressions like
// `rate(metric[5m])` and `increase(metric[1h])`.
//
// SQL form depends on Func; the emitter selects an idiomatic CH pattern
// (window function, asof self-join, or array aggregation depending on the
// schema and CH version).
type RangeWindow struct {
	Input Node

	// Func is the PromQL range function: "rate", "increase", "delta",
	// "avg_over_time", "sum_over_time", "min_over_time", "max_over_time", ...
	Func string

	// Range is the [duration] window from the PromQL source.
	Range time.Duration

	// Step is the evaluation step (the resolution of the produced series).
	// Zero means instant query (a single step at the query end time).
	Step time.Duration
}

func (*RangeWindow) planNode() {}

func (r *RangeWindow) Children() []Node { return []Node{r.Input} }

func (r *RangeWindow) Equal(other Node) bool {
	o, ok := other.(*RangeWindow)
	if !ok {
		return false
	}
	return r.Func == o.Func && r.Range == o.Range && r.Step == o.Step && r.Input.Equal(o.Input)
}

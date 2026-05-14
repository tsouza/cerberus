package chplan

import "time"

// StepGrid emits one row per Prometheus query_range step in the
// inclusive window `[Start, End]` spaced by Step. Each row exposes a
// single column named `anchor_ts` (DateTime64(9)) whose value is the
// step's evaluation timestamp.
//
// Used by the PromQL lowerings whose result has "no driving vector" —
// `time()`, `vector(scalar)`, the zero-arg date functions (`year()`,
// `month()`, ...), and `absent(...)` over a metric with no matching
// samples. The Prom range-query spec is "emit one sample per
// (start, end, step) regardless of input data" for these shapes; using
// `OneRow` here would yield a single sample at `now64(9)` and the
// matrix-pivot step loop in the API handler would then drop everything
// outside the 5-minute lookback.
//
// The instant case (start == end == ts, step == 0) keeps using
// `OneRow` — there is exactly one anchor to evaluate at, the resulting
// row count is identical, and the older `now64(9)` shape stays
// byte-stable across fixtures.
type StepGrid struct {
	Start time.Time
	End   time.Time
	Step  time.Duration
}

func (*StepGrid) planNode() {}

func (*StepGrid) Children() []Node { return nil }

func (s *StepGrid) Equal(other Node) bool {
	o, ok := other.(*StepGrid)
	if !ok {
		return false
	}
	return s.Start.Equal(o.Start) && s.End.Equal(o.End) && s.Step == o.Step
}

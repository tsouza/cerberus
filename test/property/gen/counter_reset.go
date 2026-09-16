package gen

import (
	"fmt"

	"github.com/tsouza/cerberus/test/property"
)

// This file builds the DETERMINISTIC counter-reset dataset that
// test/property/counter_reset_test.go proves rate()/increase()'s
// counter-reset compensation against (issue #3449).
//
// Unlike CounterDupTimestampDataset (counter_dup.go), which pins one
// specific DUPLICATE-timestamp shape and deliberately never exercises a
// real reset — its own header is explicit that it proves dedup, not
// reset handling — this series carries a GENUINE value decrease between
// two adjacent samples (20 -> 5 at t=180s): the counter-reset event
// Prometheus's extrapolation rule must detect and compensate for (adding
// the pre-reset value back into the accumulated delta) rather than read
// as a negative rate.

// CounterResetMetricName avoids every _sum/_count/_total/_bucket suffix
// so the schema heuristic keeps it in the gauge table — the same storage
// shape test/property/instant_window_test.go already establishes applies
// rate()/increase()'s counter-reset compensation regardless of physical
// OTel temporality (see instant_window.go's own header: "all cases are
// gauge-table samples ... rate/increase apply Prometheus counter-reset
// arithmetic to every geometry").
const CounterResetMetricName = "counter_reset_probe"

// CounterResetRangeSeconds is the [range] the proof's query carries: the
// whole span the four seeded samples occupy.
const CounterResetRangeSeconds = 240

// CounterResetEvalTsSec is the instant the proof evaluates at, so the
// matched window is the half-open interval (0, 240].
const CounterResetEvalTsSec = CounterResetRangeSeconds

// counterResetLabels is the single series identity the dataset carries.
var counterResetLabels = map[string]string{"job": "api"}

// counterResetRawPoints is the raw (second, value) series: a monotonic
// rise (10 -> 20), a genuine counter reset (20 -> 5 — a real value
// decrease between adjacent samples, not a duplicate timestamp), then a
// further rise (5 -> 15).
var counterResetRawPoints = []struct {
	tsSec int64
	value float64
}{
	{60, 10},
	{120, 20},
	{180, 5}, // counter reset: the value drops between adjacent samples.
	{240, 15},
}

// CounterResetCase returns the deterministic mid-window counter-reset
// InstantWindowCase: increase() over the gauge series above, evaluated at
// CounterResetEvalTsSec.
func CounterResetCase() InstantWindowCase {
	points := make([]property.Point, 0, len(counterResetRawPoints))
	for _, p := range counterResetRawPoints {
		points = append(points, property.Point{TimestampMs: p.tsSec * 1000, Value: p.value})
	}
	series := []property.SeriesData{{
		MetricName: CounterResetMetricName,
		Labels:     counterResetLabels,
		Points:     points,
	}}
	ds := property.Dataset{
		DDL:     renderDDL(series),
		Metrics: &property.MetricsModel{Series: series},
	}
	query := fmt.Sprintf(
		"increase(%s%s[%ds])",
		CounterResetMetricName, matcherString(counterResetLabels), CounterResetRangeSeconds,
	)
	return InstantWindowCase{
		Dataset:      ds,
		RangeSec:     CounterResetRangeSeconds,
		EvalOffset:   0,
		ValueProfile: InstantWindowValueProfileWave,
		Fn:           "increase",
		MetricName:   CounterResetMetricName,
		Labels:       counterResetLabels,
		LatestSample: CounterResetEvalTsSec,
		Query: property.Query{
			ShapeID: "test.counter-reset-mid-window",
			String:  query,
			EvalTs:  CounterResetEvalTsSec,
		},
	}
}

package promql

import (
	"fmt"
	"math"

	"github.com/prometheus/prometheus/promql/parser"
)

// evalRangeFunction applies a range-vector function to a range-vector
// input. The result is an instant vector: one row per input series,
// stamped at the eval ts. Per Prom semantics, each series's output is
// computed independently from its window samples.
//
// Supported (PR 2 MVP):
//
//   - rate(m[range])              — extrapolated counter rate / sec
//   - increase(m[range])          — extrapolated counter increase
//   - delta(m[range])             — extrapolated gauge delta
//   - sum_over_time(m[range])     — sum of samples in window
//   - avg_over_time(m[range])     — mean of samples in window
//   - min_over_time(m[range])     — minimum sample
//   - max_over_time(m[range])     — maximum sample
//   - count_over_time(m[range])   — sample count in window
//
// Anything else returns an error so the caller can surface it.
func (e *Evaluator) evalRangeFunction(c *parser.Call, evalTsMs int64) ([]VectorRow, error) {
	if len(c.Args) == 0 {
		return nil, fmt.Errorf("oracle: %s requires a range-vector arg", c.Func.Name)
	}
	ms, ok := c.Args[0].(*parser.MatrixSelector)
	if !ok {
		return nil, fmt.Errorf("oracle: %s argument must be a matrix selector, got %T", c.Func.Name, c.Args[0])
	}
	ranges := e.evalMatrixSelector(ms, evalTsMs)

	vs, _ := ms.VectorSelector.(*parser.VectorSelector)
	rangeMs := ms.Range.Milliseconds()
	// The "window edges" are the (T-range, T] mathematical interval
	// for extrapolation, where T already includes @-modifier + offset
	// shifts. This is what Prom uses for rate's extrapolation, not
	// the first/last actual sample timestamps.
	effectiveTs := evalTsMs
	if vs != nil {
		effectiveTs = effectiveEvalTs(vs, evalTsMs, e.startMs, e.endMs)
		effectiveTs -= vs.OriginalOffset.Milliseconds()
	}

	out := make([]VectorRow, 0, len(ranges))
	for _, r := range ranges {
		v, ok := applyRangeFn(c.Func.Name, r.Samples, rangeMs, effectiveTs)
		if !ok {
			// Fewer than 2 samples for rate/increase/delta -> Prom
			// drops the series silently.
			continue
		}
		// All range functions strip __name__ per Prom convention.
		out = append(out, VectorRow{
			Labels: DropLabel(r.Labels, MetricNameLabel),
			T:      evalTsMs,
			V:      v,
		})
	}
	sortVectorRows(out)
	return out, nil
}

// applyRangeFn dispatches by function name. Returns (value, true) on
// success or (0, false) when the function output is undefined for this
// window (insufficient samples for the extrapolating functions).
func applyRangeFn(name string, samples []Sample, rangeMs, effectiveTs int64) (float64, bool) {
	switch name {
	case "rate":
		return extrapolatedRate(samples, rangeMs, effectiveTs, true, true)
	case "increase":
		return extrapolatedRate(samples, rangeMs, effectiveTs, true, false)
	case "delta":
		return extrapolatedRate(samples, rangeMs, effectiveTs, false, false)
	case "sum_over_time":
		return sumOverTime(samples), len(samples) > 0
	case "avg_over_time":
		if len(samples) == 0 {
			return 0, false
		}
		return sumOverTime(samples) / float64(len(samples)), true
	case "min_over_time":
		return minOverTime(samples), len(samples) > 0
	case "max_over_time":
		return maxOverTime(samples), len(samples) > 0
	case "count_over_time":
		return float64(len(samples)), len(samples) > 0
	}
	return 0, false
}

// extrapolatedRate implements Prometheus's rate/increase/delta
// algorithm. The Prom engine has one shared helper for all three;
// the only knobs are:
//
//   - isCounter — true for rate/increase (handles counter resets by
//     bumping the running sum back up).
//   - isRate    — true for rate (divides by the window duration in
//     seconds at the end).
//
// The algorithm (paraphrased from prometheus/promql/functions.go's
// extrapolatedRate):
//
//  1. With fewer than 2 samples, output is undefined.
//  2. Compute the "result delta" as (last.V - first.V) plus counter-
//     reset bumps if isCounter.
//  3. Extrapolate to the window edges: if the first sample is close
//     to the left edge (within averageDurationBetweenSamples / 2),
//     extrapolate. Same for the right edge. Otherwise treat the
//     measured window as half the average gap. The exact heuristic
//     matches Prom's behavior — see comments below.
//  4. For rate, divide by the window duration in seconds.
//
// We mirror this faithfully; the comments are dense on purpose
// because the property test's whole point is to catch divergences
// from this exact behavior.
func extrapolatedRate(samples []Sample, rangeMs, effectiveTs int64, isCounter, isRate bool) (float64, bool) {
	if len(samples) < 2 {
		return 0, false
	}
	rangeStartMs := effectiveTs - rangeMs
	rangeEndMs := effectiveTs

	first := samples[0]
	last := samples[len(samples)-1]

	resultValue := last.V - first.V
	if isCounter {
		// Counter-reset detection: any value lower than the previous
		// one means the counter was reset. Add the pre-reset value
		// back to the running sum so the increase across the reset
		// is captured.
		prev := first.V
		for _, s := range samples[1:] {
			if s.V < prev {
				resultValue += prev
			}
			prev = s.V
		}
	}

	// Duration the measured samples cover, in ms.
	durationToStart := float64(first.T - rangeStartMs)
	durationToEnd := float64(rangeEndMs - last.T)

	sampledIntervalMs := float64(last.T - first.T)
	averageDurationBetweenSamplesMs := sampledIntervalMs / float64(len(samples)-1)

	// Extrapolate window edges, but cap the extrapolation distance:
	// don't assume the counter would have produced more than half
	// the average gap of extra increase past either edge.
	extrapolationThreshold := averageDurationBetweenSamplesMs * 1.1
	extrapolateToInterval := sampledIntervalMs

	if durationToStart >= extrapolationThreshold {
		durationToStart = averageDurationBetweenSamplesMs / 2
	}
	// For counters, the zero-start case is already captured by the
	// counter-reset detection above; no additional left-edge handling
	// needed (Prom's average-gap heuristic below applies uniformly).
	extrapolateToInterval += durationToStart

	if durationToEnd >= extrapolationThreshold {
		durationToEnd = averageDurationBetweenSamplesMs / 2
	}
	extrapolateToInterval += durationToEnd

	if sampledIntervalMs == 0 {
		return 0, false
	}
	factor := extrapolateToInterval / sampledIntervalMs
	if isRate {
		factor /= float64(rangeMs) / 1000.0
	}
	resultValue *= factor

	if math.IsNaN(resultValue) || math.IsInf(resultValue, 0) {
		// Still return — Prom emits these, the comparator's NaN
		// handling treats both-NaN as equal.
		return resultValue, true
	}
	return resultValue, true
}

func sumOverTime(samples []Sample) float64 {
	var sum float64
	for _, s := range samples {
		sum += s.V
	}
	return sum
}

func minOverTime(samples []Sample) float64 {
	if len(samples) == 0 {
		return 0
	}
	m := samples[0].V
	for _, s := range samples[1:] {
		if s.V < m || math.IsNaN(m) {
			m = s.V
		}
	}
	return m
}

func maxOverTime(samples []Sample) float64 {
	if len(samples) == 0 {
		return 0
	}
	m := samples[0].V
	for _, s := range samples[1:] {
		if s.V > m || math.IsNaN(m) {
			m = s.V
		}
	}
	return m
}

// isRangeFunctionName returns whether name is one of the MVP
// range-vector functions this oracle implements.
func isRangeFunctionName(name string) bool {
	switch name {
	case "rate", "increase", "delta",
		"sum_over_time", "avg_over_time",
		"min_over_time", "max_over_time", "count_over_time":
		return true
	}
	return false
}

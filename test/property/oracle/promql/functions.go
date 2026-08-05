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
// Supported:
//
//   - rate(m[range])              — extrapolated counter rate / sec
//   - increase(m[range])          — extrapolated counter increase
//   - delta(m[range])             — extrapolated gauge delta
//   - sum_over_time(m[range])     — sum of samples in window
//   - avg_over_time(m[range])     — mean of samples in window
//   - min_over_time(m[range])     — minimum sample
//   - max_over_time(m[range])     — maximum sample
//   - count_over_time(m[range])   — sample count in window
//   - last_over_time(m[range])    — most recent sample in window
//   - changes(m[range])           — count of value changes in window
//   - resets(m[range])            — count of counter resets in window
//   - stddev_over_time(m[range])  — population stddev of window samples
//   - stdvar_over_time(m[range])  — population variance of window samples
//
// The matrix argument itself may be a bare matrix selector (`m[5m]`) or
// a subquery (`<expr>[5m:1m]`, #1694, e.g.
// `max_over_time(rate(m[5m])[10m:1m])`) — see evalRangeArg
// (subquery.go).
//
// Anything else returns an error so the caller can surface it.
func (e *Evaluator) evalRangeFunction(c *parser.Call, evalTsMs int64) ([]VectorRow, error) {
	if len(c.Args) == 0 {
		return nil, fmt.Errorf("oracle: %s requires a range-vector arg", c.Func.Name)
	}
	ranges, rangeMs, effectiveTs, err := e.evalRangeArg(c.Args[0], evalTsMs)
	if err != nil {
		return nil, fmt.Errorf("oracle: %s: %w", c.Func.Name, err)
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
	case "last_over_time":
		if len(samples) == 0 {
			return 0, false
		}
		return samples[len(samples)-1].V, true
	case "changes":
		// Prom drops the row only when the window is entirely empty; a
		// single-sample window is well-defined at 0 changes.
		return changesOverTime(samples), len(samples) > 0
	case "resets":
		return resetsOverTime(samples), len(samples) > 0
	case "stddev_over_time":
		return math.Sqrt(varianceOverTime(samples)), len(samples) > 0
	case "stdvar_over_time":
		return varianceOverTime(samples), len(samples) > 0
	}
	return 0, false
}

// changesOverTime counts the number of value changes between
// consecutive samples in window order, matching Prom's funcChanges for
// the float-only case (no native histograms in this oracle). Two
// consecutive NaNs count as unchanged, mirroring Prom's explicit
// !(NaN && NaN) guard.
func changesOverTime(samples []Sample) float64 {
	changes := 0
	for i := 1; i < len(samples); i++ {
		cur, prev := samples[i].V, samples[i-1].V
		if cur != prev && !(math.IsNaN(cur) && math.IsNaN(prev)) {
			changes++
		}
	}
	return float64(changes)
}

// resetsOverTime counts counter resets (a value lower than the
// immediately preceding one) across the window, matching Prom's
// funcResets for the float-only case.
func resetsOverTime(samples []Sample) float64 {
	resets := 0
	for i := 1; i < len(samples); i++ {
		if samples[i].V < samples[i-1].V {
			resets++
		}
	}
	return float64(resets)
}

// varianceOverTime computes the population variance of the window's
// values — the shared core of stddev_over_time / stdvar_over_time,
// matching Prom's promql/functions.go::varianceOverTime (mean, then
// mean of squared deviations). A plain two-pass computation rather
// than Prom's single-pass Kahan-summed running variance: mathematically
// equivalent, and the dataset's value magnitudes never approach the
// range where the numerical difference would exceed the comparator's
// tolerance.
func varianceOverTime(samples []Sample) float64 {
	if len(samples) == 0 {
		return 0
	}
	var mean float64
	for _, s := range samples {
		mean += s.V
	}
	mean /= float64(len(samples))

	var sumSquares float64
	for _, s := range samples {
		d := s.V - mean
		sumSquares += d * d
	}
	return sumSquares / float64(len(samples))
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
	// Counter zero-crossing clamp (Prom functions.go::extrapolatedRate,
	// the `if isCounter { … durationToZero … }` block, applied AFTER the
	// threshold clamp above). Counters cannot be negative, so when the
	// series has positive slope we extrapolate back only as far as the
	// counter's implied zero point rather than all the way to the window
	// edge — otherwise the left-edge extrapolation would invent negative
	// counter history. `least(durationToStart, durationToZero)`: the zero
	// point only shortens the reach, never lengthens it. Reachable once
	// `offset` slides the window so its left edge sits before the first
	// in-window sample (durationToStart large), which the range-offset
	// generator now exercises.
	if isCounter {
		durationToZero := durationToStart
		if resultValue > 0 && len(samples) > 0 && first.V >= 0 {
			durationToZero = sampledIntervalMs * (first.V / resultValue)
		}
		if durationToZero < durationToStart {
			durationToStart = durationToZero
		}
	}
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

// isRangeFunctionName returns whether name is one of the range-vector
// functions this oracle implements.
func isRangeFunctionName(name string) bool {
	switch name {
	case "rate", "increase", "delta",
		"sum_over_time", "avg_over_time",
		"min_over_time", "max_over_time", "count_over_time",
		"last_over_time", "changes", "resets",
		"stddev_over_time", "stdvar_over_time":
		return true
	}
	return false
}

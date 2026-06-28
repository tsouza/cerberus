// SPDX-License-Identifier: Apache-2.0
//
// Clean-room, in-house implementations of the three metrics-engine helpers
// cerberus previously borrowed from grafana/tempo's AGPL pkg/traceql:
//
//   - defaultQueryRangeStep — the "no explicit ?step" granularity heuristic
//     (target a fixed number of data points across the window, snapping to a
//     human-friendly granularity ladder).
//   - histogramBucket / log2QuantileWithBucket — quantile estimation over a
//     base-2 (log-linear) exponential histogram, with exponential
//     interpolation between adjacent bucket boundaries.
//
// Both are uncopyrightable algorithm facts (a step heuristic and a histogram
// quantile interpolation); this file is an original expression of them and
// carries no AGPL lineage. Removing the borrowed symbols is what lets
// `go list -deps ./cmd/cerberus` come back free of grafana/tempo/pkg/traceql.

package tempo

import (
	"math"
	"time"
)

// targetDataPoints is the number of samples the default-step heuristic aims to
// produce across a query window when the caller omits ?step (1h at 15s ==
// 240 points). The baseline step is window/targetDataPoints, then snapped down
// to a granularity-ladder rung so adjacent windows pick stable step values.
const targetDataPoints = 240

// subMinuteGranularity is the snap unit for sub-minute windows, where clients
// (Grafana) render millisecond precision; the resulting step never drops below
// this floor.
const subMinuteGranularity = 50 * time.Millisecond

// stepGranularityLadder lists, high to low, the rungs the baseline step snaps
// down to for windows of a minute or more. The first rung strictly below the
// baseline wins; baseline is floored to a whole multiple of that rung so we err
// toward more data points. minStepGranularity is the floor when no rung fits.
var stepGranularityLadder = []time.Duration{
	time.Hour,
	5 * time.Minute,
	time.Minute,
	15 * time.Second,
	5 * time.Second,
	time.Second,
}

const minStepGranularity = time.Second

// defaultQueryRangeStep returns the step (in nanoseconds) to use when a
// query_range request omits ?step, given the [start,end] window in unix
// nanoseconds. It mirrors reference Tempo's frontend default so Grafana's
// Traces Drilldown (which omits step) renders an equivalent point density.
func defaultQueryRangeStep(start, end uint64) uint64 {
	window := time.Duration(end - start) //nolint:gosec // G115: ns window within retention; no int64 overflow.
	baseline := window / targetDataPoints

	snap := func(unit time.Duration) uint64 {
		// Floor baseline to a whole multiple of unit.
		return uint64((baseline / unit * unit).Nanoseconds()) //nolint:gosec // G115: positive granularity duration.
	}

	if window < time.Minute {
		if baseline > subMinuteGranularity {
			return snap(subMinuteGranularity)
		}
		return uint64(subMinuteGranularity.Nanoseconds()) //nolint:gosec // G115: positive granularity const.
	}

	for _, rung := range stepGranularityLadder {
		if baseline > rung {
			return snap(rung)
		}
	}
	return uint64(minStepGranularity.Nanoseconds()) //nolint:gosec // G115: positive granularity const.
}

// histogramBucket is a single boundary of a base-2 exponential histogram: Count
// observations fell at or below Max. The quantile helper assumes buckets are
// sorted ascending by Max.
type histogramBucket struct {
	Max   float64
	Count int
}

// log2QuantileWithBucket estimates the p-quantile (0<=p<=1) over a base-2
// exponential histogram and returns the estimate plus the index of the bucket
// the quantile landed in (-1 when there is no data).
//
// The boundaries are powers of two, so two adjacent buckets [prevMax, max]
// bound a value whose log2 is linear in the consumed sample fraction. We locate
// the bucket holding the ceil(p*total)-th sample, compute how far into that
// bucket the target sample sits, and interpolate in log2 space:
//
//	value = 2 ^ ( log2(prevMax) + (log2(max) - log2(prevMax)) * frac )
//
// For the first bucket (no predecessor) the lower boundary is taken one octave
// below max, matching the power-of-two spacing.
func log2QuantileWithBucket(p float64, buckets []histogramBucket) (float64, int) {
	if len(buckets) == 0 {
		return 0, -1
	}

	total := 0
	for _, b := range buckets {
		total += b.Count
	}
	if total == 0 {
		return 0, -1
	}

	// Round up so low-sample-count quantiles still read at least one sample.
	target := int(math.Ceil(p * float64(total)))
	if target < 1 {
		target = 1
	}

	// Walk buckets, consuming whole buckets until the next one would overshoot
	// the target sample. `consumed` is the count strictly before `idx`.
	idx := 0
	consumed := 0
	for i, b := range buckets {
		idx = i
		if consumed+b.Count > target {
			break
		}
		consumed += b.Count
		if consumed == target {
			// Target lands exactly on this bucket's upper boundary; no
			// interpolation needed.
			return b.Max, i
		}
	}

	frac := float64(target-consumed) / float64(buckets[idx].Count)

	upper := math.Log2(buckets[idx].Max)
	lower := upper - 1 // one octave below, for the no-predecessor case
	if idx > 0 {
		lower = math.Log2(buckets[idx-1].Max)
	}
	return math.Pow(2, lower+(upper-lower)*frac), idx
}

// Package tolerant implements the ONE dedicated tolerant comparator
// docs/migration-testing.md section 5 mode 3 describes: long-range panels
// served from cerberus's raw/MV-rollup data compared against the incumbent's
// 5m/1h downsampled counter-aware aggregates, within a band declared up front
// from the aggregation math. This is deliberately NOT `cerberus migrate
// verify` — verify's comparator is a flat absolute epsilon with no
// counter-aware/downsample mode, so this mode lives outside the zero-diverge
// verify gate entirely (MIG-20).
package tolerant

import (
	"fmt"
	"time"

	"github.com/tsouza/cerberus/test/e2e/migration/tolerances"
)

// Sample is one instant of a counter series: a timestamp and its raw value.
type Sample struct {
	T time.Time
	V float64
}

// DownsampleCounterAware reconstructs what a Thanos-style 5m/1h counter
// downsample would hold: one value per bucket, aligned to from, equal to the
// MAXIMUM raw sample observed in that bucket. Max-in-bucket (rather than
// last-in-bucket) is what stays correct across a counter reset inside a
// bucket — a reset makes later raw values smaller, and taking the max is what
// a counter-aware downsampler does to avoid reporting a spurious drop. A
// bucket with no raw sample at all carries forward the previous bucket's
// value (last observation carried forward) rather than reporting a gap as
// zero, which would read as an impossible reset back to nothing.
//
// This is the "aggregation math" MIG-20's PASS assertion says the accepted
// band is derived from: the caller declares bucket and reads the loss this
// function structurally introduces at the leading edge (see Compare).
func DownsampleCounterAware(raw []Sample, from, to time.Time, bucket time.Duration) ([]Sample, error) {
	if bucket <= 0 {
		return nil, fmt.Errorf("tolerant: downsample bucket must be positive, got %s", bucket)
	}
	if !to.After(from) {
		return nil, fmt.Errorf("tolerant: downsample window end %s must be after start %s", to, from)
	}
	n := int(to.Sub(from)/bucket) + 1
	out := make([]Sample, 0, n)
	var carry float64
	haveCarry := false
	for i := 0; i < n; i++ {
		bStart := from.Add(time.Duration(i) * bucket)
		bEnd := bStart.Add(bucket)
		max := carry
		found := false
		for _, s := range raw {
			if s.T.Before(bStart) || !s.T.Before(bEnd) {
				continue
			}
			if !found || s.V > max {
				max = s.V
				found = true
			}
		}
		if !found {
			if !haveCarry {
				continue
			}
			max = carry
		}
		carry, haveCarry = max, true
		out = append(out, Sample{T: bStart, V: max})
	}
	return out, nil
}

// Increase returns the total growth a monotonically-increasing counter series
// shows between its first and last point. An empty or single-point series has
// no measured growth at all, which the caller treats as "nothing was
// compared" rather than a growth of zero.
func Increase(series []Sample) (float64, bool) {
	if len(series) < 2 {
		return 0, false
	}
	return series[len(series)-1].V - series[0].V, true
}

// Result is one comparator verdict: both sides' measured growth, the relative
// delta between them, and whether that delta cleared the declared band.
type Result struct {
	RawIncrease         float64
	DownsampledIncrease float64
	RelativeDelta       float64
	Band                tolerances.Band
	WithinBand          bool
	Detail              string
}

// Compare judges cerberus's raw-path answer (rawIncrease, already computed by
// the caller from a live query) against a downsampled reconstruction of the
// incumbent's own raw samples, within band. It is the whole comparator: no
// other function in this package makes the pass/fail call.
func Compare(rawIncrease float64, downsampled []Sample, band tolerances.Band) (Result, error) {
	downsampledIncrease, ok := Increase(downsampled)
	if !ok {
		return Result{}, fmt.Errorf("tolerant: the downsampled reconstruction has fewer than two points, so it measures no growth")
	}
	if rawIncrease == 0 {
		return Result{}, fmt.Errorf("tolerant: cerberus's raw-path answer shows zero growth, so a relative delta is undefined")
	}
	delta := (rawIncrease - downsampledIncrease) / rawIncrease
	if delta < 0 {
		delta = -delta
	}
	res := Result{
		RawIncrease:         rawIncrease,
		DownsampledIncrease: downsampledIncrease,
		RelativeDelta:       delta,
		Band:                band,
		WithinBand:          delta <= band.Value,
	}
	res.Detail = fmt.Sprintf(
		"raw increase %.6f, downsampled-reconstruction increase %.6f, relative delta %.6f, declared band %.6f",
		res.RawIncrease, res.DownsampledIncrease, res.RelativeDelta, band.Value,
	)
	return res, nil
}

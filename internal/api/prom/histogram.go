package prom

import (
	"math"
	"strconv"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// Boundary-rule constants matching upstream Prometheus's
// util/jsonutil.MarshalHistogram vocabulary exactly: which of a
// bucket's two edges are inclusive.
const (
	boundariesLowerExclusiveUpperInclusive = 0
	boundariesLowerInclusiveUpperExclusive = 1
	boundariesBothInclusive                = 3
)

// histogramSample builds the `[timestamp_seconds_float, {count, sum,
// buckets}]` wire pair upstream Prometheus emits for a histogram-valued
// point (marshalSampleJSON / marshalHPointJSON's `histogram` key
// shape — prometheus/web/api/v1/json_codec.go), from a decoded
// chclient.HistogramValue. Populated only when a row carries one — see
// chclient.Sample.Histogram's doc comment for why that is nil today.
func histogramSample(ts time.Time, hv *chclient.HistogramValue) Sample {
	stamp := float64(ts.UnixMilli()) / 1e3
	return Sample{stamp, histogramFromValue(hv)}
}

// histogramFromValue converts cerberus's decoded OTel exponential
// (native) histogram representation — log-scale buckets addressed by
// (Scale, Offset, BucketCounts) rather than explicit boundaries — into
// upstream Prometheus's wire shape: an explicit (lower, upper, count)
// triple per POPULATED bucket, walked in ascending value order.
//
// The bucket-boundary math mirrors internal/chsql's
// histogramQuantileNativeValueFrag (the SQL-side quantile emitter's
// per-bucket edges: same base = 2^(2^-Scale)) but walks every populated
// bucket instead of interpolating a single quantile point.
//
//   - Negative buckets cover `[-base^(offset+i+1), -base^(offset+i))` —
//     lower inclusive, upper exclusive — walked from the
//     LARGEST-magnitude bucket (the end of the array) toward zero, so
//     the output is ascending by value.
//   - Positive buckets cover `(base^(offset+i), base^(offset+i+1)]` —
//     lower exclusive, upper inclusive.
//   - The zero bucket spans `[-ZeroThreshold, +ZeroThreshold]` (both
//     inclusive), one-sided-clamped exactly like the SQL emitter's
//     zeroBandLower / zeroBandUpper when a distribution has
//     observations on only one side of zero (reference Prometheus's
//     promql/quantile.go:263-273 clamp): a distribution recording
//     nothing on one side has no observation that could have produced
//     a value there, so that edge collapses to 0 rather than spanning
//     the full symmetric band.
//
// A bucket whose count is zero is never appended, matching upstream's
// own `if bucket.Count == 0 { continue }`.
func histogramFromValue(hv *chclient.HistogramValue) *Histogram {
	if hv == nil {
		return nil
	}
	base := math.Pow(2, math.Pow(2, -float64(hv.Scale)))

	var buckets []HistogramBucket
	nlen := len(hv.NegativeBucketCounts)
	for i := nlen - 1; i >= 0; i-- {
		count := hv.NegativeBucketCounts[i]
		if count == 0 {
			continue
		}
		idx := float64(hv.NegativeOffset) + float64(i)
		buckets = append(buckets, HistogramBucket{
			Boundaries: boundariesLowerInclusiveUpperExclusive,
			Lower:      formatWireFloat(-math.Pow(base, idx+1)),
			Upper:      formatWireFloat(-math.Pow(base, idx)),
			Count:      formatWireFloat(float64(count)),
		})
	}
	if hv.ZeroCount > 0 {
		lower, upper := -hv.ZeroThreshold, hv.ZeroThreshold
		if nlen == 0 && len(hv.PositiveBucketCounts) > 0 {
			lower = 0
		}
		if len(hv.PositiveBucketCounts) == 0 && nlen > 0 {
			upper = 0
		}
		buckets = append(buckets, HistogramBucket{
			Boundaries: boundariesBothInclusive,
			Lower:      formatWireFloat(lower),
			Upper:      formatWireFloat(upper),
			Count:      formatWireFloat(float64(hv.ZeroCount)),
		})
	}
	for i, count := range hv.PositiveBucketCounts {
		if count == 0 {
			continue
		}
		idx := float64(hv.PositiveOffset) + float64(i)
		buckets = append(buckets, HistogramBucket{
			Boundaries: boundariesLowerExclusiveUpperInclusive,
			Lower:      formatWireFloat(math.Pow(base, idx)),
			Upper:      formatWireFloat(math.Pow(base, idx+1)),
			Count:      formatWireFloat(float64(count)),
		})
	}

	return &Histogram{
		Count:   formatWireFloat(float64(hv.Count)),
		Sum:     formatWireFloat(hv.Sum),
		Buckets: buckets,
	}
}

// formatWireFloat renders f the way every numeric value crosses the
// Prometheus wire: a quoted, shortest-round-trip decimal string —
// strconv.FormatFloat(f, 'f', -1, 64), the same call every float
// [Sample] pair in this package already formats its value slot with.
func formatWireFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

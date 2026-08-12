package promql

import (
	"fmt"
	"math"

	"github.com/tsouza/cerberus/test/property"
)

// nativeHistogramQuantile implements `histogram_quantile(phi, vector)`
// over a NATIVE (OTel exponential) histogram instant vector — the
// counterpart to histogramQuantile's classic-bucket path. Only rows
// carrying a Histogram payload are consumed; the caller is responsible
// for routing classic-bucket rows (Histogram == nil) to
// histogramQuantile instead.
//
// cerberus's own native-quantile support (internal/chsql/
// histogram_quantile_native.go) is wider than this oracle: it also
// handles rate()/sum by(...)-wrapped arguments via a scale-fold merge
// algorithm. This oracle only covers the bare-selector shape (the
// argument is a plain VectorSelector, one histogram sample per
// series). Rows arriving without a Histogram payload — which is what
// the oracle's aggregation path produces — route to the classic
// le-bucket oracle and fail loudly there rather than being silently
// dropped. Teaching the oracle the merge is tracked in #2063.
func nativeHistogramQuantile(phi float64, rows []VectorRow, evalTsMs int64) []VectorRow {
	out := make([]VectorRow, 0, len(rows))
	for _, r := range rows {
		if r.Histogram == nil {
			continue
		}
		out = append(out, VectorRow{
			Labels: DropLabel(r.Labels, MetricNameLabel),
			T:      evalTsMs,
			V:      nativeHistogramQuantileValue(phi, r.Histogram),
		})
	}
	sortVectorRows(out)
	return out
}

// reverseWalkPhi is the phi at which reference Prometheus's rank walk
// flips from the forward bucket iterator to the reverse one
// (promql/quantile.go: `if math.IsNaN(h.Sum) || q < 0.5`). cerberus's
// emitter names the same constant.
const reverseWalkPhi = 0.5

// nativeHistogramQuantileValue walks the bucket layout of a single
// native-histogram sample to compute the phi-th quantile, following
// reference Prometheus's HistogramQuantile (promql/quantile.go) — which
// is also what internal/chsql/histogram_quantile_native.go emits:
//
//  1. base = 2^(2^-Scale).
//  2. Build the cumulative-count array over
//     reverse(NegativeBucketCounts) ++ [ZeroCount] ++ PositiveBucketCounts.
//  3. Walk that array for the rank, in one of TWO directions:
//     forward from the bottom for phi < reverseWalkPhi (stop at the
//     first cumulative count >= phi * total), backward from the top
//     otherwise (stop at the first bucket whose count-at-or-above,
//     total - cum[i-1], reaches (1 - phi) * total). The directions
//     agree except at an exact rank tie, where the forward walk stops
//     on the bucket ENDING at the target and the backward walk on the
//     next POPULATED one — a real divergence whenever an empty bucket
//     or a region boundary separates them (#2066).
//  4. Interpolate within the stop bucket via the fraction of it the
//     rank consumed, then map back to a value via the region
//     (negative / zero / positive) the index falls in.
//
// Both walk arms skip empty buckets for free, because a cumulative
// count array is non-decreasing: an empty bucket repeats its
// predecessor's value and so can never be the FIRST index to reach a
// strictly positive rank. Only rank = 0 could stop on one, which is
// exactly the phi = 0 / phi = 1 pair handled explicitly below.
//
// ZeroThreshold is always 0 in cerberus (the default OTel-CH DDL
// doesn't persist it), so the zero-bucket region always evaluates to
// exactly 0.
//
// phi == 0 and phi == 1 are in-domain and saturate to the LOWEST and
// HIGHEST edge the histogram actually observed — the lower edge of the
// lowest populated bucket and the upper edge of the highest populated
// one. Populated, not outermost: reference Prometheus's rank walk
// skips zero-count buckets outright (promql/quantile.go's
// HistogramQuantile: `if bucket.Count == 0 { continue }`), and a bucket
// array may carry zeros anywhere, since the offsets describe only the
// leading gap. Answering from an array end would report the edge of an
// interval no observation ever fell in.
func nativeHistogramQuantileValue(phi float64, h *property.NativeHistogram) float64 {
	if phi < 0 {
		return math.Inf(-1)
	}
	if phi > 1 {
		return math.Inf(1)
	}

	nlen := len(h.NegativeBucketCounts)
	plen := len(h.PositiveBucketCounts)

	// buckets is the single walk order every index below is expressed
	// in: reverse(negative) ++ [zero] ++ positive, running from the
	// most-negative observation toward the most-positive one.
	buckets := make([]float64, 0, nlen+1+plen)
	for i := nlen - 1; i >= 0; i-- {
		buckets = append(buckets, float64(h.NegativeBucketCounts[i]))
	}
	buckets = append(buckets, float64(h.ZeroCount))
	for _, c := range h.PositiveBucketCounts {
		buckets = append(buckets, float64(c))
	}

	// cum is 1-based to match the spec: cum[0] = 0 is the implicit "no
	// bucket consumed yet" value the fraction formula needs at idx = 1.
	cum := make([]float64, len(buckets)+1)
	for i, b := range buckets {
		cum[i+1] = cum[i] + b
	}
	total := cum[len(buckets)]
	if total == 0 {
		return math.NaN()
	}

	// phi == 0 saturates to the lower edge of the lowest POPULATED
	// bucket and phi == 1 to the upper edge of the highest populated
	// one — each is the interpolation below evaluated at fraction 0 / 1.
	// They walk to a populated bucket rather than to an array end
	// because a bucket array may carry zeros anywhere (the offsets
	// describe only the leading gap), and an array end can be an
	// interval no observation fell in.
	if phi == 0 {
		return nativeHistogramBucketValue(h, populatedIndex(buckets, false), 0)
	}
	if phi == 1 {
		return nativeHistogramBucketValue(h, populatedIndex(buckets, true), 1)
	}

	target := phi * total
	idx := len(buckets) // 1-based; the walks below always reassign it
	if phi < reverseWalkPhi {
		for i := 1; i <= len(buckets); i++ {
			if cum[i] >= target {
				idx = i
				break
			}
		}
	} else {
		rank := (1 - phi) * total
		for i := 1; i <= len(buckets); i++ {
			if total-cum[i] < rank {
				idx = i
				break
			}
		}
	}

	// One fraction formula serves both arms: reference rebases the
	// remaining rank per direction, but substituting the backward arm's
	// running count (total - cum[idx-1]) reduces its expression to this
	// same one, so only the stop INDEX ever differs.
	var fraction float64
	if cum[idx] > cum[idx-1] {
		fraction = (target - cum[idx-1]) / (cum[idx] - cum[idx-1])
	}
	return nativeHistogramBucketValue(h, idx, fraction)
}

// populatedIndex returns the 1-based position of the first (last = false)
// or last (last = true) bucket carrying a non-zero count, in the same
// concatenated walk order nativeHistogramQuantileValue builds.
func populatedIndex(buckets []float64, last bool) int {
	idx := 1
	for i, b := range buckets {
		if b == 0 {
			continue
		}
		if !last {
			return i + 1
		}
		idx = i + 1
	}
	return idx
}

// nativeHistogramBucketValue maps a 1-based walk index plus the
// fraction of that bucket the rank consumed back to an observation
// value, by the region the index falls in. fraction = 0 is the bucket's
// low edge (nearest -Inf) and fraction = 1 its high edge.
func nativeHistogramBucketValue(h *property.NativeHistogram, idx int, fraction float64) float64 {
	base := math.Pow(2, math.Pow(2, -float64(h.Scale)))
	nlen := len(h.NegativeBucketCounts)
	switch {
	case idx <= nlen:
		// Negative region: the cum-sum enters from the more-negative
		// edge, so a larger fraction means a value closer to zero.
		return -math.Pow(base, float64(h.NegativeOffset)+float64(nlen-idx)+1-fraction)
	case idx == nlen+1:
		// Zero bucket, a point at 0 (ZeroThreshold is always 0 here).
		return 0
	default:
		return math.Pow(base, float64(h.PositiveOffset)+float64(idx-nlen-2)+fraction)
	}
}

// nativeHistogramValue implements the scalar-per-sample native-
// histogram value functions (histogram_count/_sum/_avg/_stddev/
// _stdvar). Rows without a Histogram payload (plain float samples) are
// skipped rather than erroring — this mirrors cerberus's own emitter
// (internal/promql/histogram_value_fns.go), which folds any
// non-selector or float-sample input to an always-empty vector rather
// than raising an error.
func nativeHistogramValue(name string, rows []VectorRow, evalTsMs int64) ([]VectorRow, error) {
	out := make([]VectorRow, 0, len(rows))
	for _, r := range rows {
		if r.Histogram == nil {
			continue
		}
		v, err := histogramScalarValue(name, r.Histogram)
		if err != nil {
			return nil, err
		}
		out = append(out, VectorRow{
			Labels: DropLabel(r.Labels, MetricNameLabel),
			T:      evalTsMs,
			V:      v,
		})
	}
	sortVectorRows(out)
	return out, nil
}

func histogramScalarValue(name string, h *property.NativeHistogram) (float64, error) {
	switch name {
	case "histogram_count":
		return float64(h.Count), nil
	case "histogram_sum":
		return h.Sum, nil
	case "histogram_avg":
		if h.Count == 0 {
			return math.NaN(), nil
		}
		return h.Sum / float64(h.Count), nil
	case "histogram_stddev":
		return math.Sqrt(histogramVariance(h)), nil
	case "histogram_stdvar":
		return histogramVariance(h), nil
	}
	return 0, fmt.Errorf("oracle: unsupported native-histogram value function %q", name)
}

// histogramVariance computes the population variance of a native
// histogram, treating every bucket's count as concentrated at its
// midpoint (base^(offset+i+0.5), 1-based i) and the zero bucket as
// concentrated at 0 (ZeroThreshold is always 0 in cerberus). Mirrors
// internal/promql/histogram_value_fns.go's histogramVarianceExpr,
// using the histogram's own Sum/Count as the mean rather than a
// bucket-approximated one.
func histogramVariance(h *property.NativeHistogram) float64 {
	if h.Count == 0 {
		return math.NaN()
	}
	mean := h.Sum / float64(h.Count)
	base := math.Pow(2, math.Pow(2, -float64(h.Scale)))

	var sumSq float64
	for i, c := range h.PositiveBucketCounts {
		mid := math.Pow(base, float64(h.PositiveOffset)+float64(i)+0.5)
		d := mid - mean
		sumSq += float64(c) * d * d
	}
	for i, c := range h.NegativeBucketCounts {
		mid := -math.Pow(base, float64(h.NegativeOffset)+float64(i)+0.5)
		d := mid - mean
		sumSq += float64(c) * d * d
	}
	sumSq += float64(h.ZeroCount) * mean * mean

	return sumSq / float64(h.Count)
}

// nativeHistogramFraction implements `histogram_fraction(lower, upper,
// vector)` over native-histogram rows. Rows without a Histogram
// payload are skipped (see nativeHistogramValue's doc for why).
func nativeHistogramFraction(lower, upper float64, rows []VectorRow, evalTsMs int64) []VectorRow {
	out := make([]VectorRow, 0, len(rows))
	for _, r := range rows {
		if r.Histogram == nil {
			continue
		}
		out = append(out, VectorRow{
			Labels: DropLabel(r.Labels, MetricNameLabel),
			T:      evalTsMs,
			V:      histogramFractionValue(lower, upper, r.Histogram),
		})
	}
	sortVectorRows(out)
	return out
}

func histogramFractionValue(lower, upper float64, h *property.NativeHistogram) float64 {
	if math.IsNaN(lower) || math.IsNaN(upper) {
		return math.NaN()
	}
	if h.Count == 0 {
		return math.NaN()
	}
	if lower >= upper {
		return 0
	}
	count := float64(h.Count)
	rl := math.Min(count, histogramRank(h, lower))
	ru := math.Min(count, histogramRank(h, upper))
	return (ru - rl) / count
}

// histogramRank computes R(v): the count of samples in h at or below
// value v, per internal/promql/histogram_value_fns.go's
// histogramRankExpr. logIdx converts a magnitude into the histogram's
// exponential bucket-index space (base^logIdx == |v|); the sPos/sNeg
// helpers then interpolate within the positive/negative bucket arrays.
func histogramRank(h *property.NativeHistogram, v float64) float64 {
	tn := sumUint64(h.NegativeBucketCounts)
	tp := sumUint64(h.PositiveBucketCounts)
	z := float64(h.ZeroCount)
	scaleFactor := math.Pow(2, float64(h.Scale))

	switch {
	case v > 0:
		logIdx := math.Log2(v) * scaleFactor
		p := logIdx - float64(h.PositiveOffset)
		return tn + z + sPos(h.PositiveBucketCounts, tp, p)
	case v < 0:
		logIdx := math.Log2(-v) * scaleFactor
		q := logIdx - float64(h.NegativeOffset)
		return sNeg(h.NegativeBucketCounts, tn, q)
	default:
		return tn
	}
}

// sPos is the positive-side rank contribution: the count of positive-
// bucket samples at or below bucket-index position p (fractional).
func sPos(pbc []uint64, total, p float64) float64 {
	if p <= 0 {
		return 0
	}
	if p >= float64(len(pbc)) {
		return total
	}
	k := int(math.Floor(p))
	frac := p - float64(k)
	var cum float64
	for i := 0; i < k; i++ {
		cum += float64(pbc[i])
	}
	return cum + float64(pbc[k])*frac
}

// sNeg is the negative-side rank contribution: the count of negative-
// bucket samples at or below magnitude-position q (fractional),
// counted from the far (most-negative) end inward — negative buckets
// closer to zero rank HIGHER, mirroring how more-negative values sort
// lower than less-negative ones.
func sNeg(nbc []uint64, total, q float64) float64 {
	nlen := len(nbc)
	if q >= float64(nlen) {
		return 0
	}
	if q <= 0 {
		return total
	}
	m := int(math.Ceil(q))
	var cum float64
	for i := 0; i < m; i++ {
		cum += float64(nbc[i])
	}
	return (total - cum) + float64(nbc[m-1])*(float64(m)-q)
}

func sumUint64(xs []uint64) float64 {
	var total float64
	for _, x := range xs {
		total += float64(x)
	}
	return total
}

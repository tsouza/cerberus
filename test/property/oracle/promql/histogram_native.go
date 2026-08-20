package promql

import (
	"fmt"
	"math"
)

// nativeHistogramQuantile implements `histogram_quantile(phi, vector)`
// over a NATIVE (OTel exponential) histogram instant vector — the
// counterpart to histogramQuantile's classic-bucket path. Only rows
// carrying a Histogram payload are consumed; the caller is responsible
// for routing classic-bucket rows (Histogram == nil) to
// histogramQuantile instead.
//
// Rows may come from a bare selector, the aggregation merge, or a
// rate/increase window fold; all three carry the same float histogram
// representation by the time they reach this function.
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
//  2. Lay the buckets out in ascending value order,
//     reverse(NegativeBucketCounts) ++ [ZeroCount] ++ PositiveBucketCounts.
//  3. Walk them for the rank, in one of TWO directions: forward from the
//     bottom when Sum is NaN or phi < reverseWalkPhi, backward from the
//     top otherwise. Either way the walk accumulates the bucket counts
//     IT passes over and stops on the first bucket at which that running
//     count reaches the rank — phi * Count forward, (1 - phi) * Count
//     backward. The directions agree except at an exact rank tie, where
//     the forward walk stops on the bucket ENDING at the target and the
//     backward walk on the next POPULATED one — a real divergence
//     whenever an empty bucket or a region boundary separates them.
//  4. Rebase the remaining rank inside the stop bucket, divide by that
//     bucket's count to get the fraction of it the rank consumed, then
//     map back to a value via the region (negative / zero / positive)
//     the index falls in.
//
// # Why the walk accumulates instead of subtracting
//
// Every quantity here is computed the way reference computes it, in
// reference's own order, and never derived from the opposite direction
// by subtraction. A cumulative array walked from the wrong end, or a
// running count reconstructed as `total - cum[i]`, is equal in exact
// arithmetic and NOT equal in floating point: the subtraction cancels
// away low-order bits. Once a rate()/increase() window fold has
// multiplied every bucket by one extrapolation factor, those bits are
// what decides an exact rank tie, and across the zero bucket that is a
// sign flip in the answer (cerberus issue #2403, where a subtraction-
// derived walk answered 0 for a median reference put at -1). This
// oracle carried a 1e-12 rank tolerance to paper over exactly that, and
// the tolerance is what made it disagree with reference; accumulating in
// reference's direction removes the need for one.
//
// Both walk arms skip empty buckets, exactly as reference's own
// `if bucket.Count == 0 { continue }` does, so the stop bucket is always
// one an observation fell in and the fraction never divides by zero.
// That is also what makes phi == 0 and phi == 1 land on the LOWEST and
// HIGHEST POPULATED bucket rather than on an array end: both give a rank
// of 0, which the first populated bucket in the walk's direction already
// reaches, and a bucket array may carry zeros anywhere since the offsets
// describe only the leading gap.
//
// ZeroThreshold is always 0 in cerberus (the default OTel-CH DDL
// doesn't persist it), so the zero-bucket region always evaluates to
// exactly 0.
func nativeHistogramQuantileValue(phi float64, h *nativeHistogram) float64 {
	if phi < 0 {
		return math.Inf(-1)
	}
	if phi > 1 {
		return math.Inf(1)
	}
	if h.Count == 0 || math.IsNaN(phi) {
		return math.NaN()
	}

	nlen := len(h.NegativeBucketCounts)
	plen := len(h.PositiveBucketCounts)

	// buckets is the single walk order every index below is expressed
	// in: reverse(negative) ++ [zero] ++ positive, running from the
	// most-negative observation toward the most-positive one.
	buckets := make([]float64, 0, nlen+1+plen)
	for i := nlen - 1; i >= 0; i-- {
		buckets = append(buckets, h.NegativeBucketCounts[i])
	}
	buckets = append(buckets, h.ZeroCount)
	buckets = append(buckets, h.PositiveBucketCounts...)

	// Reference forces the forward arm whenever Sum is NaN regardless of
	// phi: NaN observations inflate Count without landing in any bucket,
	// and a reverse walk cannot find a percentile whose mass sits outside
	// every bucket.
	forward := math.IsNaN(h.Sum) || phi < reverseWalkPhi
	rank := (1 - phi) * h.Count
	if forward {
		rank = phi * h.Count
	}

	// idx is 1-based and ends on the bucket reference's iterator was
	// sitting on when the walk stopped — the stop bucket if the rank was
	// reached, the last bucket iterated otherwise (reference reads the
	// iterator BEFORE its own empty-bucket skip, so an exhausted walk
	// leaves it on the final element even when that element is empty).
	idx, count := 0, 0.0
	step, i := 1, 1
	if !forward {
		step, i = -1, len(buckets)
	}
	for ; i >= 1 && i <= len(buckets); i += step {
		idx = i
		if buckets[i-1] == 0 {
			continue
		}
		count += buckets[i-1]
		if count >= rank {
			break
		}
	}
	// Reference's own clamp: numerical drift in the accumulation can put
	// the running count a few ULPs above the stored Count.
	if count > h.Count {
		count = h.Count
	}
	if count < rank {
		// The walk exhausted every bucket without reaching the rank,
		// which needs the stored Count to exceed the buckets' combined
		// reach — a shape this suite's generator never draws, since it
		// derives Count from the buckets it drew. NaN is what cerberus's
		// emitter answers there (its index functions' not-found sentinel
		// routes to NaN); reference answers NaN only for a NaN Sum and
		// gives the last iterated bucket's upper bound otherwise, which
		// is cerberus issue #2405.
		return math.NaN()
	}

	// Rebase the remaining rank inside the stop bucket, per direction —
	// each arm against the running count that arm accumulated.
	if forward {
		rank -= count - buckets[idx-1]
	} else {
		rank = count - rank
	}
	return nativeHistogramBucketValue(h, idx, rank/buckets[idx-1])
}

// nativeHistogramBucketValue maps a 1-based walk index plus the
// fraction of that bucket the rank consumed back to an observation
// value, by the region the index falls in. fraction = 0 is the bucket's
// low edge (nearest -Inf) and fraction = 1 its high edge.
func nativeHistogramBucketValue(h *nativeHistogram, idx int, fraction float64) float64 {
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

func histogramScalarValue(name string, h *nativeHistogram) (float64, error) {
	switch name {
	case "histogram_count":
		return h.Count, nil
	case "histogram_sum":
		return h.Sum, nil
	case "histogram_avg":
		if h.Count == 0 {
			return math.NaN(), nil
		}
		return h.Sum / h.Count, nil
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
func histogramVariance(h *nativeHistogram) float64 {
	if h.Count == 0 {
		return math.NaN()
	}
	mean := h.Sum / h.Count
	base := math.Pow(2, math.Pow(2, -float64(h.Scale)))

	var sumSq float64
	for i, c := range h.PositiveBucketCounts {
		mid := math.Pow(base, float64(h.PositiveOffset)+float64(i)+0.5)
		d := mid - mean
		sumSq += c * d * d
	}
	for i, c := range h.NegativeBucketCounts {
		mid := -math.Pow(base, float64(h.NegativeOffset)+float64(i)+0.5)
		d := mid - mean
		sumSq += c * d * d
	}
	sumSq += h.ZeroCount * mean * mean

	return sumSq / h.Count
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

func histogramFractionValue(lower, upper float64, h *nativeHistogram) float64 {
	if math.IsNaN(lower) || math.IsNaN(upper) {
		return math.NaN()
	}
	if h.Count == 0 {
		return math.NaN()
	}
	if lower >= upper {
		return 0
	}
	count := h.Count
	rl := math.Min(count, histogramRank(h, lower))
	ru := math.Min(count, histogramRank(h, upper))
	return (ru - rl) / count
}

// histogramRank computes R(v): the count of samples in h at or below
// value v, per internal/promql/histogram_value_fns.go's
// histogramRankExpr. logIdx converts a magnitude into the histogram's
// exponential bucket-index space (base^logIdx == |v|); the sPos/sNeg
// helpers then interpolate within the positive/negative bucket arrays.
func histogramRank(h *nativeHistogram, v float64) float64 {
	tn := sumHistogramBuckets(h.NegativeBucketCounts)
	tp := sumHistogramBuckets(h.PositiveBucketCounts)
	z := h.ZeroCount
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
func sPos(pbc []float64, total, p float64) float64 {
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
		cum += pbc[i]
	}
	return cum + pbc[k]*frac
}

// sNeg is the negative-side rank contribution: the count of negative-
// bucket samples at or below magnitude-position q (fractional),
// counted from the far (most-negative) end inward — negative buckets
// closer to zero rank HIGHER, mirroring how more-negative values sort
// lower than less-negative ones.
func sNeg(nbc []float64, total, q float64) float64 {
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
		cum += nbc[i]
	}
	return (total - cum) + nbc[m-1]*(float64(m)-q)
}

func sumHistogramBuckets(xs []float64) float64 {
	var total float64
	for _, x := range xs {
		total += x
	}
	return total
}

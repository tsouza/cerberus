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
// series) — the merge path is a documented gap, not silently dropped
// data.
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

// nativeHistogramQuantileValue walks the bucket layout of a single
// native-histogram sample to compute the phi-th quantile, mirroring
// the bucket-walk algorithm in internal/chsql/histogram_quantile_native.go:
//
//  1. base = 2^(2^-Scale).
//  2. Build the cumulative-count array over
//     reverse(NegativeBucketCounts) ++ [ZeroCount] ++ PositiveBucketCounts.
//  3. target = phi * total; find the first cumulative index >= target.
//  4. Interpolate within that bucket via linear fraction, then map
//     back to a value via the region (negative / zero / positive) the
//     index falls in.
//
// ZeroThreshold is always 0 in cerberus (the default OTel-CH DDL
// doesn't persist it), so the zero-bucket region always evaluates to
// exactly 0.
func nativeHistogramQuantileValue(phi float64, h *property.NativeHistogram) float64 {
	if phi < 0 {
		return math.Inf(-1)
	}
	if phi > 1 {
		return math.Inf(1)
	}

	base := math.Pow(2, math.Pow(2, -float64(h.Scale)))
	nlen := len(h.NegativeBucketCounts)
	plen := len(h.PositiveBucketCounts)

	if phi == 0 {
		if nlen > 0 {
			return -math.Pow(base, float64(h.NegativeOffset)+float64(nlen))
		}
		return 0
	}
	if phi == 1 {
		if plen > 0 {
			return math.Pow(base, float64(h.PositiveOffset)+float64(plen))
		}
		if h.ZeroCount > 0 {
			return 0
		}
		return -math.Pow(base, float64(h.NegativeOffset))
	}

	// cum[i] (Go 0-based) is the cumulative count over
	// reverse(negative) ++ [zero] ++ positive, i.e. the 1-based spec
	// index i+1. Spec's cum[0]=0 is the implicit "before the slice"
	// value, never stored.
	cum := make([]float64, nlen+1+plen)
	var running float64
	for i := 0; i < nlen; i++ {
		running += float64(h.NegativeBucketCounts[nlen-1-i])
		cum[i] = running
	}
	running += float64(h.ZeroCount)
	cum[nlen] = running
	for i := 0; i < plen; i++ {
		running += float64(h.PositiveBucketCounts[i])
		cum[nlen+1+i] = running
	}
	total := running
	if total == 0 {
		return math.NaN()
	}
	target := phi * total

	idx := len(cum) // 1-based spec index; default to the last bucket
	for i, c := range cum {
		if c >= target {
			idx = i + 1
			break
		}
	}

	var prevCum float64
	if idx > 1 {
		prevCum = cum[idx-2]
	}
	currCum := cum[idx-1]
	var fraction float64
	if currCum > prevCum {
		fraction = (target - prevCum) / (currCum - prevCum)
	}

	switch {
	case idx <= nlen:
		return -math.Pow(base, float64(h.NegativeOffset)+float64(nlen-idx)+1-fraction)
	case idx == nlen+1:
		return 0
	default:
		k := idx - nlen - 2
		return math.Pow(base, float64(h.PositiveOffset)+float64(k)+fraction)
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

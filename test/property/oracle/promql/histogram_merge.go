package promql

import (
	"math"

	"github.com/tsouza/cerberus/test/property"
)

// nativeHistogram is the oracle's merged exponential-histogram value.
// Unlike the stored property.NativeHistogram, every count is float64:
// rate and increase multiply a whole distribution by one extrapolation
// factor and therefore produce fractional bucket populations.
type nativeHistogram struct {
	Count                float64
	Sum                  float64
	Scale                int32
	ZeroCount            float64
	PositiveOffset       int32
	PositiveBucketCounts []float64
	NegativeOffset       int32
	NegativeBucketCounts []float64
}

func nativeHistogramFromStored(h *property.NativeHistogram) *nativeHistogram {
	if h == nil {
		return nil
	}
	return &nativeHistogram{
		Count:                float64(h.Count),
		Sum:                  h.Sum,
		Scale:                h.Scale,
		ZeroCount:            float64(h.ZeroCount),
		PositiveOffset:       h.PositiveOffset,
		PositiveBucketCounts: floatBuckets(h.PositiveBucketCounts),
		NegativeOffset:       h.NegativeOffset,
		NegativeBucketCounts: floatBuckets(h.NegativeBucketCounts),
	}
}

func floatBuckets(in []uint64) []float64 {
	out := make([]float64, len(in))
	for i, v := range in {
		out[i] = float64(v)
	}
	return out
}

// mergeNativeHistograms folds every input to the coarsest common scale,
// aligns each signed ladder by absolute bucket index, zero-pads gaps, and
// sums every component. This is the lossless merge exponential histograms
// permit: a fine bucket maps to its containing coarse bucket by arithmetic
// right shift of its absolute index.
func mergeNativeHistograms(in []*nativeHistogram) *nativeHistogram {
	if len(in) == 0 {
		return nil
	}
	targetScale := in[0].Scale
	for _, h := range in[1:] {
		if h.Scale < targetScale {
			targetScale = h.Scale
		}
	}

	out := &nativeHistogram{Scale: targetScale}
	for _, h := range in {
		folded := downscaleNativeHistogram(h, targetScale)
		out.Count += folded.Count
		out.Sum += folded.Sum
		out.ZeroCount += folded.ZeroCount
		out.PositiveOffset, out.PositiveBucketCounts = addBucketLadders(
			out.PositiveOffset, out.PositiveBucketCounts,
			folded.PositiveOffset, folded.PositiveBucketCounts,
		)
		out.NegativeOffset, out.NegativeBucketCounts = addBucketLadders(
			out.NegativeOffset, out.NegativeBucketCounts,
			folded.NegativeOffset, folded.NegativeBucketCounts,
		)
	}
	return out
}

func downscaleNativeHistogram(h *nativeHistogram, targetScale int32) *nativeHistogram {
	if h == nil {
		return nil
	}
	shift := h.Scale - targetScale
	return &nativeHistogram{
		Count:                h.Count,
		Sum:                  h.Sum,
		Scale:                targetScale,
		ZeroCount:            h.ZeroCount,
		PositiveOffset:       downscaledOffset(h.PositiveOffset, h.PositiveBucketCounts, shift),
		PositiveBucketCounts: downscaleBuckets(h.PositiveOffset, h.PositiveBucketCounts, shift),
		NegativeOffset:       downscaledOffset(h.NegativeOffset, h.NegativeBucketCounts, shift),
		NegativeBucketCounts: downscaleBuckets(h.NegativeOffset, h.NegativeBucketCounts, shift),
	}
}

func downscaledOffset(offset int32, buckets []float64, shift int32) int32 {
	if len(buckets) == 0 {
		return offset
	}
	return int32(int64(offset) >> shift)
}

func downscaleBuckets(offset int32, buckets []float64, shift int32) []float64 {
	if len(buckets) == 0 {
		return nil
	}
	start := int64(offset) >> shift
	end := (int64(offset) + int64(len(buckets)) - 1) >> shift
	out := make([]float64, end-start+1)
	for i, count := range buckets {
		target := ((int64(offset) + int64(i)) >> shift) - start
		out[target] += count
	}
	return out
}

func addBucketLadders(leftOffset int32, left []float64, rightOffset int32, right []float64) (int32, []float64) {
	if len(left) == 0 {
		return rightOffset, append([]float64(nil), right...)
	}
	if len(right) == 0 {
		return leftOffset, append([]float64(nil), left...)
	}
	offset := min(leftOffset, rightOffset)
	end := max(int64(leftOffset)+int64(len(left)), int64(rightOffset)+int64(len(right)))
	out := make([]float64, end-int64(offset))
	for i, count := range left {
		out[int64(leftOffset-offset)+int64(i)] += count
	}
	for i, count := range right {
		out[int64(rightOffset-offset)+int64(i)] += count
	}
	return offset, out
}

func addScaledNativeHistogram(dst, src *nativeHistogram, factor float64) *nativeHistogram {
	if dst == nil {
		dst = &nativeHistogram{Scale: src.Scale}
	}
	if dst.Scale != src.Scale {
		target := min(dst.Scale, src.Scale)
		dst = downscaleNativeHistogram(dst, target)
		src = downscaleNativeHistogram(src, target)
	}
	dst.Count += src.Count * factor
	dst.Sum += src.Sum * factor
	dst.ZeroCount += src.ZeroCount * factor
	dst.PositiveOffset, dst.PositiveBucketCounts = addScaledBucketLadders(
		dst.PositiveOffset, dst.PositiveBucketCounts,
		src.PositiveOffset, src.PositiveBucketCounts, factor,
	)
	dst.NegativeOffset, dst.NegativeBucketCounts = addScaledBucketLadders(
		dst.NegativeOffset, dst.NegativeBucketCounts,
		src.NegativeOffset, src.NegativeBucketCounts, factor,
	)
	return dst
}

func addScaledBucketLadders(leftOffset int32, left []float64, rightOffset int32, right []float64, factor float64) (int32, []float64) {
	scaled := make([]float64, len(right))
	for i, count := range right {
		scaled[i] = count * factor
	}
	return addBucketLadders(leftOffset, left, rightOffset, scaled)
}

func scaleNativeHistogram(h *nativeHistogram, factor float64) {
	h.Count *= factor
	h.Sum *= factor
	h.ZeroCount *= factor
	for i := range h.PositiveBucketCounts {
		h.PositiveBucketCounts[i] *= factor
	}
	for i := range h.NegativeBucketCounts {
		h.NegativeBucketCounts[i] *= factor
	}
}

// nativeHistogramReset implements the observable DetectReset conditions for
// this schema: total/zero/bucket regressions and a resolution increase. Sum
// deliberately does not vote; negative observations can lower it without a
// counter restart.
func nativeHistogramReset(current, previous *nativeHistogram) bool {
	if current.Count < previous.Count || current.ZeroCount < previous.ZeroCount || current.Scale > previous.Scale {
		return true
	}
	// A non-reset scale transition can only coarsen. Compare the previous
	// distribution after folding it to the current sample's resolution.
	prev := downscaleNativeHistogram(previous, current.Scale)
	return bucketLadderRegressed(current.PositiveOffset, current.PositiveBucketCounts, prev.PositiveOffset, prev.PositiveBucketCounts) ||
		bucketLadderRegressed(current.NegativeOffset, current.NegativeBucketCounts, prev.NegativeOffset, prev.NegativeBucketCounts)
}

func bucketLadderRegressed(currentOffset int32, current []float64, previousOffset int32, previous []float64) bool {
	for i, count := range previous {
		idx := previousOffset + int32(i)
		cur := bucketAt(currentOffset, current, idx)
		if cur < count {
			return true
		}
	}
	return false
}

func bucketAt(offset int32, buckets []float64, idx int32) float64 {
	pos := int64(idx) - int64(offset)
	if pos < 0 || pos >= int64(len(buckets)) {
		return 0
	}
	return buckets[pos]
}

func nativeHistogramOutcome(h *nativeHistogram) *property.Histogram {
	if h == nil {
		return nil
	}
	base := math.Pow(2, math.Pow(2, -float64(h.Scale)))
	buckets := make([]property.HistogramBucket, 0, len(h.NegativeBucketCounts)+1+len(h.PositiveBucketCounts))
	for i := len(h.NegativeBucketCounts) - 1; i >= 0; i-- {
		count := h.NegativeBucketCounts[i]
		if count == 0 {
			continue
		}
		idx := float64(h.NegativeOffset) + float64(i)
		buckets = append(buckets, property.HistogramBucket{
			Boundaries: 1,
			Lower:      -math.Pow(base, idx+1),
			Upper:      -math.Pow(base, idx),
			Count:      count,
		})
	}
	if h.ZeroCount != 0 {
		lower, upper := math.Copysign(0, -1), 0.0
		if len(h.NegativeBucketCounts) == 0 && len(h.PositiveBucketCounts) > 0 {
			lower = 0
		}
		buckets = append(buckets, property.HistogramBucket{
			Boundaries: 3,
			Lower:      lower,
			Upper:      upper,
			Count:      h.ZeroCount,
		})
	}
	for i, count := range h.PositiveBucketCounts {
		if count == 0 {
			continue
		}
		idx := float64(h.PositiveOffset) + float64(i)
		buckets = append(buckets, property.HistogramBucket{
			Lower: math.Pow(base, idx),
			Upper: math.Pow(base, idx+1),
			Count: count,
		})
	}
	return &property.Histogram{Count: h.Count, Sum: h.Sum, Buckets: buckets}
}

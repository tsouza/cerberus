package promql

import (
	"fmt"
	"math"

	"github.com/prometheus/prometheus/model/histogram"
)

// otelToPromBucketIndex is what separates an OTel exponential-histogram
// bucket index from the Prometheus index naming the SAME bucket.
//
// Both models number buckets by a power of the same base — 2**(2**-scale)
// — and both agree the boundaries are that base's consecutive powers. They
// disagree only about which power the index names. OTel's index i denotes
// the bucket starting at base**i, so it covers (base**i, base**(i+1)];
// Prometheus's index i denotes the bucket ENDING at base**i, so it covers
// (base**(i-1), base**i]. The bucket OTel calls i is therefore the one
// Prometheus calls i+1.
//
// This is derived from the two specifications, not copied from cerberus's
// own translation — the point of this package is to be able to disagree
// with that translation. It nonetheless agrees with what cerberus's wire
// layer emits, which is exactly the corroboration a reviewer should want:
// two independent readings of the same pair of specs reaching the same
// off-by-one.
const otelToPromBucketIndex int32 = 1

// toFloatHistogram builds the reference engine's own histogram type from
// the dense OTel-shaped one.
//
// The bucket array becomes a SINGLE span, because a contiguous OTel array
// is by definition one uninterrupted run of buckets. Prometheus permits
// several spans in order to skip empty stretches, which is a compactness
// choice with no effect on the histogram's meaning; the array is trimmed
// of its leading and trailing empty buckets first so that the span this
// produces is the same one Prometheus itself would have chosen for the
// same data, and an interior empty bucket simply stays inside the span.
// It returns an error rather than truncating a bucket array too long for
// Prometheus's span encoding to describe — see [maxSpanLength].
func (h *Histogram) toFloatHistogram() (*histogram.FloatHistogram, error) {
	positiveOffset, positive := trimEmptyBuckets(h.PositiveOffset, h.PositiveBuckets)
	negativeOffset, negative := trimEmptyBuckets(h.NegativeOffset, h.NegativeBuckets)
	positiveSpans, err := singleSpan(positiveOffset, len(positive))
	if err != nil {
		return nil, fmt.Errorf("positive buckets: %w", err)
	}
	negativeSpans, err := singleSpan(negativeOffset, len(negative))
	if err != nil {
		return nil, fmt.Errorf("negative buckets: %w", err)
	}
	return &histogram.FloatHistogram{
		Schema:          h.Scale,
		ZeroThreshold:   h.ZeroThreshold,
		ZeroCount:       h.ZeroCount,
		Count:           h.Count,
		Sum:             h.Sum,
		PositiveSpans:   positiveSpans,
		PositiveBuckets: positive,
		NegativeSpans:   negativeSpans,
		NegativeBuckets: negative,
	}, nil
}

// maxSpanLength is the longest run of buckets one Prometheus span can
// describe, because histogram.Span's Length field is a uint32.
//
// A bucket array longer than this is not representable, and the only
// alternatives to refusing it are to truncate — dropping observations —
// or to let the conversion wrap, which would produce a well-formed
// histogram carrying the wrong buckets and a comparison that passed or
// failed on nonsense. No fixture comes near the bound; it exists so that
// the narrowing below is provably lossless rather than assumed to be.
const maxSpanLength = math.MaxUint32

// singleSpan describes n contiguous buckets starting at the Prometheus
// index naming the bucket OTel calls otelOffset. An empty array is spanned
// by nothing at all rather than by a zero-length span, which is the shape
// Prometheus's own validation expects.
func singleSpan(otelOffset int32, n int) ([]histogram.Span, error) {
	if n == 0 {
		return nil, nil
	}
	// The lower bound is not reachable — n is a slice length — but it is
	// stated rather than assumed, because what makes the narrowing below
	// provably lossless is the pair of bounds, not the reader's knowledge
	// of where n came from.
	if n < 0 || n > maxSpanLength {
		return nil, fmt.Errorf(
			"%d buckets cannot be described by one span, which carries a length in [0, %d]",
			n, maxSpanLength,
		)
	}
	return []histogram.Span{{
		Offset: otelOffset + otelToPromBucketIndex,
		Length: uint32(n),
	}}, nil
}

// histogramFromFloat converts a reference-engine answer back into the
// dense shape the comparison happens in. See [Histogram] for why the
// comparison lives in that shape rather than in Prometheus's.
func histogramFromFloat(fh *histogram.FloatHistogram) *Histogram {
	positiveOffset, positive := denseBuckets(fh.PositiveSpans, fh.PositiveBuckets)
	negativeOffset, negative := denseBuckets(fh.NegativeSpans, fh.NegativeBuckets)
	return &Histogram{
		Count:           fh.Count,
		Sum:             fh.Sum,
		Scale:           fh.Schema,
		ZeroThreshold:   fh.ZeroThreshold,
		ZeroCount:       fh.ZeroCount,
		PositiveOffset:  positiveOffset,
		PositiveBuckets: positive,
		NegativeOffset:  negativeOffset,
		NegativeBuckets: negative,
	}
}

// denseBuckets flattens Prometheus's span encoding into one contiguous
// array plus the OTel index of its first entry, filling each skipped
// stretch with the explicit zeros the spans exist to omit.
//
// # How a span's Offset is read
//
// Prometheus's own bucket iterator seeds the running index from the FIRST
// span's Offset — which is an absolute index, and may be negative — and
// then advances one bucket at a time. Crossing into a later span, it
// advances past the previous span's last bucket and THEN adds that span's
// Offset, which is why Offset is a gap for every span but the first. The
// loop below walks the same way, so a histogram round-tripped through this
// function and [Histogram.toFloatHistogram] addresses exactly the buckets
// the iterator would have yielded.
func denseBuckets(spans []histogram.Span, buckets []float64) (int32, []float64) {
	var (
		out       []float64
		firstIdx  int32
		haveFirst bool
		next      int32
		bucketIdx int
		// emitted counts what has been appended to out, tracked as the
		// index type rather than derived from len(out): the gap-filling
		// comparison below is against a bucket INDEX, and narrowing a
		// slice length to reach it would be a conversion this function
		// has no way to prove lossless.
		emitted int32
	)
	for i, span := range spans {
		if i == 0 {
			next = span.Offset
		} else {
			next += span.Offset
		}
		for k := uint32(0); k < span.Length; k++ {
			if bucketIdx >= len(buckets) {
				// Only reachable for a histogram whose spans claim more
				// buckets than it carries, which the reference engine
				// does not produce. Stopping is what its own iterator
				// does in that case.
				break
			}
			if !haveFirst {
				firstIdx, haveFirst = next, true
			}
			for emitted+firstIdx < next {
				out = append(out, 0)
				emitted++
			}
			out = append(out, buckets[bucketIdx])
			emitted++
			bucketIdx++
			next++
		}
	}
	if !haveFirst {
		return 0, nil
	}
	return trimEmptyBuckets(firstIdx-otelToPromBucketIndex, out)
}

// trimEmptyBuckets drops the leading and trailing zero-count buckets,
// moving the offset to keep every remaining bucket addressing the same
// index it did before.
//
// This is a re-encoding, not a relaxation. An empty bucket at either end
// carries no observations, so `offset 0, [0, 3, 5]` and `offset 1, [3, 5]`
// are two spellings of one histogram; comparing them as raw arrays would
// report a disagreement about the ENCODING, which is not something either
// engine promises anything about. Both sides of the comparison are trimmed
// through this one function, so neither can win an argument the other
// never entered. An INTERIOR zero is left alone: it sits between two
// occupied buckets, so no offset can express its absence.
func trimEmptyBuckets(offset int32, buckets []float64) (int32, []float64) {
	lo := 0
	for lo < len(buckets) && buckets[lo] == 0 {
		lo++
	}
	hi := len(buckets)
	for hi > lo && buckets[hi-1] == 0 {
		hi--
	}
	if lo == hi {
		return 0, nil
	}
	return offset + int32(lo), buckets[lo:hi]
}

// EqualHistograms reports whether two native-histogram samples agree.
//
// Every field is compared through [EqualValues], so the exact-equality
// rule — and its single NaN==NaN allowance — governs a histogram's counts
// and sum exactly as it governs a float sample's value. There is no
// per-field tolerance and no ignored field: a histogram answer is only
// equal when its scale, its zero bucket, its offsets and every bucket
// count agree.
//
// Both sides are trimmed to the canonical encoding first, for the reason
// [trimEmptyBuckets] gives.
func EqualHistograms(a, b *Histogram) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Scale != b.Scale {
		return false
	}
	if !EqualValues(a.Count, b.Count) || !EqualValues(a.Sum, b.Sum) {
		return false
	}
	if !EqualValues(a.ZeroThreshold, b.ZeroThreshold) ||
		!EqualValues(a.ZeroCount, b.ZeroCount) {
		return false
	}
	return equalBuckets(a.PositiveOffset, a.PositiveBuckets, b.PositiveOffset, b.PositiveBuckets) &&
		equalBuckets(a.NegativeOffset, a.NegativeBuckets, b.NegativeOffset, b.NegativeBuckets)
}

func equalBuckets(aOffset int32, a []float64, bOffset int32, b []float64) bool {
	aOffset, a = trimEmptyBuckets(aOffset, a)
	bOffset, b = trimEmptyBuckets(bOffset, b)
	if aOffset != bOffset || len(a) != len(b) {
		return false
	}
	for i := range a {
		if !EqualValues(a[i], b[i]) {
			return false
		}
	}
	return true
}

package promql

import (
	"math"
	"reflect"
	"testing"

	"github.com/prometheus/prometheus/model/histogram"
)

// TestBucketBoundsMatchTheOTelSpecification is the load-bearing test of
// this file, and the reason it asserts through Prometheus's OWN bucket
// iterator rather than against arithmetic written here.
//
// [otelToPromBucketIndex] claims the bucket OTel calls i is the one
// Prometheus calls i+1. Re-deriving the boundaries by hand would only
// check that claim against the same reasoning that produced it. Asking
// the reference engine where it thinks each bucket's edges are checks it
// against the implementation the comparison is actually run by, so an
// off-by-one — in either direction — moves a boundary by a whole factor
// of the base and fails here loudly.
func TestBucketBoundsMatchTheOTelSpecification(t *testing.T) {
	t.Parallel()

	// Scale 0 means base 2, so OTel bucket i covers (2**i, 2**(i+1)].
	for _, tc := range []struct {
		name         string
		otelOffset   int32
		lower, upper float64
	}{
		{name: "bucket 0 is (1, 2]", otelOffset: 0, lower: 1, upper: 2},
		{name: "bucket 1 is (2, 4]", otelOffset: 1, lower: 2, upper: 4},
		{name: "bucket -1 is (0.5, 1]", otelOffset: -1, lower: 0.5, upper: 1},
		{name: "bucket -3 is (0.125, 0.25]", otelOffset: -3, lower: 0.125, upper: 0.25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := &Histogram{
				Count:           1,
				Scale:           0,
				PositiveOffset:  tc.otelOffset,
				PositiveBuckets: []float64{1},
			}
			it := h.toFloatHistogram().PositiveBucketIterator()
			if !it.Next() {
				t.Fatal("converted histogram yielded no positive bucket")
			}
			got := it.At()
			if got.Lower != tc.lower || got.Upper != tc.upper {
				t.Errorf("OTel bucket %d became (%v, %v], want (%v, %v]",
					tc.otelOffset, got.Lower, got.Upper, tc.lower, tc.upper)
			}
			if it.Next() {
				t.Error("a one-entry bucket array produced more than one bucket")
			}
		})
	}
}

// TestScaleIsPrometheusSchema pins the other half of the index mapping:
// the two models disagree about bucket NUMBERING but agree exactly about
// the base, so OTel's scale passes through as Prometheus's schema
// unchanged. A fixture seeded at scale 3 whose buckets were read at
// schema 0 would place every observation 8 times too far apart.
func TestScaleIsPrometheusSchema(t *testing.T) {
	t.Parallel()

	const scale = 3
	h := &Histogram{Count: 1, Scale: scale, PositiveOffset: 0, PositiveBuckets: []float64{1}}
	fh := h.toFloatHistogram()
	if fh.Schema != scale {
		t.Fatalf("Schema = %d, want %d", fh.Schema, scale)
	}

	it := fh.PositiveBucketIterator()
	if !it.Next() {
		t.Fatal("converted histogram yielded no positive bucket")
	}
	// Base is 2**(2**-scale), so OTel bucket 0 ends one base-step above 1.
	if want := math.Exp2(math.Exp2(-scale)); it.At().Upper != want {
		t.Errorf("bucket upper bound = %v, want %v", it.At().Upper, want)
	}
}

// TestSpanRoundTripPreservesEveryBucket walks a histogram out to
// Prometheus's span encoding and back, and requires the buckets to land
// on the same indices carrying the same counts. Both directions are used
// in the same comparison — the seeded rows go out through
// toFloatHistogram and the reference's answer comes back through
// histogramFromFloat — so an asymmetry between them would compare a
// histogram against a shifted copy of itself.
func TestSpanRoundTripPreservesEveryBucket(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   *Histogram
		want *Histogram
	}{
		{
			name: "positive and negative buckets",
			in: &Histogram{
				Count: 12, Sum: 40.5, Scale: 2, ZeroCount: 1,
				PositiveOffset: 3, PositiveBuckets: []float64{4, 5},
				NegativeOffset: -2, NegativeBuckets: []float64{2},
			},
			want: &Histogram{
				Count: 12, Sum: 40.5, Scale: 2, ZeroCount: 1,
				PositiveOffset: 3, PositiveBuckets: []float64{4, 5},
				NegativeOffset: -2, NegativeBuckets: []float64{2},
			},
		},
		{
			name: "an interior empty bucket stays inside the run",
			in: &Histogram{
				Count: 8, Scale: 0,
				PositiveOffset: 0, PositiveBuckets: []float64{3, 0, 5},
			},
			want: &Histogram{
				Count: 8, Scale: 0,
				PositiveOffset: 0, PositiveBuckets: []float64{3, 0, 5},
			},
		},
		{
			name: "leading and trailing empties are trimmed, not moved",
			in: &Histogram{
				Count: 3, Scale: 0,
				PositiveOffset: -4, PositiveBuckets: []float64{0, 0, 3, 0},
			},
			want: &Histogram{
				Count: 3, Scale: 0,
				PositiveOffset: -2, PositiveBuckets: []float64{3},
			},
		},
		{
			name: "an entirely empty side has no buckets at all",
			in: &Histogram{
				Count: 1, Scale: 0, ZeroCount: 1,
				PositiveOffset: 7, PositiveBuckets: []float64{0, 0},
			},
			want: &Histogram{Count: 1, Scale: 0, ZeroCount: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := histogramFromFloat(tc.in.toFloatHistogram())
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("round trip\n  got:  %+v\n  want: %+v", got, tc.want)
			}
		})
	}
}

// TestDenseBucketsReadsMultiSpanGaps pins the one piece of span
// arithmetic a single-span round trip can never exercise: Prometheus
// compacts a run of empty buckets by starting a NEW span, whose Offset is
// a gap measured from one past the previous span's last bucket. Reading
// that gap as an absolute index — or forgetting the "one past" — would
// silently shift every bucket after the first span, and the reference
// engine emits multi-span histograms whenever an aggregation leaves a
// hole.
func TestDenseBucketsReadsMultiSpanGaps(t *testing.T) {
	t.Parallel()

	// Buckets at Prometheus indices 1, 2 and then 6: the second span's
	// Offset of 3 is the gap from index 3 (one past the first span) to
	// index 6.
	fh := &histogram.FloatHistogram{
		Schema: 0,
		PositiveSpans: []histogram.Span{
			{Offset: 1, Length: 2},
			{Offset: 3, Length: 1},
		},
		PositiveBuckets: []float64{4, 5, 9},
	}

	// Cross-check the indices against the engine's own iterator before
	// asserting on the conversion, so this test cannot pass by agreeing
	// with a misreading of the encoding.
	var indices []int32
	for it := fh.PositiveBucketIterator(); it.Next(); {
		indices = append(indices, it.At().Index)
	}
	if want := []int32{1, 2, 6}; !reflect.DeepEqual(indices, want) {
		t.Fatalf("reference iterator yielded indices %v, want %v", indices, want)
	}

	offset, buckets := denseBuckets(fh.PositiveSpans, fh.PositiveBuckets)
	// Prometheus index 1 is OTel index 0, and the gap is filled with the
	// explicit zeros the spans exist to omit.
	if want := int32(0); offset != want {
		t.Errorf("offset = %d, want %d", offset, want)
	}
	if want := []float64{4, 5, 0, 0, 0, 9}; !reflect.DeepEqual(buckets, want) {
		t.Errorf("buckets = %v, want %v", buckets, want)
	}
}

// TestEqualHistogramsSeesEveryField mutates one component at a time and
// requires each mutation to be noticed. A comparator that silently
// ignored a field would let a whole axis of the answer go unchecked while
// every fixture still passed, which is the hollow green this layer exists
// to prevent — so the test is written as "every field must be able to
// FAIL", not as "equal histograms compare equal".
func TestEqualHistogramsSeesEveryField(t *testing.T) {
	t.Parallel()

	base := func() *Histogram {
		return &Histogram{
			Count: 12, Sum: 40.5, Scale: 2, ZeroThreshold: 0, ZeroCount: 1,
			PositiveOffset: 3, PositiveBuckets: []float64{4, 5},
			NegativeOffset: -2, NegativeBuckets: []float64{2},
		}
	}
	if !EqualHistograms(base(), base()) {
		t.Fatal("a histogram compared unequal to itself")
	}

	for _, tc := range []struct {
		field  string
		mutate func(*Histogram)
	}{
		{"Count", func(h *Histogram) { h.Count = 13 }},
		{"Sum", func(h *Histogram) { h.Sum = 40.75 }},
		{"Scale", func(h *Histogram) { h.Scale = 3 }},
		{"ZeroThreshold", func(h *Histogram) { h.ZeroThreshold = 0.5 }},
		{"ZeroCount", func(h *Histogram) { h.ZeroCount = 2 }},
		{"PositiveOffset", func(h *Histogram) { h.PositiveOffset = 4 }},
		{"PositiveBuckets value", func(h *Histogram) { h.PositiveBuckets = []float64{4, 6} }},
		{"PositiveBuckets length", func(h *Histogram) { h.PositiveBuckets = []float64{4, 5, 1} }},
		{"NegativeOffset", func(h *Histogram) { h.NegativeOffset = -1 }},
		{"NegativeBuckets value", func(h *Histogram) { h.NegativeBuckets = []float64{3} }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()

			mutated := base()
			tc.mutate(mutated)
			if EqualHistograms(base(), mutated) {
				t.Errorf("a histogram differing only in %s compared equal", tc.field)
			}
		})
	}
}

// TestEqualHistogramsIgnoresTheEncodingOnly is the other half of the
// previous test: the ONE thing the comparator must not report is a
// difference in how the same bucket populations were spelled. Two
// encodings of one histogram are equal; a third with a genuinely
// different population is not, and it is placed alongside them so this
// test cannot pass by comparing nothing at all.
func TestEqualHistogramsIgnoresTheEncodingOnly(t *testing.T) {
	t.Parallel()

	padded := &Histogram{
		Count: 8, Scale: 0,
		PositiveOffset: -2, PositiveBuckets: []float64{0, 0, 3, 5, 0},
	}
	trimmed := &Histogram{
		Count: 8, Scale: 0,
		PositiveOffset: 0, PositiveBuckets: []float64{3, 5},
	}
	shifted := &Histogram{
		Count: 8, Scale: 0,
		PositiveOffset: 1, PositiveBuckets: []float64{3, 5},
	}

	if !EqualHistograms(padded, trimmed) {
		t.Error("two encodings of one histogram compared unequal")
	}
	if EqualHistograms(trimmed, shifted) {
		t.Error("histograms whose buckets sit at different indices compared equal")
	}
}

// TestEqualHistogramsMatchesEqualValuesOnNaN keeps the histogram
// comparison under the same equality rule float samples obey rather than
// inventing a second one. NaN is a legitimate PromQL answer, and a
// histogram-valued NaN sum reaches this comparator exactly as a
// float-valued one reaches EqualValues.
func TestEqualHistogramsMatchesEqualValuesOnNaN(t *testing.T) {
	t.Parallel()

	nan := func() *Histogram {
		return &Histogram{
			Count: 1, Sum: math.NaN(), Scale: 0,
			PositiveOffset: 0, PositiveBuckets: []float64{1},
		}
	}
	if !EqualHistograms(nan(), nan()) {
		t.Error("two NaN-summed histograms compared unequal, but EqualValues calls NaN == NaN")
	}

	finite := nan()
	finite.Sum = 0
	if EqualHistograms(nan(), finite) {
		t.Error("a NaN sum compared equal to a finite one")
	}
}

// TestEqualHistogramsHandlesAbsence pins the nil cases, which the
// comparator reaches whenever one side answered with a float and the
// other with a histogram.
func TestEqualHistogramsHandlesAbsence(t *testing.T) {
	t.Parallel()

	h := &Histogram{Count: 1, Scale: 0, PositiveOffset: 0, PositiveBuckets: []float64{1}}
	if !EqualHistograms(nil, nil) {
		t.Error("two absent histograms compared unequal")
	}
	if EqualHistograms(h, nil) || EqualHistograms(nil, h) {
		t.Error("a histogram compared equal to no histogram at all")
	}
}

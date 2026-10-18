package prom

import (
	"reflect"
	"testing"

	"github.com/prometheus/prometheus/model/histogram"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestReferenceHistogramConversion pins the bucket-index mapping between
// cerberus's decoded OTel histogram and the reference engine's native
// one, in both directions: OTel bucket k is Prometheus bucket k+1.
func TestReferenceHistogramConversion(t *testing.T) {
	t.Parallel()
	hv := &chclient.HistogramValue{
		Count: 30, Sum: 15, Scale: 0, ZeroCount: 1,
		PositiveOffset: -1, PositiveBucketCounts: []float64{14, 15},
		NegativeOffset: 2, NegativeBucketCounts: []float64{3},
	}
	fh, err := floatHistogramFromValue(hv)
	if err != nil {
		t.Fatalf("floatHistogramFromValue: %v", err)
	}
	if want := []histogram.Span{{Offset: 0, Length: 2}}; !reflect.DeepEqual(fh.PositiveSpans, want) {
		t.Errorf("positive spans %v, want %v", fh.PositiveSpans, want)
	}
	if want := []histogram.Span{{Offset: 3, Length: 1}}; !reflect.DeepEqual(fh.NegativeSpans, want) {
		t.Errorf("negative spans %v, want %v", fh.NegativeSpans, want)
	}
	if err := fh.Validate(); err != nil {
		t.Errorf("converted histogram is invalid: %v", err)
	}
	back, err := histogramValueFromFloat(fh)
	if err != nil {
		t.Fatalf("histogramValueFromFloat: %v", err)
	}
	if !reflect.DeepEqual(back, hv) {
		t.Errorf("round trip %+v, want %+v", back, hv)
	}
}

// TestReferenceHistogramGapsBecomeZeroBuckets pins that a histogram the
// reference engine returns with gaps between its spans expands into one
// dense OTel bucket array.
func TestReferenceHistogramGapsBecomeZeroBuckets(t *testing.T) {
	t.Parallel()
	fh := &histogram.FloatHistogram{
		Schema: 1, Count: 6, Sum: 9,
		// Prometheus buckets 2, 3, then 6: a gap of two.
		PositiveSpans:   []histogram.Span{{Offset: 2, Length: 2}, {Offset: 2, Length: 1}},
		PositiveBuckets: []float64{1, 2, 3},
	}
	hv, err := histogramValueFromFloat(fh)
	if err != nil {
		t.Fatalf("histogramValueFromFloat: %v", err)
	}
	if hv.PositiveOffset != 1 || !reflect.DeepEqual(hv.PositiveBucketCounts, []float64{1, 2, 0, 0, 3}) {
		t.Errorf("got offset %d buckets %v, want offset 1 buckets [1 2 0 0 3]", hv.PositiveOffset, hv.PositiveBucketCounts)
	}
	if hv.NegativeBucketCounts != nil {
		t.Errorf("negative buckets %v, want none", hv.NegativeBucketCounts)
	}
}

// TestReferenceHistogramSchemaBounds pins the two scales outside
// Prometheus's exponential schemas: a finer one is merged down to the
// finest schema, a coarser one is refused rather than misread.
func TestReferenceHistogramSchemaBounds(t *testing.T) {
	t.Parallel()
	fine := &chclient.HistogramValue{
		Count: 3, Sum: 3, Scale: histogram.ExponentialSchemaMax + 1,
		PositiveOffset: 0, PositiveBucketCounts: []float64{1, 2},
	}
	fh, err := floatHistogramFromValue(fine)
	if err != nil {
		t.Fatalf("finer scale: %v", err)
	}
	if fh.Schema != histogram.ExponentialSchemaMax {
		t.Errorf("finer scale converted to schema %d, want %d", fh.Schema, histogram.ExponentialSchemaMax)
	}
	coarse := &chclient.HistogramValue{Scale: histogram.ExponentialSchemaMin - 1}
	if _, err := floatHistogramFromValue(coarse); err == nil {
		t.Errorf("coarser scale converted without error")
	}
	custom := &histogram.FloatHistogram{Schema: histogram.CustomBucketsSchema}
	if _, err := histogramValueFromFloat(custom); err == nil {
		t.Errorf("custom-bucket histogram converted without error")
	}
}

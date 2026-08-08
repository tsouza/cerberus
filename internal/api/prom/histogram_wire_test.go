package prom

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestHistogramFromValue_PositiveBucketBoundaries pins the positive-side
// bucket math: `(base^(offset+i), base^(offset+i+1)]` — lower
// exclusive, upper inclusive (boundary 0) — matching
// internal/chsql's histogramQuantileNativeValueFrag doc comment for the
// same schema.
func TestHistogramFromValue_PositiveBucketBoundaries(t *testing.T) {
	t.Parallel()

	hv := &chclient.HistogramValue{
		Count:                3,
		Sum:                  10,
		Scale:                0, // base = 2^(2^0) = 2
		PositiveOffset:       1,
		PositiveBucketCounts: []uint64{2, 3},
	}
	h := histogramFromValue(hv)
	if h == nil {
		t.Fatalf("histogramFromValue returned nil")
	}
	if len(h.Buckets) != 2 {
		t.Fatalf("got %d buckets, want 2: %+v", len(h.Buckets), h.Buckets)
	}
	// i=0: (2^1, 2^2] = (2, 4], count 2.
	b0 := h.Buckets[0]
	if b0.Boundaries != boundariesLowerExclusiveUpperInclusive || b0.Lower != "2" || b0.Upper != "4" || b0.Count != "2" {
		t.Errorf("bucket 0: got %+v, want {0, 2, 4, 2}", b0)
	}
	// i=1: (2^2, 2^3] = (4, 8], count 3.
	b1 := h.Buckets[1]
	if b1.Boundaries != boundariesLowerExclusiveUpperInclusive || b1.Lower != "4" || b1.Upper != "8" || b1.Count != "3" {
		t.Errorf("bucket 1: got %+v, want {0, 4, 8, 3}", b1)
	}
	if h.Count != "3" || h.Sum != "10" {
		t.Errorf("Count/Sum: got %q/%q, want 3/10", h.Count, h.Sum)
	}
}

// TestHistogramFromValue_NegativeBucketBoundariesAscending pins the
// negative-side bucket math AND the ascending-value walk order: bucket i
// covers `[-base^(offset+i+1), -base^(offset+i))` — lower inclusive,
// upper exclusive (boundary 1) — and the largest-magnitude (most
// negative) bucket is emitted FIRST so the overall Buckets list is
// ascending by value.
func TestHistogramFromValue_NegativeBucketBoundariesAscending(t *testing.T) {
	t.Parallel()

	hv := &chclient.HistogramValue{
		Scale:                0, // base = 2
		NegativeOffset:       0,
		NegativeBucketCounts: []uint64{5, 7}, // i=0 -> [-2,-1); i=1 -> [-4,-2)
	}
	h := histogramFromValue(hv)
	if len(h.Buckets) != 2 {
		t.Fatalf("got %d buckets, want 2: %+v", len(h.Buckets), h.Buckets)
	}
	// Ascending by value: [-4,-2) (i=1, count 7) comes before [-2,-1) (i=0, count 5).
	b0 := h.Buckets[0]
	if b0.Boundaries != boundariesLowerInclusiveUpperExclusive || b0.Lower != "-4" || b0.Upper != "-2" || b0.Count != "7" {
		t.Errorf("bucket 0: got %+v, want {1, -4, -2, 7}", b0)
	}
	b1 := h.Buckets[1]
	if b1.Boundaries != boundariesLowerInclusiveUpperExclusive || b1.Lower != "-2" || b1.Upper != "-1" || b1.Count != "5" {
		t.Errorf("bucket 1: got %+v, want {1, -2, -1, 5}", b1)
	}
}

// TestHistogramFromValue_ZeroBucketSymmetric pins the two-sided zero
// band when the distribution has observations on both sides of zero.
func TestHistogramFromValue_ZeroBucketSymmetric(t *testing.T) {
	t.Parallel()

	hv := &chclient.HistogramValue{
		ZeroThreshold:        0.5,
		ZeroCount:            9,
		PositiveBucketCounts: []uint64{1},
		NegativeBucketCounts: []uint64{1},
	}
	h := histogramFromValue(hv)
	var zero *HistogramBucket
	for i := range h.Buckets {
		if h.Buckets[i].Count == "9" {
			zero = &h.Buckets[i]
		}
	}
	if zero == nil {
		t.Fatalf("no zero bucket found in %+v", h.Buckets)
	}
	if zero.Boundaries != boundariesBothInclusive || zero.Lower != "-0.5" || zero.Upper != "0.5" {
		t.Errorf("zero bucket: got %+v, want {3, -0.5, 0.5, 9}", *zero)
	}
}

// TestHistogramFromValue_ZeroBucketOneSidedClamp pins reference
// Prometheus's one-sided clamp (promql/quantile.go:263-273): a
// distribution with observations on only ONE side of zero has its zero
// band collapsed on the side it never recorded, rather than spanning
// the full symmetric [-ZeroThreshold, +ZeroThreshold] band.
func TestHistogramFromValue_ZeroBucketOneSidedClamp(t *testing.T) {
	t.Parallel()

	t.Run("positive-only clamps lower to 0", func(t *testing.T) {
		t.Parallel()
		hv := &chclient.HistogramValue{
			ZeroThreshold:        0.5,
			ZeroCount:            4,
			PositiveBucketCounts: []uint64{1},
		}
		h := histogramFromValue(hv)
		zero := findBucketByCount(t, h.Buckets, "4")
		if zero.Lower != "0" || zero.Upper != "0.5" {
			t.Errorf("positive-only zero band: got [%s, %s], want [0, 0.5]", zero.Lower, zero.Upper)
		}
	})
	t.Run("negative-only clamps upper to 0", func(t *testing.T) {
		t.Parallel()
		hv := &chclient.HistogramValue{
			ZeroThreshold:        0.5,
			ZeroCount:            4,
			NegativeBucketCounts: []uint64{1},
		}
		h := histogramFromValue(hv)
		zero := findBucketByCount(t, h.Buckets, "4")
		if zero.Lower != "-0.5" || zero.Upper != "0" {
			t.Errorf("negative-only zero band: got [%s, %s], want [-0.5, 0]", zero.Lower, zero.Upper)
		}
	})
}

func findBucketByCount(t *testing.T, buckets []HistogramBucket, count string) HistogramBucket {
	t.Helper()
	for _, b := range buckets {
		if b.Count == count {
			return b
		}
	}
	t.Fatalf("no bucket with count %q in %+v", count, buckets)
	return HistogramBucket{}
}

// TestHistogramFromValue_SkipsZeroCountBuckets pins the
// zero-count-bucket skip, matching upstream's own `if bucket.Count == 0
// { continue }` — a bucket array may legitimately carry zeros anywhere,
// and each one must be absent from the wire, not emitted as an empty
// entry.
func TestHistogramFromValue_SkipsZeroCountBuckets(t *testing.T) {
	t.Parallel()

	hv := &chclient.HistogramValue{
		PositiveBucketCounts: []uint64{0, 5, 0},
		NegativeBucketCounts: []uint64{0},
		ZeroCount:            0,
	}
	h := histogramFromValue(hv)
	if len(h.Buckets) != 1 {
		t.Fatalf("got %d buckets, want 1 (only the count=5 bucket): %+v", len(h.Buckets), h.Buckets)
	}
	if h.Buckets[0].Count != "5" {
		t.Errorf("surviving bucket count: got %q, want 5", h.Buckets[0].Count)
	}
}

// TestHistogramFromValue_Nil pins the nil-in/nil-out contract so a
// caller can pass chclient.Sample.Histogram straight through without a
// separate nil check.
func TestHistogramFromValue_Nil(t *testing.T) {
	t.Parallel()
	if got := histogramFromValue(nil); got != nil {
		t.Errorf("histogramFromValue(nil) = %+v, want nil", got)
	}
}

// TestHistogramBucket_MarshalJSON pins the compact 4-element array
// shape upstream's model.HistogramBucket.MarshalJSON produces.
func TestHistogramBucket_MarshalJSON(t *testing.T) {
	t.Parallel()
	b := HistogramBucket{Boundaries: 1, Lower: "-4", Upper: "-2", Count: "7"}
	got, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `[1,"-4","-2","7"]`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestVectorSample_MarshalJSON_HistogramShape pins the wire shape for a
// hand-constructed histogram-valued VectorSample: a `histogram` key
// carrying `[ts, {count, sum, buckets}]`, and NO `value` key at all —
// matching upstream Prometheus's mutual exclusivity
// (web/api/v1/json_codec.go's marshalSampleJSON).
func TestVectorSample_MarshalJSON_HistogramShape(t *testing.T) {
	t.Parallel()

	hs := Sample{1717171717.5, &Histogram{
		Count: "5", Sum: "12.5",
		Buckets: []HistogramBucket{{Boundaries: 0, Lower: "1", Upper: "2", Count: "5"}},
	}}
	vs := VectorSample{
		Metric:    map[string]string{"__name__": "request_duration_seconds"},
		Histogram: &hs,
	}
	got, err := json.Marshal(vs)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, present := decoded["value"]; present {
		t.Errorf("wire object carries a `value` key alongside `histogram`: %s", got)
	}
	histRaw, present := decoded["histogram"]
	if !present {
		t.Fatalf("wire object has no `histogram` key: %s", got)
	}

	var pair [2]json.RawMessage
	if err := json.Unmarshal(histRaw, &pair); err != nil {
		t.Fatalf("histogram is not a 2-element array: %v (%s)", err, histRaw)
	}
	var body struct {
		Count   string `json:"count"`
		Sum     string `json:"sum"`
		Buckets []any  `json:"buckets"`
	}
	if err := json.Unmarshal(pair[1], &body); err != nil {
		t.Fatalf("histogram value is not {count,sum,buckets}: %v (%s)", err, pair[1])
	}
	if body.Count != "5" || body.Sum != "12.5" || len(body.Buckets) != 1 {
		t.Errorf("histogram body: got %+v, want Count=5 Sum=12.5 len(Buckets)=1", body)
	}
}

// TestVectorSample_MarshalJSON_FloatShapeUnchanged pins that the
// existing float-valued wire shape is byte-identical to before the
// Value field became a pointer: `value` present, `histogram` absent.
func TestVectorSample_MarshalJSON_FloatShapeUnchanged(t *testing.T) {
	t.Parallel()

	vs := VectorSample{
		Metric: map[string]string{"__name__": "up"},
		Value:  &Sample{1717171717, "1"},
	}
	got, err := json.Marshal(vs)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"metric":{"__name__":"up"},"value":[1717171717,"1"]}`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// TestMatrixSample_MarshalJSON_HistogramShape pins the matrix sibling:
// a `histograms` key alongside (or instead of) `values`.
func TestMatrixSample_MarshalJSON_HistogramShape(t *testing.T) {
	t.Parallel()

	ms := MatrixSample{
		Metric: map[string]string{"__name__": "request_duration_seconds"},
		Histograms: []Sample{
			{1717171717.0, &Histogram{Count: "1", Sum: "1", Buckets: nil}},
		},
	}
	got, err := json.Marshal(ms)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, present := decoded["values"]; present {
		t.Errorf("wire object carries a `values` key for an all-histogram series: %s", got)
	}
	if _, present := decoded["histograms"]; !present {
		t.Fatalf("wire object has no `histograms` key: %s", got)
	}
}

// TestToVector_PopulatesHistogram pins the handler-side wiring: a
// chclient.Sample carrying a decoded Histogram routes to
// VectorSample.Histogram, not .Value, through toVector.
func TestToVector_PopulatesHistogram(t *testing.T) {
	t.Parallel()

	ts := time.Unix(1717171717, 0).UTC()
	hv := &chclient.HistogramValue{Count: 3, Sum: 6, PositiveBucketCounts: []uint64{3}}
	samples := []chclient.Sample{
		{MetricName: "h", Labels: map[string]string{"job": "api"}, Timestamp: ts, Histogram: hv},
	}
	out := toVector(samples, ts)
	if len(out) != 1 {
		t.Fatalf("got %d vector samples, want 1", len(out))
	}
	if out[0].Value != nil {
		t.Errorf("Value: got %+v, want nil for a histogram-valued row", out[0].Value)
	}
	if out[0].Histogram == nil {
		t.Fatalf("Histogram: got nil, want a populated pair")
	}
	body, ok := out[0].Histogram[1].(*Histogram)
	if !ok {
		t.Fatalf("Histogram[1] is %T, want *Histogram", out[0].Histogram[1])
	}
	if body.Count != "3" || body.Sum != "6" {
		t.Errorf("Histogram body: got Count=%q Sum=%q, want 3/6", body.Count, body.Sum)
	}
}

// TestMatrixFromSamples_PopulatesHistogram pins the matrix instant
// pivot's histogram wiring.
func TestMatrixFromSamples_PopulatesHistogram(t *testing.T) {
	t.Parallel()

	ts := time.Unix(1717171717, 0).UTC()
	hv := &chclient.HistogramValue{Count: 1, PositiveBucketCounts: []uint64{1}}
	samples := []chclient.Sample{
		{MetricName: "h", Labels: map[string]string{"job": "api"}, Timestamp: ts, Histogram: hv},
	}
	out := matrixFromSamples(samples)
	if len(out) != 1 {
		t.Fatalf("got %d matrix series, want 1", len(out))
	}
	if len(out[0].Values) != 0 {
		t.Errorf("Values: got %v, want empty for an all-histogram series", out[0].Values)
	}
	if len(out[0].Histograms) != 1 {
		t.Fatalf("Histograms: got %d entries, want 1", len(out[0].Histograms))
	}
}

// TestMatrixFromCursor_PopulatesHistogram pins the streaming matrix
// pivot's histogram wiring, including the "at least one histogram point
// keeps the series" contract (the len(Values) > 0 filter alone would
// have dropped an all-histogram series before this change).
func TestMatrixFromCursor_PopulatesHistogram(t *testing.T) {
	t.Parallel()

	ts := time.Unix(1717171717, 0).UTC()
	hv := &chclient.HistogramValue{Count: 1, PositiveBucketCounts: []uint64{1}}
	samples := []chclient.Sample{
		{MetricName: "h", Labels: map[string]string{"job": "api"}, Timestamp: ts, Histogram: hv},
	}
	out, err := matrixFromCursor(&orderTestCursor{samples: samples, idx: -1}, ts.Add(-time.Hour), ts.Add(time.Hour), 10*time.Second)
	if err != nil {
		t.Fatalf("matrixFromCursor: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d matrix series, want 1", len(out))
	}
	if len(out[0].Histograms) != 1 || len(out[0].Values) != 0 {
		t.Errorf("got Values=%v Histograms=%v, want 0 Values / 1 Histograms", out[0].Values, out[0].Histograms)
	}
}

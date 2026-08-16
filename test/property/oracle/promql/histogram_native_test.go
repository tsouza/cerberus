package promql

import (
	"math"
	"testing"
	"time"

	"github.com/tsouza/cerberus/test/property"
)

// histSeries builds one native-histogram SeriesData with a single
// sample at `offsetSec` seconds after anchor. The numeric expected
// values in these tests are cross-checked by hand against
// test/spec/promql/*.txtar fixtures that exercise the same seed
// against real cerberus + chDB (see the PR description for the
// mapping); this keeps the oracle's bucket-walk math pinned to
// cerberus's actual emitted semantics, not just self-consistent.
func histSeries(name string, lbls map[string]string, offsetSec int, h property.NativeHistogram) property.SeriesData {
	return property.SeriesData{
		MetricName: name,
		Labels:     lbls,
		Points: []property.Point{
			{TimestampMs: ts(offsetSec), Histogram: &h},
		},
	}
}

func TestNativeHistogramQuantile_P99(t *testing.T) {
	d := build(histSeries("http_server_duration_exp_hist", map[string]string{"service": "api"}, 60, property.NativeHistogram{
		Count:                6,
		Scale:                0,
		PositiveOffset:       0,
		PositiveBucketCounts: []uint64{1, 2, 3},
	}))
	o := eval(d, `histogram_quantile(0.99, http_server_duration_exp_hist)`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"service": "api"}, 7.889861635946874),
	})
}

func TestNativeHistogramQuantile_P50(t *testing.T) {
	d := build(histSeries("http_server_duration_exp_hist", map[string]string{"service": "api"}, 60, property.NativeHistogram{
		Count:                6,
		Scale:                0,
		PositiveOffset:       0,
		PositiveBucketCounts: []uint64{1, 2, 3},
	}))
	o := eval(d, `histogram_quantile(0.5, http_server_duration_exp_hist)`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"service": "api"}, 4.0),
	})
}

func TestNativeHistogramQuantile_MultiSeries(t *testing.T) {
	d := build(
		histSeries("http_server_duration_exp_hist", map[string]string{"service": "api"}, 60, property.NativeHistogram{
			Count:                6,
			Scale:                0,
			PositiveOffset:       0,
			PositiveBucketCounts: []uint64{1, 2, 3},
		}),
		histSeries("http_server_duration_exp_hist", map[string]string{"service": "web"}, 60, property.NativeHistogram{
			Count:                10,
			Scale:                0,
			PositiveOffset:       0,
			PositiveBucketCounts: []uint64{4, 4, 2},
		}),
	)
	o := eval(d, `histogram_quantile(0.95, http_server_duration_exp_hist{service=~".+"})`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"service": "api"}, 7.464263932294459),
		row(map[string]string{"service": "web"}, 6.727171322029716),
	})
}

func TestNativeHistogramQuantile_NegativeBuckets(t *testing.T) {
	// No repo fixture exercises a bare-selector negative-bucket
	// quantile; this seed is hand-derived directly from the documented
	// bucket-walk algorithm (see histogram_native.go's doc comment),
	// not ported from a txtar ground truth.
	d := build(histSeries("latency_exp_hist", map[string]string{"service": "api"}, 60, property.NativeHistogram{
		Count:                6,
		Scale:                0,
		NegativeOffset:       0,
		NegativeBucketCounts: []uint64{1, 2, 3},
	}))
	o := eval(d, `histogram_quantile(0.5, latency_exp_hist)`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"service": "api"}, -4.0),
	})
}

// The three tests below pin the phi == 0 / phi == 1 saturation edges
// against EMPTY outer buckets. A bucket array's offsets describe only
// its leading gap, so a zero can sit anywhere in the array — including
// at the end. Reference Prometheus's rank walk skips zero-count
// buckets (promql/quantile.go's HistogramQuantile:
// `if bucket.Count == 0 { continue }`), so both edges must saturate to
// the lowest / highest POPULATED bucket, never to the end of the array.
//
// Answering from the array end reports the edge of an interval no
// observation fell in. TestPromQL_Property_NativeHistogram found this
// against real cerberus, which walks to a populated bucket via
// arrayFirstIndex / arrayLastIndex over a non-zero count
// (internal/chsql/histogram_quantile_native.go's phiLow / phiHigh).
// Expected values below are derived by hand from the bucket geometry,
// not read back from either implementation.

func TestNativeHistogramQuantile_Phi0SkipsEmptyNegativeBucket(t *testing.T) {
	// scale 2 → base = 2^(2^-2) = 2^0.25. Negative buckets [3, 0] at
	// offset 0: bucket 0 (spanning magnitudes (base^0, base^1]) holds
	// 3 observations, bucket 1 is EMPTY. Walking from the most negative
	// end inward, the first populated bucket is bucket 0, whose lower
	// edge is -base^1 = -2^0.25.
	//
	// Saturating to the array end instead would answer -base^2 = -2^0.5
	// — a magnitude larger than anything the histogram observed.
	d := build(histSeries("latency_exp_hist", map[string]string{"service": "api"}, 60, property.NativeHistogram{
		Count:                4,
		Scale:                2,
		ZeroCount:            1,
		NegativeOffset:       0,
		NegativeBucketCounts: []uint64{3, 0},
	}))
	o := eval(d, `histogram_quantile(0, latency_exp_hist)`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"service": "api"}, -math.Pow(2, 0.25)),
	})
}

func TestNativeHistogramQuantile_Phi1SkipsEmptyPositiveBucket(t *testing.T) {
	// scale 0 → base 2. Positive buckets [2, 0] at offset 0: bucket 0
	// (spanning (1, 2]) holds 2 observations, bucket 1 is EMPTY. The
	// highest populated bucket is bucket 0, upper edge 2^1 = 2.
	//
	// Saturating to the array end would answer 2^2 = 4.
	d := build(histSeries("latency_exp_hist", map[string]string{"service": "api"}, 60, property.NativeHistogram{
		Count:                2,
		Scale:                0,
		PositiveOffset:       0,
		PositiveBucketCounts: []uint64{2, 0},
	}))
	o := eval(d, `histogram_quantile(1, latency_exp_hist)`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"service": "api"}, 2),
	})
}

func TestNativeHistogramQuantile_Phi0PositiveOnlyStartsAtLowestPopulated(t *testing.T) {
	// scale 0 → base 2. Positive buckets [0, 5] at offset 0: bucket 0
	// is EMPTY, bucket 1 (spanning (2, 4]) holds all 5 observations.
	// With no negative buckets and an empty zero bucket, the lowest
	// populated bucket is positive bucket 1, lower edge 2^1 = 2.
	//
	// A histogram whose observations all sit above 2 must not answer 0
	// for its minimum.
	d := build(histSeries("latency_exp_hist", map[string]string{"service": "api"}, 60, property.NativeHistogram{
		Count:                5,
		Scale:                0,
		PositiveOffset:       0,
		PositiveBucketCounts: []uint64{0, 5},
	}))
	o := eval(d, `histogram_quantile(0, latency_exp_hist)`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"service": "api"}, 2),
	})
}

func TestNativeHistogramValue_CountSumAvg(t *testing.T) {
	d := build(histSeries("latency_exp_hist", map[string]string{"service": "api"}, 60, property.NativeHistogram{
		Count: 13,
		Sum:   30,
		Scale: 0,
	}))
	assertRows(t, eval(d, `histogram_count(latency_exp_hist)`, 90),
		[]property.OutcomeRow{row(map[string]string{"service": "api"}, 13)})
	assertRows(t, eval(d, `histogram_sum(latency_exp_hist)`, 90),
		[]property.OutcomeRow{row(map[string]string{"service": "api"}, 30)})
	assertRows(t, eval(d, `histogram_avg(latency_exp_hist)`, 90),
		[]property.OutcomeRow{row(map[string]string{"service": "api"}, 30.0/13.0)})
}

func TestNativeHistogramValue_StddevStdvar(t *testing.T) {
	d := build(histSeries("latency_exp_hist", map[string]string{"service": "api"}, 60, property.NativeHistogram{
		Count:                13,
		Sum:                  30,
		Scale:                0,
		ZeroCount:            1,
		PositiveOffset:       0,
		PositiveBucketCounts: []uint64{3, 5, 2},
		NegativeOffset:       1,
		NegativeBucketCounts: []uint64{2},
	}))
	assertRows(t, eval(d, `histogram_stddev(latency_exp_hist)`, 90),
		[]property.OutcomeRow{row(map[string]string{"service": "api"}, 2.546028542558652)})
	assertRows(t, eval(d, `histogram_stdvar(latency_exp_hist)`, 90),
		[]property.OutcomeRow{row(map[string]string{"service": "api"}, 6.482261339523332)})
}

func TestNativeHistogramFraction(t *testing.T) {
	d := build(histSeries("latency_exp_hist", map[string]string{"service": "api"}, 60, property.NativeHistogram{
		Count:                13,
		Sum:                  30,
		Scale:                0,
		ZeroCount:            1,
		PositiveOffset:       0,
		PositiveBucketCounts: []uint64{3, 5, 2},
		NegativeOffset:       1,
		NegativeBucketCounts: []uint64{2},
	}))
	o := eval(d, `histogram_fraction(0.5, 3, latency_exp_hist)`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"service": "api"}, 0.45575480796967544),
	})
}

func TestNativeHistogramFraction_NegativeBounds(t *testing.T) {
	d := build(histSeries("latency_exp_hist", map[string]string{"service": "api"}, 60, property.NativeHistogram{
		Count:                13,
		Sum:                  30,
		Scale:                0,
		ZeroCount:            1,
		PositiveOffset:       0,
		PositiveBucketCounts: []uint64{3, 5, 2},
		NegativeOffset:       1,
		NegativeBucketCounts: []uint64{2},
	}))
	o := eval(d, `histogram_fraction(-3, 3, latency_exp_hist)`, 90)
	assertRows(t, o, []property.OutcomeRow{
		row(map[string]string{"service": "api"}, 0.6226721157729302),
	})
}

func TestNativeHistogramValue_SkipsFloatSamples(t *testing.T) {
	d := build(makeSeries("cpu_seconds", map[string]string{"job": "api"}, sampleSpec{60, 42}))
	o := eval(d, `histogram_count(cpu_seconds)`, 90)
	assertRows(t, o, nil)
}

func TestMergeNativeHistogramsCoarsestScaleAndAlignedOffsets(t *testing.T) {
	merged := mergeNativeHistograms([]*nativeHistogram{
		{
			Count: 10, Sum: 12, Scale: 1, ZeroCount: 1,
			PositiveOffset: -1, PositiveBucketCounts: []float64{1, 2, 3, 4},
			NegativeOffset: 0, NegativeBucketCounts: []float64{2, 4},
		},
		{
			Count: 20, Sum: 30, Scale: 0, ZeroCount: 2,
			PositiveOffset: 0, PositiveBucketCounts: []float64{10, 20},
			NegativeOffset: -1, NegativeBucketCounts: []float64{3, 5},
		},
	})

	if merged.Scale != 0 || merged.Count != 30 || merged.Sum != 42 || merged.ZeroCount != 3 {
		t.Fatalf("merged scalars = %+v", merged)
	}
	assertBucketLadder(t, "positive", merged.PositiveOffset, merged.PositiveBucketCounts, -1, []float64{1, 15, 24})
	assertBucketLadder(t, "negative", merged.NegativeOffset, merged.NegativeBucketCounts, -1, []float64{3, 11})
}

func TestNativeHistogramAggregationRetainsMergedHistogram(t *testing.T) {
	d := build(
		histSeries("latency_exp_hist", map[string]string{"job": "api", "instance": "a"}, 60, property.NativeHistogram{
			Count: 10, Sum: 12, Scale: 1, PositiveOffset: 0, PositiveBucketCounts: []uint64{1, 2, 3, 4},
		}),
		histSeries("latency_exp_hist", map[string]string{"job": "api", "instance": "b"}, 60, property.NativeHistogram{
			Count: 15, Sum: 18, Scale: 0, PositiveOffset: 0, PositiveBucketCounts: []uint64{5, 10},
		}),
	)

	o := eval(d, `sum by (job) (latency_exp_hist)`, 90)
	h := onlyHistogram(t, o)
	if o.Rows[0].Labels["job"] != "api" || len(o.Rows[0].Labels) != 1 {
		t.Errorf("labels = %v, want only job=api", o.Rows[0].Labels)
	}
	if h.Count != 25 || h.Sum != 30 || len(h.Buckets) != 2 {
		t.Fatalf("merged wire histogram = %+v", h)
	}
	if h.Buckets[0].Count != 8 || h.Buckets[1].Count != 17 {
		t.Errorf("merged bucket counts = [%g, %g], want [8, 17]", h.Buckets[0].Count, h.Buckets[1].Count)
	}
}

func TestNativeHistogramIncreaseUsesWholeHistogramReset(t *testing.T) {
	d := build(property.SeriesData{
		MetricName: "latency_exp_hist",
		Labels:     map[string]string{"service": "api"},
		Points: []property.Point{
			{TimestampMs: ts(0), Histogram: &property.NativeHistogram{Count: 10, Sum: 5, Scale: 0, PositiveBucketCounts: []uint64{8, 2}}},
			{TimestampMs: ts(60), Histogram: &property.NativeHistogram{Count: 20, Sum: 10, Scale: 0, PositiveBucketCounts: []uint64{16, 4}}},
			{TimestampMs: ts(120), Histogram: &property.NativeHistogram{Count: 12, Sum: 6, Scale: 0, PositiveBucketCounts: []uint64{5, 7}}},
		},
	})

	h := onlyHistogram(t, eval(d, `increase(latency_exp_hist[5m])`, 120))
	if !valuesClose(h.Count, 27.5) || !valuesClose(h.Sum, 13.75) {
		t.Fatalf("increase count/sum = %g/%g, want 27.5/13.75", h.Count, h.Sum)
	}
	if len(h.Buckets) != 2 || !valuesClose(h.Buckets[0].Count, 16.25) || !valuesClose(h.Buckets[1].Count, 11.25) {
		t.Fatalf("increase buckets = %+v, want [16.25, 11.25]", h.Buckets)
	}

	rate := onlyHistogram(t, eval(d, `rate(latency_exp_hist[5m])`, 120))
	if !valuesClose(rate.Count, 27.5/300) || !valuesClose(rate.Buckets[1].Count, 11.25/300) {
		t.Fatalf("rate histogram = %+v", rate)
	}
}

func TestNativeHistogramQuantileOverMergedRate(t *testing.T) {
	d := build(
		property.SeriesData{
			MetricName: "latency_exp_hist", Labels: map[string]string{"job": "api", "instance": "a"},
			Points: []property.Point{
				{TimestampMs: ts(0), Histogram: &property.NativeHistogram{Count: 2, Sum: 3, Scale: 1, PositiveBucketCounts: []uint64{1, 1}}},
				{TimestampMs: ts(60), Histogram: &property.NativeHistogram{Count: 6, Sum: 9, Scale: 1, PositiveBucketCounts: []uint64{3, 3}}},
			},
		},
		property.SeriesData{
			MetricName: "latency_exp_hist", Labels: map[string]string{"job": "api", "instance": "b"},
			Points: []property.Point{
				{TimestampMs: ts(0), Histogram: &property.NativeHistogram{Count: 2, Sum: 4, Scale: 0, PositiveBucketCounts: []uint64{2}}},
				{TimestampMs: ts(60), Histogram: &property.NativeHistogram{Count: 4, Sum: 8, Scale: 0, PositiveBucketCounts: []uint64{4}}},
			},
		},
	)

	o := eval(d, `histogram_quantile(0.5, sum by (job) (rate(latency_exp_hist[5m])))`, 60)
	if o.Err != nil {
		t.Fatalf("unexpected oracle error: %v", o.Err)
	}
	if len(o.Rows) != 1 || o.Rows[0].Histogram != nil || math.IsNaN(o.Rows[0].Value) {
		t.Fatalf("quantile outcome = %+v", o.Rows)
	}
}

func TestNativeHistogramRateResetWithShiftedOffsets(t *testing.T) {
	samples := []Sample{
		{T: ts(0), H: nativeHistogramFromStored(&property.NativeHistogram{
			Count: 17, Sum: -72, Scale: 0, ZeroCount: 3,
			PositiveOffset: -2, PositiveBucketCounts: []uint64{5, 3, 1},
			NegativeOffset: 3, NegativeBucketCounts: []uint64{2, 3},
		})},
		{T: ts(15), H: nativeHistogramFromStored(&property.NativeHistogram{
			Count: 22, Sum: 244, Scale: 0, ZeroCount: 3,
			PositiveOffset: 3, PositiveBucketCounts: []uint64{5, 3, 3},
			NegativeOffset: 0, NegativeBucketCounts: []uint64{5, 3},
		})},
	}

	h, histogramInput, ok := extrapolatedHistogramValue(samples, int64((5*time.Minute)/time.Millisecond), ts(200), true, true)
	if !histogramInput || !ok {
		t.Fatalf("extrapolatedHistogramValue = input %v ok %v", histogramInput, ok)
	}
	if got := nativeHistogramQuantileValue(0.5, h); !valuesClose(got, 8) {
		t.Fatalf("quantile = %g, want 8; histogram=%+v", got, h)
	}
}

func TestNativeHistogramRangeFunctionsDropMixedFloatHistogramSamples(t *testing.T) {
	t.Parallel()

	d := build(property.SeriesData{
		MetricName: "mixed_exp_hist",
		Labels:     map[string]string{"service": "api"},
		Points: []property.Point{
			{TimestampMs: ts(0), Histogram: &property.NativeHistogram{
				Count: 2, Sum: 3, Scale: 0, PositiveBucketCounts: []uint64{2},
			}},
			{TimestampMs: ts(60), Value: 7},
		},
	})

	for _, fn := range []string{"delta", "irate", "idelta"} {
		fn := fn
		t.Run(fn, func(t *testing.T) {
			t.Parallel()
			o := eval(d, fn+`(mixed_exp_hist[5m])`, 60)
			if o.Err != nil {
				t.Fatalf("%s mixed input: unexpected oracle error: %v", fn, o.Err)
			}
			if len(o.Rows) != 0 {
				t.Fatalf("%s mixed input returned %+v, want the series dropped", fn, o.Rows)
			}
		})
	}
}

func assertBucketLadder(t *testing.T, name string, gotOffset int32, got []float64, wantOffset int32, want []float64) {
	t.Helper()
	if gotOffset != wantOffset || len(got) != len(want) {
		t.Fatalf("%s ladder = offset %d %v, want offset %d %v", name, gotOffset, got, wantOffset, want)
	}
	for i := range want {
		if !valuesClose(got[i], want[i]) {
			t.Errorf("%s bucket[%d] = %g, want %g", name, i, got[i], want[i])
		}
	}
}

func onlyHistogram(t *testing.T, o property.Outcome) *property.Histogram {
	t.Helper()
	if o.Err != nil {
		t.Fatalf("unexpected oracle error: %v", o.Err)
	}
	if len(o.Rows) != 1 || o.Rows[0].Histogram == nil {
		t.Fatalf("outcome rows = %+v, want one histogram", o.Rows)
	}
	return o.Rows[0].Histogram
}

package promql

import (
	"math"
	"testing"

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

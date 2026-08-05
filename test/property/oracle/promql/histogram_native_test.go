package promql

import (
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

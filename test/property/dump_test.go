package property

import (
	"strings"
	"testing"
)

// TestDumpDatasetNativeHistogram pins the failure-log rendering of a
// native-histogram dataset.
//
// This is the only channel through which a shrunk property-test
// counterexample reaches a human: a histogram point's entire payload is
// its bucket layout, so a dump that printed just "points=1" would leave
// a reader unable to re-derive the expected answer by hand. The field
// order is asserted because the doc comment on dumpNativeHistogram
// promises it matches the NativeHistogram declaration.
func TestDumpDatasetNativeHistogram(t *testing.T) {
	d := Dataset{Metrics: &MetricsModel{Series: []SeriesData{{
		MetricName: "latency_exp_hist",
		Labels:     map[string]string{"job": "api"},
		Points: []Point{{
			TimestampMs: 1778673600000,
			Histogram: &NativeHistogram{
				Count:                6,
				Sum:                  1.5,
				Scale:                -1,
				ZeroCount:            2,
				PositiveOffset:       3,
				PositiveBucketCounts: []uint64{1, 2},
				NegativeOffset:       -1,
				NegativeBucketCounts: nil,
			},
		}},
	}}}}

	got := dumpDataset(d)

	const wantPayload = "ts=1778673600000 count=6 sum=1.5 scale=-1 zeroCount=2 pos=+3[1 2] neg=-1[]"
	if !strings.Contains(got, wantPayload) {
		t.Errorf("dumpDataset did not render the histogram payload.\n got %q\nwant a line containing %q", got, wantPayload)
	}
	if !strings.Contains(got, `latency_exp_hist{job="api"} points=1`) {
		t.Errorf("dumpDataset lost the series header:\n%s", got)
	}
}

// TestDumpDatasetFloatPointsStaySummarised guards the other direction:
// gauge datasets are summarised by their point COUNT, and adding a
// per-point line for them would bury a 5-series × 10-point failure log
// in fifty lines of noise.
func TestDumpDatasetFloatPointsStaySummarised(t *testing.T) {
	d := Dataset{Metrics: &MetricsModel{Series: []SeriesData{{
		MetricName: "up",
		Labels:     map[string]string{"job": "api"},
		Points: []Point{
			{TimestampMs: 1778673600000, Value: 1},
			{TimestampMs: 1778673615000, Value: 0},
		},
	}}}}

	got := dumpDataset(d)

	if want := `up{job="api"} points=2`; !strings.Contains(got, want) {
		t.Errorf("dumpDataset = %q, want a line containing %q", got, want)
	}
	// "count=" appears only on a rendered histogram payload; matching
	// on "ts=" would be satisfied by the "points=" in the header.
	if strings.Contains(got, "count=") {
		t.Errorf("dumpDataset expanded float points into per-point lines:\n%s", got)
	}
}

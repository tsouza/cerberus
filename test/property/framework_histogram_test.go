package property

import (
	"strings"
	"testing"
)

func TestCompareOutcomesHistogramDoesNotFallThroughToFloatZero(t *testing.T) {
	want := Outcome{Rows: []OutcomeRow{{
		Labels: map[string]string{"job": "api"},
		Histogram: &Histogram{
			Count: 2,
			Sum:   3,
			Buckets: []HistogramBucket{{
				Boundaries: 0,
				Lower:      1,
				Upper:      2,
				Count:      2,
			}},
		},
	}}}
	got := Outcome{Rows: []OutcomeRow{{Labels: map[string]string{"job": "api"}}}}

	diff := CompareOutcomes(want, got)
	if !strings.Contains(diff, "histogram[0]") || !strings.Contains(diff, "shape mismatch") {
		t.Fatalf("CompareOutcomes diff = %q, want histogram shape mismatch", diff)
	}
}

func TestCompareOutcomesHistogramComparesBuckets(t *testing.T) {
	want := &Histogram{Count: 2, Sum: 3, Buckets: []HistogramBucket{{Boundaries: 0, Lower: 1, Upper: 2, Count: 2}}}
	got := &Histogram{Count: 2, Sum: 3, Buckets: []HistogramBucket{{Boundaries: 0, Lower: 1, Upper: 4, Count: 2}}}

	diff := CompareOutcomes(
		Outcome{Rows: []OutcomeRow{{Histogram: want}}},
		Outcome{Rows: []OutcomeRow{{Histogram: got}}},
	)
	if !strings.Contains(diff, "bucket[0]") {
		t.Fatalf("CompareOutcomes diff = %q, want bucket mismatch", diff)
	}
}

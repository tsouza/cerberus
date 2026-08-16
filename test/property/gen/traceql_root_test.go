package gen

import "testing"

func TestTraceQLDatasetUsesEmptyParentForRoots(t *testing.T) {
	dataset := TraceQLDataset().Example(0)
	if dataset.Metrics == nil || len(dataset.Metrics.Series) == 0 {
		t.Fatal("TraceQL dataset has no generated spans")
	}

	rootCountByTrace := map[string]int{}
	spanCountByTrace := map[string]int{}
	for _, span := range dataset.Metrics.Series {
		traceID := span.Labels["__traceID__"]
		spanCountByTrace[traceID]++
		if span.Labels["__parentSpanID__"] == "" {
			rootCountByTrace[traceID]++
		}
	}
	for traceID, spanCount := range spanCountByTrace {
		if got := rootCountByTrace[traceID]; got != 1 {
			t.Errorf("trace %q with %d spans has %d empty-parent roots, want 1", traceID, spanCount, got)
		}
	}
}

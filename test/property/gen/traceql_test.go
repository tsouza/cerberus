package gen

import (
	"testing"

	"pgregory.net/rapid"
)

// TestTraceQLDatasetParentChainsAreRooted pins the structural precondition
// shared by Tempo and cerberus: every generated trace has exactly one root
// whose ParentSpanId is empty, and every other parent reference resolves to a
// span in the same trace. An all-zero ParentSpanId is not a root marker in the
// OTel ClickHouse schema; it names a missing parent and makes the whole chain
// unreachable from structural-query rootedness walks.
func TestTraceQLDatasetParentChainsAreRooted(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		dataset := TraceQLDataset().Draw(rt, "dataset")
		if dataset.Metrics == nil || len(dataset.Metrics.Series) == 0 {
			rt.Fatal("dataset drew no spans")
		}

		spanIDsByTrace := make(map[string]map[string]struct{})
		for _, span := range dataset.Metrics.Series {
			traceID := span.Labels["__traceID__"]
			spanID := span.Labels["__spanID__"]
			if traceID == "" || spanID == "" {
				rt.Fatalf("generated span has empty identity: trace=%q span=%q", traceID, spanID)
			}
			if spanIDsByTrace[traceID] == nil {
				spanIDsByTrace[traceID] = make(map[string]struct{})
			}
			spanIDsByTrace[traceID][spanID] = struct{}{}
		}

		rootCounts := make(map[string]int)
		for _, span := range dataset.Metrics.Series {
			traceID := span.Labels["__traceID__"]
			parentID := span.Labels["__parentSpanID__"]
			if parentID == "" {
				rootCounts[traceID]++
				continue
			}
			if _, ok := spanIDsByTrace[traceID][parentID]; !ok {
				rt.Fatalf("trace %q span %q references missing parent %q",
					traceID, span.Labels["__spanID__"], parentID)
			}
		}

		for traceID := range spanIDsByTrace {
			if roots := rootCounts[traceID]; roots != 1 {
				rt.Fatalf("trace %q has %d roots, want exactly one empty-ParentSpanId root", traceID, roots)
			}
		}
	})
}

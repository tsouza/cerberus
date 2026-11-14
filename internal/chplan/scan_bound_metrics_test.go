package chplan

import (
	"errors"
	"testing"
)

// metricsOver wraps inner in each metrics node the chokepoint descends.
func metricsOver(inner Node) map[string]Node {
	return map[string]Node{
		"metrics_aggregate":         &MetricsAggregate{Inner: inner},
		"metrics_histogram":         &MetricsHistogramOverTime{Inner: inner},
		"metrics_compare":           &MetricsCompare{Inner: inner},
		"metrics_compare_root_side": &MetricsCompare{Inner: spansScan(), RootLookup: inner},
	}
}

// perTraceLookup is a per-trace lookup relation over scan: one row per trace.
func perTraceLookup(scan Node) Node {
	return &Aggregate{Input: scan, GroupBy: []Expr{&ColumnRef{Name: "TraceId"}}}
}

// TestRequireSpansScansBounded_MetricsSpineIsEmitWindowed: the input spine of
// a metrics node carries no window in the IR — the matrix emitters bound its
// spans scan from the RangeWindow — so a bare spans scan there passes.
func TestRequireSpansScansBounded_MetricsSpineIsEmitWindowed(t *testing.T) {
	t.Parallel()
	for name, plan := range metricsOver(&Filter{Input: spansScan(), Predicate: &ColumnRef{Name: "x"}}) {
		if err := RequireSpansScansBounded(spansTable, plan); err != nil {
			t.Errorf("%s: a metrics node's input spine was rejected: %v", name, err)
		}
	}
}

// TestRequireSpansScansBounded_MetricsPerTraceSubqueriesMustBeBounded: under a
// metrics node, a per-trace subquery reads the spans table on its own — a
// membership subquery in an Expr slot, or a TraceLookupFilter lookup joined
// beside the input — so the emitter's window on the input spine does not
// bound it. Unbounded, each is rejected; windowed, each passes.
func TestRequireSpansScansBounded_MetricsPerTraceSubqueriesMustBeBounded(t *testing.T) {
	t.Parallel()
	shapes := map[string]func(lookupScan Node) Node{
		"membership_subquery": func(lookupScan Node) Node {
			return &Filter{
				Input:     spansScan(),
				Predicate: &InSubquery{Left: &ColumnRef{Name: "TraceId"}, Subquery: perTraceLookup(lookupScan)},
			}
		},
		"trace_lookup_filter_lookup": func(lookupScan Node) Node {
			return &TraceLookupFilter{
				Input:     spansScan(),
				Lookups:   []Node{perTraceLookup(lookupScan)},
				Predicate: &ColumnRef{Name: "x"},
			}
		},
	}
	for shape, build := range shapes {
		for name, plan := range metricsOver(build(spansScan())) {
			var v *ScanResourceBoundViolation
			if err := RequireSpansScansBounded(spansTable, plan); !errors.As(err, &v) {
				t.Errorf("%s under %s: an unbounded per-trace subquery passed (err=%v)", shape, name, err)
			}
		}
		for name, plan := range metricsOver(build(&Filter{Input: spansScan(), Predicate: windowPred()})) {
			if err := RequireSpansScansBounded(spansTable, plan); err != nil {
				t.Errorf("%s under %s: a windowed per-trace subquery was rejected: %v", shape, name, err)
			}
		}
	}
}

// TestRequireSpansScansBounded_LookupIsNotBoundedByTheOuterWindow: a
// TraceLookupFilter's lookups are joined beside its input, so a window on the
// input — or above the filter — does not bound what a lookup reads.
func TestRequireSpansScansBounded_LookupIsNotBoundedByTheOuterWindow(t *testing.T) {
	t.Parallel()
	plan := &Filter{
		Predicate: windowPred(),
		Input: &TraceLookupFilter{
			Input:     &Filter{Input: spansScan(), Predicate: windowPred()},
			Lookups:   []Node{perTraceLookup(spansScan())},
			Predicate: &ColumnRef{Name: "x"},
		},
	}
	var v *ScanResourceBoundViolation
	if err := RequireSpansScansBounded(spansTable, plan); !errors.As(err, &v) {
		t.Fatalf("an unbounded lookup under a windowed filter passed (err=%v)", err)
	}
}

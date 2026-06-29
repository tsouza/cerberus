package optimizer_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// recoverScanResourceBoundViolation runs fn and returns the
// *chplan.ScanResourceBoundViolation it panics with, or nil if it did not panic.
func recoverScanResourceBoundViolation(t *testing.T, fn func()) (v *chplan.ScanResourceBoundViolation) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		got, ok := r.(*chplan.ScanResourceBoundViolation)
		if !ok {
			t.Fatalf("expected *chplan.ScanResourceBoundViolation, got %T: %v", r, r)
		}
		v = got
	}()
	fn()
	return nil
}

func nestedSetNode(traceLimit int64, input chplan.Node) *chplan.NestedSetAnnotate {
	return &chplan.NestedSetAnnotate{
		Input:              input,
		SpansTable:         "otel_traces",
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
		TimestampColumn:    "Timestamp",
		TraceLimit:         traceLimit,
	}
}

func boundedTraceScopeLeaf(limit int64) chplan.Node {
	return &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_traces"},
		Predicate: &chplan.BoundedTraceScope{
			SpansTable:         "otel_traces",
			TraceIDColumn:      "TraceId",
			ParentSpanIDColumn: "ParentSpanId",
			TimestampColumn:    "Timestamp",
			TraceLimit:         limit,
		},
	}
}

// TestRequireScanResourceBound_PassesWithLockstepGate: a bounded
// NestedSetAnnotate (TraceLimit > 0) over an input carrying the matching
// BoundedTraceScope passes — no panic, no mutation.
func TestRequireScanResourceBound_PassesWithLockstepGate(t *testing.T) {
	t.Parallel()
	nsa := nestedSetNode(20, boundedTraceScopeLeaf(20))
	v := recoverScanResourceBoundViolation(t, func() {
		out, changed := optimizer.RequireScanResourceBound{}.Apply(nsa)
		if changed {
			t.Errorf("RequireScanResourceBound must not mutate the tree")
		}
		if out != chplan.Node(nsa) {
			t.Errorf("RequireScanResourceBound must return the node unchanged")
		}
	})
	if v != nil {
		t.Fatalf("bounded NestedSetAnnotate with a lock-step gate must pass, got violation: %v", v)
	}
}

// TestRequireScanResourceBound_PanicsWhenGateStripped: the same TraceLimit > 0
// numbering walk WITHOUT a BoundedTraceScope on its input is the broken
// lock-step — the numbering scope would be bounded while the row source is not
// — and must fail closed.
func TestRequireScanResourceBound_PanicsWhenGateStripped(t *testing.T) {
	t.Parallel()
	nsa := nestedSetNode(20, &chplan.Scan{Table: "otel_traces"})
	v := recoverScanResourceBoundViolation(t, func() {
		optimizer.RequireScanResourceBound{}.Apply(nsa)
	})
	if v == nil {
		t.Fatalf("TraceLimit>0 NestedSetAnnotate without a BoundedTraceScope leaf must panic ScanResourceBoundViolation")
	}
	if v.Table != "otel_traces" {
		t.Errorf("violation Table = %q, want otel_traces", v.Table)
	}
}

// TestRequireScanResourceBound_IgnoresUnboundedNumbering: a TraceLimit == 0
// numbering walk (single-trace / non-search traceScopeFrag superset) carries no
// such fact and must be left untouched — the rule never invents a bound.
func TestRequireScanResourceBound_IgnoresUnboundedNumbering(t *testing.T) {
	t.Parallel()
	nsa := nestedSetNode(0, &chplan.Scan{Table: "otel_traces"})
	v := recoverScanResourceBoundViolation(t, func() {
		if _, changed := (optimizer.RequireScanResourceBound{}).Apply(nsa); changed {
			t.Errorf("RequireScanResourceBound must not mutate")
		}
	})
	if v != nil {
		t.Fatalf("TraceLimit==0 numbering must be ignored, got violation: %v", v)
	}
}

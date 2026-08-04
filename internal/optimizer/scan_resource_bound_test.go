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

// traceIDInListLeaf builds a literal `TraceId IN (<n literal ids>)` leaf —
// the shape a resolve-in-Go-and-splice-literals rewrite of a
// BoundedTraceScope subquery produces (#1702).
func traceIDInListLeaf(n int) chplan.Node {
	list := make([]chplan.Expr, n)
	for i := range list {
		list[i] = &chplan.LitString{V: "id"}
	}
	return &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_traces"},
		Predicate: &chplan.InList{
			Left: &chplan.ColumnRef{Name: "TraceId"},
			List: list,
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
		return
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

// TestRequireScanResourceBound_PassesWithLiteralInListWitness: #1702 — a
// literal `TraceId IN (<n ids>)` whose length matches TraceLimit is an
// equally valid witness to a BoundedTraceScope subquery (the
// resolve-in-Go-and-splice-literals rewrite of one), matching the general
// chplan.SpansScanResourceBound chokepoint's form-b classification.
func TestRequireScanResourceBound_PassesWithLiteralInListWitness(t *testing.T) {
	t.Parallel()
	nsa := nestedSetNode(20, traceIDInListLeaf(20))
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
		t.Fatalf("bounded NestedSetAnnotate with a matching-cardinality InList must pass, got violation: %v", v)
	}
}

// TestRequireScanResourceBound_PanicsOnCardinalityMismatch: #1702 — a literal
// InList whose length does NOT match TraceLimit has drifted out of lock-step
// (it proves a different-sized trace set than the numbering walk claims) and
// must still fail closed, not be waved through merely for being an InList.
func TestRequireScanResourceBound_PanicsOnCardinalityMismatch(t *testing.T) {
	t.Parallel()
	nsa := nestedSetNode(20, traceIDInListLeaf(5))
	v := recoverScanResourceBoundViolation(t, func() {
		optimizer.RequireScanResourceBound{}.Apply(nsa)
	})
	if v == nil {
		t.Fatalf("TraceLimit=20 NestedSetAnnotate with a 5-element InList must panic ScanResourceBoundViolation")
		return
	}
	if v.Table != "otel_traces" {
		t.Errorf("violation Table = %q, want otel_traces", v.Table)
	}
}

// TestRequireScanResourceBound_PanicsOnNegatedInList: a `TraceId NOT IN (...)`
// proves the opposite of a bounded set (an exclusion, not a membership) and
// must never count as a witness regardless of its length.
func TestRequireScanResourceBound_PanicsOnNegatedInList(t *testing.T) {
	t.Parallel()
	leaf := traceIDInListLeaf(20)
	leaf.(*chplan.Filter).Predicate.(*chplan.InList).Negated = true
	nsa := nestedSetNode(20, leaf)
	v := recoverScanResourceBoundViolation(t, func() {
		optimizer.RequireScanResourceBound{}.Apply(nsa)
	})
	if v == nil {
		t.Fatalf("a negated InList must never satisfy the lock-step witness, expected panic")
	}
}

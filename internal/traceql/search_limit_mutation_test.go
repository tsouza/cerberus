package traceql

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Mutation-coverage tests for search_limit.go context plumbing and the
// trace-limit pushdown guard.

// TestWithSearchWindowStoresPartialWindow pins that a window with only one
// endpoint set is still stored on the context. The guard
// `start.IsZero() && end.IsZero()` returns the context untouched only when BOTH
// bounds are zero; turning the `&&` into `||` would discard a half-open window.
func TestWithSearchWindowStoresPartialWindow(t *testing.T) {
	t.Parallel()
	start := time.Unix(1782571392, 0).UTC()

	// Only start set — must be stored.
	gotStart, gotEnd := searchWindowFromCtx(WithSearchWindow(context.Background(), start, time.Time{}))
	if !gotStart.Equal(start) || !gotEnd.IsZero() {
		t.Errorf("partial (start-only) window = (%v, %v); want (%v, zero)", gotStart, gotEnd, start)
	}

	// Only end set — must be stored.
	end := time.Unix(1782573192, 0).UTC()
	gotStart, gotEnd = searchWindowFromCtx(WithSearchWindow(context.Background(), time.Time{}, end))
	if !gotStart.IsZero() || !gotEnd.Equal(end) {
		t.Errorf("partial (end-only) window = (%v, %v); want (zero, %v)", gotStart, gotEnd, end)
	}

	// Both zero — nothing stored.
	gotStart, gotEnd = searchWindowFromCtx(WithSearchWindow(context.Background(), time.Time{}, time.Time{}))
	if !gotStart.IsZero() || !gotEnd.IsZero() {
		t.Errorf("empty window stored (%v, %v); want both zero", gotStart, gotEnd)
	}
}

// TestStampSearchTraceLimitWrapsPlainSource pins that a plain-search row source
// with a positive limit is wrapped in a chplan.SearchTraceLimit. The guard
// `limit <= 0 || plan == nil` is a no-op gate; negating the `plan == nil`
// clause would return the plan unwrapped, dropping the trace-limit pushdown
// (the summaries-drain OOM guard).
func TestStampSearchTraceLimitWrapsPlainSource(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	scan := &chplan.Scan{}

	const limit = 25
	got := stampSearchTraceLimit(scan, limit, time.Time{}, time.Time{}, s)
	stl, ok := got.(*chplan.SearchTraceLimit)
	if !ok {
		t.Fatalf("stampSearchTraceLimit returned %T; want *chplan.SearchTraceLimit", got)
	}
	if stl.TraceLimit != limit {
		t.Errorf("TraceLimit = %d; want %d", stl.TraceLimit, limit)
	}

	// limit <= 0 is a no-op: the plan is returned unchanged.
	if got := stampSearchTraceLimit(scan, 0, time.Time{}, time.Time{}, s); got != chplan.Node(scan) {
		t.Errorf("zero-limit stamp = %T; want the input scan unchanged", got)
	}
}

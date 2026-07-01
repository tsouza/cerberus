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

// TestWithSearchTraceLimit_ZeroStoresNothing pins the exact `n <= 0` boundary in
// WithSearchTraceLimit (search_limit.go:32). At n == 0 the `<=` guard returns the
// context UNCHANGED, so the key is never stored. The CONDITIONALS_BOUNDARY mutant
// `n < 0` would fall through at n == 0 and store the int 0 under the key.
// searchTraceLimit masks that (its own `n > 0` check reads it back as 0 either
// way), so the kill has to inspect the raw context value directly.
func TestWithSearchTraceLimit_ZeroStoresNothing(t *testing.T) {
	t.Parallel()
	// n == 0: nothing stored under the key (mutant `n < 0` would store int 0).
	if v := WithSearchTraceLimit(context.Background(), 0).Value(searchTraceLimitKey{}); v != nil {
		t.Errorf("WithSearchTraceLimit(ctx, 0) stored %v under the key; want nothing (n <= 0 must be a no-op)", v)
	}
	// A positive limit must still be stored (guards against the guard inverting
	// wholesale rather than merely shifting the boundary).
	const positiveLimit = 7
	if v := WithSearchTraceLimit(context.Background(), positiveLimit).Value(searchTraceLimitKey{}); v != positiveLimit {
		t.Errorf("WithSearchTraceLimit(ctx, %d) stored %v; want %d", positiveLimit, v, positiveLimit)
	}
}

// TestSearchTraceLimit_NegativeStoredValueGuarded pins the `ok && n > 0`
// conjunction in searchTraceLimit (search_limit.go:41). A negative int stored
// directly under the key (bypassing WithSearchTraceLimit's own guard) must read
// back as 0: `ok` is true but `n > 0` is false, so the AND is false. The
// INVERT_LOGICAL mutant `ok || n > 0` would be true (ok alone) and return the
// negative value verbatim.
func TestSearchTraceLimit_NegativeStoredValueGuarded(t *testing.T) {
	t.Parallel()
	const storedNegative = -5
	ctx := context.WithValue(context.Background(), searchTraceLimitKey{}, storedNegative)
	if got := searchTraceLimit(ctx); got != 0 {
		t.Errorf("searchTraceLimit with a stored %d = %d; want 0 (ok AND n>0 must reject a negative)", storedNegative, got)
	}
	// The exported accessor must agree — same guard, same 0.
	if got := SearchTraceLimit(ctx); got != 0 {
		t.Errorf("SearchTraceLimit with a stored %d = %d; want 0", storedNegative, got)
	}
}

// TestSearchTraceLimit_MinimumPositiveRoundTrips pins the exact `n >= 1` boundary
// in searchTraceLimit (search_limit.go:41). A limit of 1 — the smallest positive
// value — must read back as 1, not fall through to 0. The CONDITIONALS_BOUNDARY
// mutant `n > 1` would reject the boundary value and return 0.
func TestSearchTraceLimit_MinimumPositiveRoundTrips(t *testing.T) {
	t.Parallel()
	const minPositiveLimit = 1
	ctx := WithSearchTraceLimit(context.Background(), minPositiveLimit)
	if got := searchTraceLimit(ctx); got != minPositiveLimit {
		t.Errorf("searchTraceLimit with a stored %d = %d; want %d (n>=1 must accept the boundary)", minPositiveLimit, got, minPositiveLimit)
	}
}

// TestStampRecursiveScanWindow_EndOnlyStillStamps pins the
// `startNano == 0 && endNano == 0` guard in stampRecursiveScanWindow
// (search_limit.go:181). An end-only window (start zero, end set) has exactly one
// operand true, so the AND is false and the walk proceeds to stamp the window
// onto the StructuralJoin. The INVERT_LOGICAL mutant `||` would be true and
// short-circuit the stamp, leaving the recursive step scan windowless (full-
// retention scan — the GAP-3 OOM).
func TestStampRecursiveScanWindow_EndOnlyStillStamps(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	end := time.Unix(1782573192, 0).UTC()
	sj := &chplan.StructuralJoin{Left: &chplan.Scan{}, Right: &chplan.Scan{}}

	got := stampRecursiveScanWindow(sj, time.Time{}, end, s).(*chplan.StructuralJoin)
	if got.WindowEndNano != end.UnixNano() {
		t.Errorf("WindowEndNano = %d; want %d (end-only window must still be stamped)", got.WindowEndNano, end.UnixNano())
	}
	if got.TimestampColumn != s.TimestampColumn {
		t.Errorf("TimestampColumn = %q; want %q (stamp must arm the step-scan window)", got.TimestampColumn, s.TimestampColumn)
	}
	// A fully-zero window is a genuine no-op: nothing stamped.
	unstamped := &chplan.StructuralJoin{Left: &chplan.Scan{}, Right: &chplan.Scan{}}
	if out := stampRecursiveScanWindow(unstamped, time.Time{}, time.Time{}, s).(*chplan.StructuralJoin); out.WindowEndNano != 0 || out.TimestampColumn != "" {
		t.Errorf("zero window stamped WindowEndNano=%d TimestampColumn=%q; want unchanged", out.WindowEndNano, out.TimestampColumn)
	}
}

// parentSpanIDColForTest is the ParentSpanId column name the isRootSpanFilter
// tests build their root-span predicates against.
const parentSpanIDColForTest = "ParentSpanId"

// rootFilterOverScan builds a Filter(Scan) whose predicate is
// `<left> <op> <right>` — the shape isRootSpanFilter inspects.
func rootFilterOverScan(op chplan.BinaryOp, left, right chplan.Expr) *chplan.Filter {
	return &chplan.Filter{
		Input:     &chplan.Scan{},
		Predicate: &chplan.Binary{Op: op, Left: left, Right: right},
	}
}

// TestIsRootSpanFilter_NonEqOpRejected pins the `!ok || b.Op != OpEq` disjunction
// in isRootSpanFilter (search_limit.go:399). The predicate here IS a *Binary
// (`!ok` false) but its op is `<`, not `=` (`b.Op != OpEq` true), so the OR is
// true and the shape is rejected. The INVERT_LOGICAL mutant `&&` would be false
// (one operand false) and fall through to accept a NON-equality predicate as a
// root-span filter — bounding a shape whose root membership is not guaranteed.
func TestIsRootSpanFilter_NonEqOpRejected(t *testing.T) {
	t.Parallel()
	f := rootFilterOverScan(chplan.OpLt,
		&chplan.ColumnRef{Name: parentSpanIDColForTest},
		&chplan.LitString{V: ""})
	if isRootSpanFilter(f, parentSpanIDColForTest) {
		t.Errorf("isRootSpanFilter accepted a `%s < \"\"` predicate; want rejected (only OpEq is a root filter)", parentSpanIDColForTest)
	}
	// Sanity: the same predicate with OpEq IS a root filter — proves the test
	// isolates the op, not some other rejection.
	eq := rootFilterOverScan(chplan.OpEq,
		&chplan.ColumnRef{Name: parentSpanIDColForTest},
		&chplan.LitString{V: ""})
	if !isRootSpanFilter(eq, parentSpanIDColForTest) {
		t.Errorf("isRootSpanFilter rejected the canonical `%s = \"\"` root filter", parentSpanIDColForTest)
	}
}

// TestIsRootSpanFilter_WrongColumnRejected pins the FIRST `&&` on
// search_limit.go:407 (`ok && col.Name == parentSpanIDCol`). The predicate is a
// well-formed `<col> = ""` over the WRONG column: `ok` true, the column check
// false, the empty-string check true. The AND chain is false (rejected). The
// INVERT_LOGICAL mutant `ok || col.Name == parentSpanIDCol && lit.V == ""`
// short-circuits on `ok` alone and would ACCEPT a non-ParentSpanId column as a
// root filter.
func TestIsRootSpanFilter_WrongColumnRejected(t *testing.T) {
	t.Parallel()
	f := rootFilterOverScan(chplan.OpEq,
		&chplan.ColumnRef{Name: "SpanName"}, // not the parent-span column
		&chplan.LitString{V: ""})
	if isRootSpanFilter(f, parentSpanIDColForTest) {
		t.Errorf("isRootSpanFilter accepted a `SpanName = \"\"` predicate; want rejected (column must be %s)", parentSpanIDColForTest)
	}
}

// TestIsRootSpanFilter_NonEmptyLiteralRejected pins the SECOND `&&` on
// search_limit.go:407 (`... && lit.V == ""`). The predicate is `<parentCol> =
// "srv"`: `ok` true, column matches, but the literal is NON-empty. The AND chain
// is false (rejected). The INVERT_LOGICAL mutant `(ok && col.Name == …) ||
// lit.V == ""` is true on the left conjunction alone and would ACCEPT a
// `ParentSpanId = "srv"` predicate — a child-span filter, not a root filter.
func TestIsRootSpanFilter_NonEmptyLiteralRejected(t *testing.T) {
	t.Parallel()
	f := rootFilterOverScan(chplan.OpEq,
		&chplan.ColumnRef{Name: parentSpanIDColForTest},
		&chplan.LitString{V: "srv"}) // non-empty ⇒ not a root filter
	if isRootSpanFilter(f, parentSpanIDColForTest) {
		t.Errorf("isRootSpanFilter accepted a `%s = \"srv\"` predicate; want rejected (root filter needs the empty literal)", parentSpanIDColForTest)
	}
}

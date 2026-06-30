package chsql_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	tql "github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"
)

// Phase-4 emitter coverage for the spans-scan WINDOW invariant. These tests
// mirror the requireInstantScanBound fail-closed tests: an emitter-synthetic
// recursive / grouped spans scan that opted into request-window partition
// pruning (TimestampColumn stamped by the search lowering) but reaches emit
// with a ZERO window must be rejected (ErrUnboundedSpansScan) rather than
// silently rendering a full-retention scan behind the inert `TraceId IN (seed)`
// membership. The positive cases assert the recursive arm / root-lookup scan
// carries a direct `fromUnixTimestamp64Nano(...)` Timestamp predicate when the
// window IS present.
//
// The recursive STEP/ANCHOR arms render the window as an INLINE literal
// (`fromUnixTimestamp64Nano(<nanos>)`) — only the emitter-synthetic scans do
// that; every chplan-leaf scan renders its window as a positional `?` arg. So
// `fromUnixTimestamp64Nano(<startNano>)` appearing in the SQL is a precise
// witness that the recursive arm itself is windowed (not just the seed leaf).

const (
	scanWindowEmitLimit         = 20
	scanWindowEmitStartNano     = int64(1782571392_000000000)
	scanWindowEmitEndNano       = int64(1782573192_000000000)
	scanWindowEmitStep          = 5 * time.Minute
	fromUnixNanoCall            = "fromUnixTimestamp64Nano"
	structuralRecursiveStepFrom = "`otel_traces` AS t INNER JOIN"
)

func scanWindowEmitBounds() (time.Time, time.Time) {
	return time.Unix(0, scanWindowEmitStartNano).UTC(), time.Unix(0, scanWindowEmitEndNano).UTC()
}

// lowerWindowedSearch lowers a TraceQL search query through the real lowering
// with the /api/search limit + request window threaded on the context, exactly
// as the handler does.
func lowerWindowedSearch(t *testing.T, q string, start, end time.Time) chplan.Node {
	t.Helper()
	expr, err := ast.Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	ctx := tql.WithSearchTraceLimit(context.Background(), scanWindowEmitLimit)
	ctx = tql.WithSearchWindow(ctx, start, end)
	plan, err := tql.Lower(ctx, expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("lower %q: %v", q, err)
	}
	return plan
}

// emitSpansScoped emits plan with the spans table threaded onto the emit
// context (chsql.WithSpansTable), the genuine "production Tempo request"
// signal that arms the fail-closed window guard.
func emitSpansScoped(t *testing.T, plan chplan.Node) (string, error) {
	t.Helper()
	s := schema.DefaultOTelTraces()
	sql, _, err := chsql.Emit(chsql.WithSpansTable(context.Background(), s.SpansTable), plan)
	return sql, err
}

// zeroStructuralWindows clears the request window on every StructuralJoin while
// LEAVING TimestampColumn set, simulating a node that opted into windowing but
// reaches emit with a zero window — exactly what the fail-closed guard exists
// to catch.
func zeroStructuralWindows(plan chplan.Node) {
	chplan.Walk(plan, func(n chplan.Node) bool {
		if j, ok := n.(*chplan.StructuralJoin); ok {
			j.WindowStartNano = 0
			j.WindowEndNano = 0
		}
		return true
	})
}

// zeroNestedSetWindows is the NestedSetAnnotate analogue of
// zeroStructuralWindows.
func zeroNestedSetWindows(plan chplan.Node) {
	chplan.Walk(plan, func(n chplan.Node) bool {
		if ns, ok := n.(*chplan.NestedSetAnnotate); ok {
			ns.WindowStartNano = 0
			ns.WindowEndNano = 0
		}
		return true
	})
}

// TestEmitStructuralRecursiveStepWindowed pins the structural-closure step arm:
// with a window the recursive `t` scan carries the inline Timestamp prune; with
// the window zeroed (TimestampColumn still set) the emit fails closed.
func TestEmitStructuralRecursiveStepWindowed(t *testing.T) {
	t.Parallel()
	start, end := scanWindowEmitBounds()
	const q = `{ .service.name = "a" } >> { .http.status_code = 500 }`

	plan := lowerWindowedSearch(t, q, start, end)
	sql, err := emitSpansScoped(t, plan)
	if err != nil {
		t.Fatalf("windowed structural emit: %v", err)
	}
	if !strings.Contains(sql, structuralRecursiveStepFrom) {
		t.Fatalf("emitted SQL has no recursive `otel_traces AS t` step — shape changed, assertion vacuous:\n%s", sql)
	}
	lo := fmt.Sprintf("%s(%d)", fromUnixNanoCall, scanWindowEmitStartNano)
	hi := fmt.Sprintf("%s(%d)", fromUnixNanoCall, scanWindowEmitEndNano)
	if !strings.Contains(sql, lo) || !strings.Contains(sql, hi) {
		t.Errorf("recursive step `t` scan is not window-bounded (missing inline %s / %s):\n%s", lo, hi, sql)
	}

	// Fail closed: zero the window (keep TimestampColumn) → the step would read
	// full retention behind the inert TraceId-IN, so emit must reject it.
	zeroStructuralWindows(plan)
	if _, err := emitSpansScoped(t, plan); !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("zero-window structural step under WithSpansTable must fail closed, got %v", err)
	}
}

// TestEmitNestedSetAnchorStepWindowed pins the nested-set numbering CTE: both
// the anchor and the step `t` scans carry the inline Timestamp prune when a
// window is present, and the emit fails closed when it is zeroed.
func TestEmitNestedSetAnchorStepWindowed(t *testing.T) {
	t.Parallel()
	start, end := scanWindowEmitBounds()
	// The Grafana Traces Drilldown structure-tab query — a bounded
	// NestedSetAnnotate numbering walk.
	const q = `({ nestedSetParent < 0 } &>> { kind = server }) || ({ nestedSetParent < 0 }) | select(nestedSetParent, nestedSetLeft, nestedSetRight)`

	plan := lowerWindowedSearch(t, q, start, end)
	sql, err := emitSpansScoped(t, plan)
	if err != nil {
		t.Fatalf("windowed nested-set emit: %v", err)
	}
	if !strings.Contains(sql, "_cerberus_ns_paths") {
		t.Fatalf("emitted SQL has no nested-set numbering CTE — shape changed, assertion vacuous:\n%s", sql)
	}
	lo := fmt.Sprintf("%s(%d)", fromUnixNanoCall, scanWindowEmitStartNano)
	hi := fmt.Sprintf("%s(%d)", fromUnixNanoCall, scanWindowEmitEndNano)
	// The numbering anchor + step both render the inline window; at minimum the
	// recursive-arm prune must be present (only emitter-synthetic scans inline
	// the nanos — chplan leaves use `?`).
	if strings.Count(sql, lo) < 2 || strings.Count(sql, hi) < 2 {
		t.Errorf("nested-set anchor+step not both window-bounded (inline %s count=%d, %s count=%d, want >= 2 each):\n%s",
			lo, strings.Count(sql, lo), hi, strings.Count(sql, hi), sql)
	}

	// Fail closed: zero the window (keep TimestampColumn + TraceLimit) → reject.
	zeroNestedSetWindows(plan)
	if _, err := emitSpansScoped(t, plan); !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("zero-window nested-set numbering under WithSpansTable must fail closed, got %v", err)
	}
}

// emitCompareInRangeWindow lowers `{ } | compare(...)`, wraps it in the
// RangeWindow the /api/metrics/query_range handler builds, and emits — with or
// without the spans table on the context. start/end may be zero to exercise the
// fail-closed gate.
func emitCompareInRangeWindow(t *testing.T, start, end time.Time, spansScoped bool) (string, error) {
	t.Helper()
	s := schema.DefaultOTelTraces()
	expr, err := ast.Parse(`{ } | compare({ status = error })`)
	if err != nil {
		t.Fatalf("parse compare: %v", err)
	}
	plan, err := tql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("lower compare: %v", err)
	}
	rw := &chplan.RangeWindow{
		Input:           plan,
		Range:           scanWindowEmitStep,
		Step:            scanWindowEmitStep,
		Start:           start,
		End:             end,
		TimestampColumn: s.TimestampColumn,
	}
	ctx := context.Background()
	if spansScoped {
		ctx = chsql.WithSpansTable(ctx, s.SpansTable)
	}
	sql, _, err := chsql.Emit(ctx, chplan.Node(rw))
	return sql, err
}

// TestEmitCompareRootLookupWindowed pins the compare() root-lookup window push:
// under the spans scope the per-trace root aggregate gains two direct
// `fromUnixTimestamp64Nano(...)` Timestamp bounds (below its GROUP BY, where they
// can partition-prune); without the spans scope the golden/metrics lane stays
// byte-identical (no push). A zero window under the spans scope fails closed.
func TestEmitCompareRootLookupWindowed(t *testing.T) {
	t.Parallel()
	start, end := scanWindowEmitBounds()

	withScope, err := emitCompareInRangeWindow(t, start, end, true)
	if err != nil {
		t.Fatalf("windowed compare emit (spans-scoped): %v", err)
	}
	withoutScope, err := emitCompareInRangeWindow(t, start, end, false)
	if err != nil {
		t.Fatalf("windowed compare emit (unscoped): %v", err)
	}

	// The base compare window renders its bounds as positional `?` args, so the
	// only `fromUnixTimestamp64Nano` calls come from the root-lookup push: two
	// (lo + hi) under the spans scope, zero without it.
	countWith := strings.Count(withScope, fromUnixNanoCall)
	countWithout := strings.Count(withoutScope, fromUnixNanoCall)
	if countWith != countWithout+2 {
		t.Errorf("compare root-lookup window push = %d extra %s calls; want exactly 2 (lo+hi on the root scan)\n--- with scope ---\n%s",
			countWith-countWithout, fromUnixNanoCall, withScope)
	}
	if countWith < 2 {
		t.Errorf("spans-scoped compare carries no root-lookup Timestamp prune (%s count=%d):\n%s", fromUnixNanoCall, countWith, withScope)
	}

	// Fail closed: a zero request window under the spans scope must be rejected
	// rather than scanning full retention.
	if _, err := emitCompareInRangeWindow(t, time.Time{}, time.Time{}, true); !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("zero-window compare under WithSpansTable must fail closed, got %v", err)
	}
}

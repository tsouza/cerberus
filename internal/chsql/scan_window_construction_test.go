package chsql_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	tql "github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"
)

// These are the CONSTRUCTION-PROOF tests for the universal emit-chokepoint
// spans-scan guard (chsql.Emit → spansscan.UnwindowedSpansScans). They lower +
// emit query shapes that NO golden/fixture exercises, with the request window
// withheld from the inner recursive scan, and assert chsql.Emit fails closed
// with ErrUnboundedSpansScan.
//
// The decisive property: every case emits WITHOUT chsql.WithSpansTable. With no
// spans table threaded onto the emit context, every per-site guard
// (RequireSpansScansBounded, requireSpansScanWindow, requireInnerSpansScanBound)
// is a no-op — so the ONLY thing that can reject these statements is the
// universal backstop scanning the final SQL string (via the schema-default
// fallback table). This proves the metrics-path F1 hole could not have shipped
// even if the per-site guard were deleted: forgetting to thread the window onto a
// recursive scan is caught at the chokepoint regardless.

// lowerMetricsRangeWindow lowers a TraceQL metrics query (rate / count_over_time)
// and wraps it in the chplan.RangeWindow that /api/metrics/query_range builds
// (mirroring api/tempo/grpc_exports.go). searchWindowed controls whether the
// request window is threaded onto the lowering ctx via WithSearchWindow — i.e.
// whether stampRecursiveScanWindow bounds the emitter-synthetic recursive arm.
//
// With searchWindowed=false the RangeWindow grid still carries the request window
// (rendered as toDateTime64), but the inner recursive otel_traces scan is
// windowless — exactly the metrics-path F1 hole (a future edit dropping the
// WithSearchWindow line in the handler).
func lowerMetricsRangeWindow(t *testing.T, q string, searchWindowed bool) chplan.Node {
	t.Helper()
	s := schema.DefaultOTelTraces()
	expr, err := ast.Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	start, end := scanWindowEmitBounds()
	ctx := context.Background()
	if searchWindowed {
		ctx = tql.WithSearchTraceLimit(ctx, scanWindowEmitLimit)
		ctx = tql.WithSearchWindow(ctx, start, end)
	}
	plan, err := tql.Lower(ctx, expr, s)
	if err != nil {
		t.Fatalf("lower %q: %v", q, err)
	}
	return &chplan.RangeWindow{
		Input:           plan,
		Range:           scanWindowEmitStep,
		Step:            scanWindowEmitStep,
		Start:           start,
		End:             end,
		TimestampColumn: s.TimestampColumn,
	}
}

// TestEmitUniversalGuard_StructuralMetricsConstructionProof is the metrics-path
// proof. `{ } >> { } | rate()` lowers to a structural-closure WITH RECURSIVE
// under a rate range window. Without WithSearchWindow the recursive STEP arm is
// windowless while the range-window grid renders its bound as toDateTime64 (never
// fromUnixTimestamp64Nano) — the precise shape the pre-fix matcher missed. Emit
// without WithSpansTable, so only the universal backstop can fire.
func TestEmitUniversalGuard_StructuralMetricsConstructionProof(t *testing.T) {
	t.Parallel()
	const q = `{ } >> { } | rate()`

	// F1-hole shape: windowless inner recursive scan, window only on the grid.
	plan := lowerMetricsRangeWindow(t, q, false)
	if _, _, err := chsql.Emit(context.Background(), plan); !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("windowless structural-metrics recursive scan must be rejected by the universal backstop, got %v", err)
	}

	// Threading the request window (the production path) bounds the recursive
	// arm: the same shape now emits cleanly — proving the guard does not false
	// reject the windowed query.
	ok := lowerMetricsRangeWindow(t, q, true)
	if _, _, err := chsql.Emit(context.Background(), ok); err != nil {
		t.Fatalf("windowed structural-metrics plan must emit cleanly, got %v", err)
	}
}

// TestEmitUniversalGuard_NestedSetConstructionProof is the nested-set-numbering
// proof. `{ } | select(nestedSetLeft, nestedSetRight)` lowers to a
// NestedSetAnnotate (`_cerberus_ns_paths` WITH RECURSIVE). It is lowered WITH a
// window (so the leaf scans, and thus the statement, carry the request window),
// then the numbering node's window alone is zeroed — simulating a lowering that
// stamped every node but this one. Emit without WithSpansTable: only the
// universal backstop can catch the now-windowless recursive numbering arm.
func TestEmitUniversalGuard_NestedSetConstructionProof(t *testing.T) {
	t.Parallel()
	start, end := scanWindowEmitBounds()
	const q = `{ } | select(nestedSetLeft, nestedSetRight)`

	plan := lowerWindowedSearch(t, q, start, end)
	// Sanity: the fully-windowed plan emits cleanly (no false reject).
	if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
		t.Fatalf("windowed nested-set plan must emit cleanly, got %v", err)
	}

	// Drop ONLY the numbering window; the windowed leaf scans keep the statement
	// armed. The recursive numbering arm is now windowless → must fail closed.
	zeroNestedSetWindows(plan)
	if _, _, err := chsql.Emit(context.Background(), plan); !errors.Is(err, chsql.ErrUnboundedSpansScan) {
		t.Fatalf("windowless nested-set recursive arm must be rejected by the universal backstop, got %v", err)
	}
}

// TestEmitUniversalGuard_NegativesPass pins that the universal backstop never
// false-rejects the validated bounded shapes when emitted with no spans table:
// the simple matrix-family pass-through wrapper and a windowed structural
// recursive arm both emit cleanly.
func TestEmitUniversalGuard_NegativesPass(t *testing.T) {
	t.Parallel()

	// Simple `{ } | rate()` — matrix-family pass-through scan, no recursive arm.
	if _, _, err := chsql.Emit(context.Background(), lowerMetricsRangeWindow(t, `{ } | rate()`, false)); err != nil {
		t.Fatalf("simple windowed matrix metrics must emit cleanly, got %v", err)
	}
	// Windowed structural recursive arm (window threaded onto the inner scan).
	if _, _, err := chsql.Emit(context.Background(), lowerMetricsRangeWindow(t, `{ } >> { } | rate()`, true)); err != nil {
		t.Fatalf("windowed structural metrics must emit cleanly, got %v", err)
	}
}

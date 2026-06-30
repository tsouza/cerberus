package tempo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tql "github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// These tests pin the metrics-path half of the spans-scan WINDOW invariant: the
// request window must reach the EMITTER-SYNTHETIC recursive spans scans of a
// METRICS pipeline over a structural / nested-set source. The metrics
// /api/metrics/query_range RangeWindow wrap windows the metrics LEAF, but cannot
// reach BELOW a WITH RECURSIVE — so the universal traceql.stampRecursiveScanWindow
// pass (fed by the metrics handlers' WithSearchWindow threading) is what windows
// the recursive `t`-scan. Without it `{ } >> { } | rate()` full-scans retention
// behind an inert `TraceId IN (<seed>)`.
//
// Mirrors internal/chsql/scan_window_emit_test.go: every case asserts the
// recursive arm carries a direct INLINE fromUnixTimestamp64Nano(<startNano>)
// Timestamp prune — only the emitter-synthetic recursive scans inline the nanos
// (chplan leaves render `?`), so an inline match is a precise witness that the
// recursive arm itself is windowed, not just the metrics leaf. The structural
// case additionally zeroes the Window* nanos (keeping TimestampColumn — i.e. the
// node opted into windowing) and asserts the emit-time guard fires
// (ErrUnboundedSpansScan), proving the guard is armed on the metrics path. As in
// the chsql emit test, the nested-set numbering CTE asserts only the positive
// inline witness: its fail-closed guard is the search-path (TraceLimit > 0)
// structure-tab gate, while the metrics path's window comes from the same stamp
// the structural fail-closed case exercises.

const metricsRecursiveWindowStep = time.Minute

// lowerMetricsWithWindow lowers a TraceQL metrics-pipeline query with the request
// window threaded through ctx (no /api/search limit — the metrics path), exactly
// as the metrics handlers now do via WithSearchWindow before traceql.Lower.
func lowerMetricsWithWindow(t *testing.T, query string, start, end time.Time) chplan.Node {
	t.Helper()
	expr, err := ast.Parse(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	ctx := tql.WithSearchWindow(context.Background(), start, end)
	plan, err := tql.Lower(ctx, expr, tracesSchema())
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	return plan
}

// wrapMetricsMatrix builds the matrix-shape plan the /api/metrics/query_range
// handler emits: peel second stages, RangeWindow the aggregate, wrap for the
// Sample projection. start/end set the RangeWindow's own window so the matrix
// inner-scan guard is satisfied; the recursive-arm window is independent and
// comes from the lowering stamp already on plan.
func wrapMetricsMatrix(t *testing.T, plan chplan.Node, start, end time.Time) chplan.Node {
	t.Helper()
	stages, inner := peelMetricsSecondStages(plan)
	metrics, ok := unwrapMetricsAggregate(inner)
	if !ok {
		t.Fatalf("plan is not a metrics aggregate: %T", inner)
	}
	rw := &chplan.RangeWindow{
		Input:           inner,
		Range:           metricsRecursiveWindowStep,
		Step:            metricsRecursiveWindowStep,
		Start:           start,
		End:             end,
		TimestampColumn: tracesSchema().TimestampColumn,
	}
	return wrapMetricsForSample(
		applyMetricsSecondStages(rw, stages, []string{chsql.RangeWindowAnchorAlias}),
		metrics,
	)
}

// zeroRecursiveScanWindows clears the Window* nanos on every recursive
// emitter-synthetic node while LEAVING TimestampColumn set — simulating a node
// that opted into windowing but reaches emit with a zero window, exactly what the
// fail-closed requireSpansScanWindow guard exists to catch.
func zeroRecursiveScanWindows(plan chplan.Node) {
	chplan.Walk(plan, func(n chplan.Node) bool {
		switch v := n.(type) {
		case *chplan.StructuralJoin:
			v.WindowStartNano = 0
			v.WindowEndNano = 0
		case *chplan.NestedSetAnnotate:
			v.WindowStartNano = 0
			v.WindowEndNano = 0
		}
		return true
	})
}

// TestMetricsRecursiveArmWindowed proves FIX 1: a metrics pipeline over a
// structural / nested-set source emits a recursive arm bounded by the request
// window, and the emit-time guard fails closed when that window is absent.
func TestMetricsRecursiveArmWindowed(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	lang := &traceqlLang{schema: s}
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	// inlineWindowLo is the inline lower-bound Frag the recursive emitters render
	// for this window (InlineLit, not a positional `?`).
	inlineWindowLo := fmt.Sprintf("fromUnixTimestamp64Nano(%d)", start.UnixNano())

	cases := []struct {
		name       string
		query      string
		failClosed bool // structural arm's guard is unconditional; assert it fires
	}{
		{"structural_descendant", `{ } >> { } | rate()`, true},
		// A position-dependent nested-set filter (`nestedSetLeft > 0`, NOT the
		// root-test `nestedSetParent < 0` which lowers straight to ParentSpanId='')
		// backs the metric with the recursive nested-set numbering CTE — the
		// nested-set counterpart of the structural recursive arm. (The former
		// `{ nestedSetParent < 0 } | by(nestedSetParent) | rate()` case was the
		// Bug 1 shape: a standalone `by()` before a metric now folds into the
		// aggregate's by-clause — `rate() by (nestedSetParent)` — so it no longer
		// emits a nested-set numbering arm at all.)
		{"nested_set_filter", `{ nestedSetLeft > 0 } | rate()`, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := lowerMetricsWithWindow(t, tc.query, start, end)
			wrapped := wrapMetricsMatrix(t, plan, start, end)

			sql, err := emitScoped(t, lang, wrapped)
			if err != nil {
				t.Fatalf("windowed metrics emit: %v", err)
			}
			if !strings.Contains(sql, inlineWindowLo) {
				t.Fatalf("metrics recursive arm not windowed — no inline %s "+
					"(full-retention scan behind inert TraceId-IN):\n%s", inlineWindowLo, sql)
			}

			if !tc.failClosed {
				return
			}
			// Node opted into windowing (TimestampColumn set) but reaches emit
			// with a zero window -> the fail-closed guard must fire.
			zeroRecursiveScanWindows(wrapped)
			if _, err := emitScoped(t, lang, wrapped); !errors.Is(err, chsql.ErrUnboundedSpansScan) {
				t.Fatalf("zero-window metrics recursive arm under spans scope must fail closed, got %v", err)
			}
		})
	}
}

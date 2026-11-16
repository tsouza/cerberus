package tempo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	tql "github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// These tests pin the metrics-path half of the spans-scan WINDOW invariant: the
// EMITTER-SYNTHETIC recursive spans scans of a METRICS pipeline over a
// structural / nested-set source read the span the RangeWindow reads. The
// RangeWindow's own inner-scan bound windows the metrics LEAF but cannot reach
// BELOW a WITH RECURSIVE, so lowering (fed by the metrics handlers'
// WithSearchWindow threading) arms the recursive scans' window and the emit
// chokepoint (chplan.BoundMetricsPerTraceSubqueries) sets it to the
// RangeWindow's input span: (start - step, end] on the range path,
// (start, end] on the instant path. Without a window `{ } >> { } | rate()`
// full-scans retention behind an inert `TraceId IN (<seed>)`, and with only
// the request window it drops a child whose parent precedes start.
//
// Mirrors internal/chsql/scan_window_emit_test.go: every case asserts the
// recursive arms carry direct INLINE fromUnixTimestamp64Nano(<nanos>)
// Timestamp prunes at both ends of that span — only the emitter-synthetic
// recursive scans inline the nanos (chplan leaves render `?`), so an inline
// match is a precise witness that the recursive arm itself is windowed, not
// just the metrics leaf. The structural case additionally asserts both
// fail-closed guards (ErrUnboundedSpansScan) by their messages: a RangeWindow
// with no request window is rejected by its inner-scan guard, and the lowered
// closure, which opted into windowing (TimestampColumn set), is rejected by
// its own recursive-arm guard when emitted with its window zeroed. As in the
// chsql emit test, the nested-set numbering CTE asserts only the positive
// inline witnesses: its fail-closed guard is the search-path (TraceLimit > 0)
// structure-tab gate.

const metricsRecursiveWindowStep = time.Minute

// metricsInnerScanGuard and recursiveScanGuard are the openings of the two
// ErrUnboundedSpansScan messages, which tell apart the RangeWindow's
// inner-scan guard and a recursive arm's own window guard.
const (
	metricsInnerScanGuard = "metrics inner scan of"
	recursiveScanGuard    = "recursive spans scan of"
)

// recursiveWindowedArmsPerClosure is how many arms of one recursive closure
// must carry the request-window bound: the ANCHOR (its seed derived table) and
// the STEP (its physical `t` scan). Neither is reachable by a predicate from
// outside the WITH RECURSIVE, so each needs its own.
const recursiveWindowedArmsPerClosure = 2

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
// inner-scan guard is satisfied, and the emitter derives the recursive arms'
// window from it.
func wrapMetricsMatrix(t *testing.T, plan chplan.Node, start, end time.Time) chplan.Node {
	t.Helper()
	stages, inner := peelMetricsSecondStages(plan)
	metrics, ok := unwrapMetricsRoot[*chplan.MetricsAggregate](inner)
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

// wrapMetricsInstant builds the single-anchor plan the
// /api/metrics/query instant handler emits (execMetricsInstant): the
// RangeWindow's Start and End are both end and its Range and Step are
// end - start, so it reads (start, end].
func wrapMetricsInstant(t *testing.T, plan chplan.Node, start, end time.Time) chplan.Node {
	t.Helper()
	stages, inner := peelMetricsSecondStages(plan)
	metrics, ok := unwrapMetricsRoot[*chplan.MetricsAggregate](inner)
	if !ok {
		t.Fatalf("plan is not a metrics aggregate: %T", inner)
	}
	step := end.Sub(start)
	rw := &chplan.RangeWindow{
		Input:           inner,
		Range:           step,
		Step:            step,
		Start:           end,
		End:             end,
		TimestampColumn: tracesSchema().TimestampColumn,
	}
	return wrapMetricsForSample(applyMetricsSecondStages(rw, stages, nil), metrics)
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

// TestMetricsRecursiveArmWindowed proves that a metrics pipeline over a
// structural / nested-set source emits recursive arms bounded by the
// RangeWindow's input span, and that the emit-time guards fail closed when
// that window is absent.
func TestMetricsRecursiveArmWindowed(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	lang := &traceqlLang{schema: s}
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	// The recursive arms read the RangeWindow's input span, (start - step,
	// end] on the range path and (start, end] on the instant path, rendered
	// inclusive from the nanosecond after the exclusive bound as an inline
	// literal (InlineLit, not a positional `?`).
	windowLo := func(exclusive time.Time) string {
		return fmt.Sprintf("fromUnixTimestamp64Nano(%d)", exclusive.UnixNano()+1)
	}
	windowHi := fmt.Sprintf("fromUnixTimestamp64Nano(%d)", end.UnixNano())
	paths := []struct {
		name string
		wrap func(*testing.T, chplan.Node, time.Time, time.Time) chplan.Node
		lo   string
	}{
		{"range", wrapMetricsMatrix, windowLo(start.Add(-metricsRecursiveWindowStep))},
		{"instant", wrapMetricsInstant, windowLo(start)},
	}

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
		for _, path := range paths {
			t.Run(tc.name+"/"+path.name, func(t *testing.T) {
				t.Parallel()
				wrapped := path.wrap(t, lowerMetricsWithWindow(t, tc.query, start, end), start, end)

				sql, err := emitScoped(t, lang, wrapped)
				if err != nil {
					t.Fatalf("windowed metrics emit: %v", err)
				}
				// BOTH arms of the recursive CTE, not just the step. Each arm
				// is its own scope inside the WITH RECURSIVE — ClickHouse
				// pushes no predicate in from outside the closure — so the
				// anchor's seed scan reads full retention unless it carries
				// its own bound (#1942). Two inline bounds of each side is the
				// witness that it does; the emitter's per-arm placement is
				// pinned in internal/chsql/scan_window_emit_test.go.
				for _, bound := range []string{path.lo, windowHi} {
					if got := strings.Count(sql, bound); got < recursiveWindowedArmsPerClosure {
						t.Fatalf("metrics recursive closure has %d inline %s bounds, want >= %d "+
							"(anchor AND step arms) — an arm off the RangeWindow's span drops or over-reads spans:\n%s",
							got, bound, recursiveWindowedArmsPerClosure, sql)
					}
				}
			})
		}
		if !tc.failClosed {
			continue
		}
		t.Run(tc.name+"/fail_closed", func(t *testing.T) {
			t.Parallel()
			// Under a RangeWindow the emitter re-derives every recursive
			// arm's window from the RangeWindow, so the only way an arm
			// reaches emit unwindowed is a RangeWindow with no request window:
			// the RangeWindow's own inner-scan guard rejects it.
			unwindowed := wrapMetricsMatrix(t, lowerMetricsWithWindow(t, tc.query, start, end), time.Time{}, time.Time{})
			_, err := emitScoped(t, lang, unwindowed)
			if !errors.Is(err, chsql.ErrUnboundedSpansScan) || !strings.Contains(err.Error(), metricsInnerScanGuard) {
				t.Fatalf("metrics recursive closure under a windowless RangeWindow must fail closed at %q, got %v",
					metricsInnerScanGuard, err)
			}
			// The recursive arm's own guard stays armed on the node lowering
			// produced for the metrics path (TimestampColumn set): emitted
			// with its leaves windowed and its own window zeroed, the closure
			// fails closed.
			var closure *chplan.StructuralJoin
			chplan.Walk(lowerMetricsWithWindow(t, tc.query, start, end), func(n chplan.Node) bool {
				if sj, ok := n.(*chplan.StructuralJoin); ok && closure == nil {
					closure = sj
				}
				return closure == nil
			})
			if closure == nil || closure.TimestampColumn == "" {
				t.Fatalf("lowered %q carries no window-armed structural closure: %+v", tc.query, closure)
			}
			zeroRecursiveScanWindows(closure)
			tsCol := tracesSchema().TimestampColumn
			leafWindow := &chplan.Binary{
				Op:    chplan.OpAnd,
				Left:  chplan.TimestampBound(chplan.OpGe, start, tsCol),
				Right: chplan.TimestampBound(chplan.OpLe, end, tsCol),
			}
			_, err = emitScoped(t, lang, chplan.PushLeafWindow(closure, leafWindow, nil))
			if !errors.Is(err, chsql.ErrUnboundedSpansScan) || !strings.Contains(err.Error(), recursiveScanGuard) {
				t.Fatalf("zero-window recursive arm under spans scope must fail closed at %q, got %v",
					recursiveScanGuard, err)
			}
		})
	}
}

// TestMetricsRecursiveScanWindowCohortAndOriginal pins two halves of the
// emit-time re-derivation of the recursive scans' window that the inline-SQL
// witnesses above cannot tell apart. A structural closure inside a per-trace
// cohort subquery (the `| count() > 1` membership) reads the RangeWindow's
// input span widened by the maximum trace duration on both sides, as the
// cohort's own leaves do, while the closure in the metrics row source reads the
// span itself. And the lowered plan the RangeWindow wraps is left as lowering
// stamped it: the re-derivation works on a copy, so a plan emitted twice (the
// metrics statement and its exemplars) is not windowed by the first emit.
func TestMetricsRecursiveScanWindowCohortAndOriginal(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	const q = `{ } >> { } | count() > 1 | rate()`

	plan := lowerMetricsWithWindow(t, q, start, end)
	_, inner := peelMetricsSecondStages(plan)
	rw := &chplan.RangeWindow{
		Input:           inner,
		Range:           metricsRecursiveWindowStep,
		Step:            metricsRecursiveWindowStep,
		Start:           start,
		End:             end,
		TimestampColumn: s.TimestampColumn,
	}

	type window struct{ lo, hi int64 }
	closureWindows := func(n chplan.Node) map[window]int {
		got := map[window]int{}
		chplan.WalkDeep(n, func(x chplan.Node) bool {
			if sj, ok := x.(*chplan.StructuralJoin); ok {
				got[window{sj.WindowStartNano, sj.WindowEndNano}]++
			}
			return true
		})
		return got
	}

	spanLo := start.Add(-metricsRecursiveWindowStep).UnixNano() + 1
	lookback := s.MaxTraceDuration.Nanoseconds()
	want := map[window]int{
		{spanLo, end.UnixNano()}:                       1, // the metrics row source's closure
		{spanLo - lookback, end.UnixNano() + lookback}: 1, // the cohort subquery's closure
	}
	if got := closureWindows(chplan.BoundMetricsPerTraceSubqueries(rw)); !maps.Equal(got, want) {
		t.Fatalf("%s closure windows after the emit re-derivation = %v, want %v", q, got, want)
	}

	// Lowering stamps the request window, widened the same way in the cohort.
	lowered := map[window]int{
		{start.UnixNano(), end.UnixNano()}:                       1,
		{start.UnixNano() - lookback, end.UnixNano() + lookback}: 1,
	}
	if got := closureWindows(plan); !maps.Equal(got, lowered) {
		t.Fatalf("%s lowered plan's closure windows changed by the emit re-derivation = %v, want %v", q, got, lowered)
	}

	// With no per-trace subquery the per-trace bound copies nothing, so the
	// recursive-scan re-derivation's own copy is all that keeps the plan intact.
	const bare = `{ } >> { } | rate()`
	barePlan := lowerMetricsWithWindow(t, bare, start, end)
	_, bareInner := peelMetricsSecondStages(barePlan)
	bareRW := *rw
	bareRW.Input = bareInner
	if got, want := closureWindows(chplan.BoundMetricsPerTraceSubqueries(&bareRW)), (map[window]int{{spanLo, end.UnixNano()}: 1}); !maps.Equal(got, want) {
		t.Fatalf("%s closure windows after the emit re-derivation = %v, want %v", bare, got, want)
	}
	if got, want := closureWindows(barePlan), (map[window]int{{start.UnixNano(), end.UnixNano()}: 1}); !maps.Equal(got, want) {
		t.Fatalf("%s lowered plan's closure windows changed by the emit re-derivation = %v, want %v", bare, got, want)
	}
}

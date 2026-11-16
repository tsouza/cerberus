package chplan

import "time"

// StampRecursiveScanWindow sets the window [startNano, endNano] (unix
// nanoseconds, both inclusive, 0 = unset) on every NestedSetAnnotate and
// StructuralJoin n reaches and returns the result. Those nodes, and the
// Filters on the way to them, are modified in place; a node of any other type
// on the way is rebuilt by RewriteChildren, so a caller that must keep n
// intact stamps a CloneNode of it. Those nodes scan the
// spans table themselves inside a WITH RECURSIVE (the nested-set numbering
// CTE, the structural closure's anchor and step arms), so no leaf predicate
// reaches them; the window is what the emitter renders on each of their
// spans scans.
//
// A non-empty tsCol becomes each StructuralJoin's TimestampColumn, which arms
// the emitted window and its fail-closed guard; an empty tsCol keeps the
// column each node already carries. A NestedSetAnnotate carries its column
// from lowering.
//
// A recursive scan inside a trace-cohort subquery is part of that per-trace
// subquery, so it takes the window widened by lookback on each set side:
// the window the cohort's own leaves read.
func StampRecursiveScanWindow(n Node, startNano, endNano int64, tsCol string, lookback time.Duration) Node {
	switch v := n.(type) {
	case *NestedSetAnnotate:
		v.WindowStartNano = startNano
		v.WindowEndNano = endNano
		v.Input = StampRecursiveScanWindow(v.Input, startNano, endNano, tsCol, lookback)
		return v
	case *StructuralJoin:
		if tsCol != "" {
			v.TimestampColumn = tsCol
		}
		v.WindowStartNano = startNano
		v.WindowEndNano = endNano
		v.Left = StampRecursiveScanWindow(v.Left, startNano, endNano, tsCol, lookback)
		v.Right = StampRecursiveScanWindow(v.Right, startNano, endNano, tsCol, lookback)
		return v
	case *Filter:
		// RewriteChildren cannot reach a Node behind an Expr, and the
		// trace-cohort subquery of a spanset-aggregate operand
		// (`({A} >> {B} | count() > 1)`) can hold a structural closure, so
		// the cohort subqueries are descended here. The cohort's window is
		// already widened, so a cohort nested inside it reads the same
		// window.
		const alreadyWidened time.Duration = 0
		cohortStart, cohortEnd := widenWindowNanos(startNano, endNano, lookback)
		for _, in := range CohortSubqueries(v.Predicate) {
			in.Subquery = StampRecursiveScanWindow(in.Subquery, cohortStart, cohortEnd, tsCol, alreadyWidened)
		}
		v.Input = StampRecursiveScanWindow(v.Input, startNano, endNano, tsCol, lookback)
		return v
	default:
		out, _ := RewriteChildren(n, func(c Node) (Node, bool) {
			return StampRecursiveScanWindow(c, startNano, endNano, tsCol, lookback), true
		})
		return out
	}
}

// widenWindowNanos widens a nanosecond window as a per-trace window widens
// the window it derives from: each set (non-zero) side moves outward by
// lookback, and an unset side stays 0.
func widenWindowNanos(startNano, endNano int64, lookback time.Duration) (int64, int64) {
	if startNano != 0 {
		startNano -= lookback.Nanoseconds()
	}
	if endNano != 0 {
		endNano += lookback.Nanoseconds()
	}
	return startNano, endNano
}

// carriesRecursiveScan reports whether n, or a subquery behind one of its
// expressions, holds a NestedSetAnnotate or a StructuralJoin.
func carriesRecursiveScan(n Node) bool {
	found := false
	WalkDeep(n, func(x Node) bool {
		switch x.(type) {
		case *NestedSetAnnotate, *StructuralJoin:
			found = true
		}
		return !found
	})
	return found
}

// windowMetricsRecursiveScans returns inner with every recursive spans scan
// it carries reading rw's input span (Start - Offset - Range, End - Offset],
// the span the RangeWindow reads, and a recursive scan inside a per-trace
// subquery reading that span widened by lookback, as the per-trace
// subquery's leaves do (PerTraceWindowBound). The scans' window is inclusive
// at both ends, so the exclusive lower instant is stamped one nanosecond
// later. inner is never modified; it is returned itself when rw carries no
// request window or inner carries no recursive scan.
func windowMetricsRecursiveScans(rw *RangeWindow, inner Node, lookback time.Duration) Node {
	lower, upper, ok := rangeWindowInputSpan(rw)
	if !ok || inner == nil || !carriesRecursiveScan(inner) {
		return inner
	}
	const afterExclusiveBound = 1 // nanosecond
	return StampRecursiveScanWindow(CloneNode(inner), lower.UnixNano()+afterExclusiveBound, upper.UnixNano(), "", lookback)
}

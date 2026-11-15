package chplan

import "time"

// This file bounds the per-trace subqueries of a TraceQL metrics pipeline by
// the RangeWindow wrapped around it.
//
// A trace-scoped intrinsic (rootName, rootServiceName, traceDuration) or a
// spanset-aggregate operand is a value of the whole trace, read from a
// per-trace subquery that GROUPs the spans table BY trace id: a
// TraceLookupFilter lookup joined onto the spans (`{ rootName = name }`), or
// a membership subquery in a Filter predicate (`{ traceDuration > 1s }`).
// The metrics emitters bound the rows of the metrics input to the
// RangeWindow's input span at emit time, in the SELECT around the input
// (maybePushInnerScanTimeBounds). That bound reaches neither a subquery behind
// an Expr slot nor the lookup side of a join, so each per-trace subquery would
// read the spans table across full retention.
//
// Each per-trace subquery reads its traces' spans inside the metric's input
// span (Start - Offset - Range, End - Offset] widened on both sides by the
// metrics node's PerTraceLookback — the configured maximum trace duration —
// so a trace no longer than that setting is read whole: its root span and its
// full extent resolve even when they lie outside the input span. The plain
// row source a per-trace subquery filters (a Scan, or a Filter chain over
// one) carries the unwidened input span: the spans the metric counts are the
// spans inside it, and the MergeTree read under the join or the membership
// test is pruned to it.

// rangeWindowInputSpan returns the instants of rw's input span,
// (lower, upper], and whether rw carries a request window. A zero Range reads
// as Step, as the metrics emitters read it.
func rangeWindowInputSpan(rw *RangeWindow) (lower, upper time.Time, ok bool) {
	if rw == nil || rw.Start.IsZero() || rw.End.IsZero() {
		return time.Time{}, time.Time{}, false
	}
	reach := rw.Range
	if reach == 0 {
		reach = rw.Step
	}
	return rw.Start.Add(-rw.Offset).Add(-reach), rw.End.Add(-rw.Offset), true
}

// RangeWindowInputBound is the predicate that keeps a row of rw's input
// exactly when some anchor of rw reads it, over the input's timestamp column
// tsCol:
//
//	tsCol > Start - Offset - Range AND tsCol <= End - Offset
//
// It is the bound the metrics emitters render around the metrics input
// (chsql innerScanTsBoundsFrags); a zero Range reads as Step, as there. It is
// nil when rw carries no request window (a zero Start or End) or tsCol is
// empty.
func RangeWindowInputBound(rw *RangeWindow, tsCol string) Expr {
	return PerTraceWindowBound(rw, tsCol, 0)
}

// PerTraceWindowBound is RangeWindowInputBound widened by lookback on both
// sides — the window a per-trace subquery under rw reads its traces' spans
// in:
//
//	tsCol > Start - Offset - Range - lookback AND tsCol <= End - Offset + lookback
//
// A zero lookback is RangeWindowInputBound itself. It is nil exactly when
// RangeWindowInputBound is.
func PerTraceWindowBound(rw *RangeWindow, tsCol string, lookback time.Duration) Expr {
	lower, upper, ok := rangeWindowInputSpan(rw)
	if !ok || tsCol == "" {
		return nil
	}
	return &Binary{
		Op:    OpAnd,
		Left:  TimestampBound(OpGt, lower.Add(-lookback), tsCol),
		Right: TimestampBound(OpLe, upper.Add(lookback), tsCol),
	}
}

// BoundMetricsPerTraceSubqueries returns root with every RangeWindow over a
// TraceQL metrics node (MetricsAggregate, MetricsHistogramOverTime,
// MetricsCompare) replaced by one whose metrics node is
// WindowMetricsPerTraceSubqueries of it. root is never modified; when no
// RangeWindow needs it, root itself is returned.
//
// The chsql emit chokepoint runs it on every plan, so the bound is derived
// from the RangeWindow the emitter renders and every metrics entry point —
// range, instant, histogram, compare, explain — carries it.
func BoundMetricsPerTraceSubqueries(root Node) Node {
	if root == nil {
		return nil
	}
	if rw, ok := root.(*RangeWindow); ok {
		if input := WindowMetricsPerTraceSubqueries(rw, rw.Input); input != rw.Input {
			bounded := *rw
			bounded.Input = input
			return &bounded
		}
	}
	out, _ := RewriteChildren(root, func(c Node) (Node, bool) {
		bounded := BoundMetricsPerTraceSubqueries(c)
		return bounded, bounded != c
	})
	return out
}

// WindowMetricsPerTraceSubqueries returns metrics — the MetricsAggregate,
// MetricsHistogramOverTime or MetricsCompare node rw wraps — with its Inner
// replaced by WindowPerTraceSubqueries of it: rw's input bound
// (RangeWindowInputBound over the node's own input timestamp column) on the
// plain row sources, and that bound widened by the node's PerTraceLookback
// (PerTraceWindowBound) on the per-trace subqueries. metrics is never
// modified; any other node type, a window with no request span, or an Inner
// with no per-trace subquery returns metrics itself.
func WindowMetricsPerTraceSubqueries(rw *RangeWindow, metrics Node) Node {
	switch m := metrics.(type) {
	case *MetricsAggregate:
		return WindowMetricsAggregatePerTraceSubqueries(rw, m)
	case *MetricsHistogramOverTime:
		tsCol, _ := m.InputTimestampColumn()
		if inner, changed := windowMetricsInner(rw, m.Inner, tsCol, m.PerTraceLookback); changed {
			bounded := *m
			bounded.Inner = inner
			return &bounded
		}
	case *MetricsCompare:
		// The compare({...}) selection is a predicate over an Inner span row:
		// a trace-scoped intrinsic compared with a literal in it carries its
		// membership subquery there, beside Inner rather than inside it.
		tsCol, _ := m.InputTimestampColumn()
		inner, innerChanged := windowMetricsInner(rw, m.Inner, tsCol, m.PerTraceLookback)
		selection, selectionChanged := windowCohortSubqueries(m.Selection, PerTraceWindowBound(rw, tsCol, m.PerTraceLookback))
		if innerChanged || selectionChanged {
			bounded := *m
			bounded.Inner = inner
			bounded.Selection = selection
			return &bounded
		}
	}
	return metrics
}

// WindowMetricsAggregatePerTraceSubqueries is WindowMetricsPerTraceSubqueries
// for a MetricsAggregate, typed for a caller that renders m outside the emit
// chokepoint (the exemplar statement).
func WindowMetricsAggregatePerTraceSubqueries(rw *RangeWindow, m *MetricsAggregate) *MetricsAggregate {
	tsCol, _ := m.InputTimestampColumn()
	if inner, changed := windowMetricsInner(rw, m.Inner, tsCol, m.PerTraceLookback); changed {
		bounded := *m
		bounded.Inner = inner
		return &bounded
	}
	return m
}

// windowCohortSubqueries returns a copy of e whose cohort subqueries
// (CohortSubqueries) take window — a per-trace window — at their leaves, and
// whether e carries any; e is never modified, and is returned itself when it
// carries none or window is nil.
func windowCohortSubqueries(e, window Expr) (Expr, bool) {
	if window == nil || len(CohortSubqueries(e)) == 0 {
		return e, false
	}
	bounded := cloneExpr(e)
	PushLeafPredicateIntoCohort(bounded, window)
	return bounded, true
}

// windowMetricsInner is WindowPerTraceSubqueries of a metrics node's inner
// under rw's input bound over tsCol, widened by lookback for the per-trace
// subqueries, and whether that changed it.
func windowMetricsInner(rw *RangeWindow, inner Node, tsCol string, lookback time.Duration) (Node, bool) {
	bounded := WindowPerTraceSubqueries(inner, RangeWindowInputBound(rw, tsCol), PerTraceWindowBound(rw, tsCol, lookback))
	return bounded, bounded != inner
}

// WindowPerTraceSubqueries returns a copy of n in which perTrace bounds every
// per-trace subquery n's row-flow tree carries, and rows bounds the plain row
// source each one filters:
//
//   - a TraceLookupFilter: each lookup relation's leaves and each cohort
//     subquery of its predicate take perTrace, and its Input takes rows when
//     that is a plain row source (a Scan, or a Filter chain over one);
//   - a Filter whose predicate carries a cohort subquery: each cohort
//     subquery takes perTrace, and the Filter itself takes rows when it is a
//     plain row source.
//
// No other leaf takes either: a metrics row source that carries no per-trace
// subquery is bounded by the RangeWindow emitter alone, and a structural
// join's operands keep reading what they read today.
//
// n is never modified. It is returned itself when rows or perTrace is nil or
// n carries no per-trace subquery.
func WindowPerTraceSubqueries(n Node, rows, perTrace Expr) Node {
	if n == nil || rows == nil || perTrace == nil || !carriesPerTraceSubquery(n) {
		return n
	}
	return windowPerTrace(CloneNode(n), rows, perTrace)
}

// carriesPerTraceSubquery reports whether n's row-flow tree holds a
// TraceLookupFilter or a Filter whose predicate carries a cohort subquery.
func carriesPerTraceSubquery(n Node) bool {
	found := false
	Walk(n, func(x Node) bool {
		switch v := x.(type) {
		case *TraceLookupFilter:
			found = true
		case *Filter:
			if len(CohortSubqueries(v.Predicate)) > 0 {
				found = true
			}
		}
		return !found
	})
	return found
}

// windowPerTrace is WindowPerTraceSubqueries' walk over a tree it solely
// owns: PushLeafPredicate mutates the Filters it reaches in place.
func windowPerTrace(n Node, rows, perTrace Expr) Node {
	switch v := n.(type) {
	case *TraceLookupFilter:
		PushLeafPredicateIntoCohort(v.Predicate, perTrace)
		for i, lookup := range v.Lookups {
			v.Lookups[i] = PushLeafPredicate(lookup, perTrace)
		}
		v.Input = windowPlainRowSource(windowPerTrace(v.Input, rows, perTrace), rows)
		return v
	case *Filter:
		if len(CohortSubqueries(v.Predicate)) > 0 {
			PushLeafPredicateIntoCohort(v.Predicate, perTrace)
			v.Input = windowPerTrace(v.Input, rows, perTrace)
			return windowPlainRowSource(v, rows)
		}
	}
	out, _ := RewriteChildren(n, func(c Node) (Node, bool) {
		return windowPerTrace(c, rows, perTrace), true
	})
	return out
}

// windowPlainRowSource conjoins window onto n when n is a plain row source —
// a Scan, which it wraps in a Filter, or a chain of Filters over one, whose
// lowest Filter takes it — and that chain does not already carry window.
// Any other node is returned unchanged.
func windowPlainRowSource(n Node, window Expr) Node {
	switch v := n.(type) {
	case *Scan:
		return &Filter{Input: v, Predicate: window}
	case *Filter:
		if exprReferences(v.Predicate, window) {
			return v
		}
		if _, ok := v.Input.(*Scan); ok {
			v.Predicate = conjoinScanPred(v.Predicate, window)
			return v
		}
		if _, ok := v.Input.(*Filter); ok {
			v.Input = windowPlainRowSource(v.Input, window)
		}
	}
	return n
}

// exprReferences reports whether target is e or one of e's sub-expressions,
// by identity.
func exprReferences(e, target Expr) bool {
	found := false
	InspectExpr(e, func(x Expr) bool {
		if x == target {
			found = true
		}
		return !found
	})
	return found
}

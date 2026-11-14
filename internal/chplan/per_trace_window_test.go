package chplan

import (
	"testing"
	"time"
)

const perTraceTestTable = "otel_traces"

// perTraceTestScan is a spans Scan whose row type names its timestamp column,
// so a metrics node over it resolves InputTimestampColumn.
func perTraceTestScan() *Scan {
	return &Scan{
		Table: perTraceTestTable,
		Roles: []Column{{Name: "Timestamp", Role: RoleTimestamp}, {Name: "TraceId", Role: RoleTraceID}},
	}
}

// perTraceTestCohort is a membership test whose subquery groups the spans
// table, the shape a trace-scoped intrinsic compared with a literal lowers to.
func perTraceTestCohort() *InSubquery {
	return &InSubquery{
		Left: &ColumnRef{Name: "TraceId"},
		Subquery: &Aggregate{
			Input:   perTraceTestScan(),
			GroupBy: []Expr{&ColumnRef{Name: "TraceId"}},
		},
	}
}

// perTraceTestLookupFilter is `{ rootName = name }`'s shape: one lookup over
// root spans joined onto the spans.
func perTraceTestLookupFilter() *TraceLookupFilter {
	return &TraceLookupFilter{
		Input: perTraceTestScan(),
		Lookups: []Node{&Aggregate{
			Input:   &Filter{Input: perTraceTestScan(), Predicate: &Binary{Op: OpEq, Left: &ColumnRef{Name: "ParentSpanId"}, Right: &LitString{}}},
			GroupBy: []Expr{&ColumnRef{Name: "TraceId"}},
		}},
		Predicate: &Binary{Op: OpEq, Left: &ColumnRef{Name: "_cerb_root_name"}, Right: &ColumnRef{Name: "SpanName"}},
	}
}

func perTraceTestWindow(input Node) *RangeWindow {
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	return &RangeWindow{
		Input: input, Range: time.Minute, Step: time.Minute,
		Start: start, End: start.Add(time.Hour), TimestampColumn: "Timestamp",
	}
}

// boundedScans counts the Scans reachable from n, through Expr slots too, whose
// enclosing Filter's predicate references window, and all Scans.
func boundedScans(n Node, window Expr) (bounded, total int) {
	WalkDeep(n, func(x Node) bool {
		switch v := x.(type) {
		case *Scan:
			total++
		case *Filter:
			if _, ok := v.Input.(*Scan); ok && exprReferences(v.Predicate, window) {
				bounded++
			}
		}
		return true
	})
	return bounded, total
}

func TestRangeWindowInputBound(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	const offset = 5 * time.Minute
	cases := []struct {
		name         string
		rw           *RangeWindow
		lower, upper time.Time
	}{
		{"range", &RangeWindow{Range: 2 * time.Minute, Step: time.Minute, Start: start, End: end}, start.Add(-2 * time.Minute), end},
		{"zero_range_reads_step", &RangeWindow{Step: time.Minute, Start: start, End: end}, start.Add(-time.Minute), end},
		{"offset", &RangeWindow{Range: time.Minute, Step: time.Minute, Offset: offset, Start: start, End: end}, start.Add(-offset - time.Minute), end.Add(-offset)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := &Binary{
				Op:    OpAnd,
				Left:  TimestampBound(OpGt, tc.lower, "Timestamp"),
				Right: TimestampBound(OpLe, tc.upper, "Timestamp"),
			}
			if got := RangeWindowInputBound(tc.rw, "Timestamp"); got == nil || !got.Equal(want) {
				t.Fatalf("RangeWindowInputBound = %#v, want %#v", got, want)
			}
		})
	}
	for name, rw := range map[string]*RangeWindow{
		"no_start": {Step: time.Minute, End: end},
		"no_end":   {Step: time.Minute, Start: start},
	} {
		if got := RangeWindowInputBound(rw, "Timestamp"); got != nil {
			t.Fatalf("%s: RangeWindowInputBound = %#v, want nil", name, got)
		}
	}
	if got := RangeWindowInputBound(&RangeWindow{Step: time.Minute, Start: start, End: end}, ""); got != nil {
		t.Fatalf("empty timestamp column: RangeWindowInputBound = %#v, want nil", got)
	}
}

// TestBoundMetricsPerTraceSubqueriesBoundsEveryPerTraceScan pins that every
// spans scan behind a per-trace subquery — each lookup, each membership
// subquery, and the plain row source each one filters — takes the RangeWindow
// input bound, and that the input plan is left as it was.
func TestBoundMetricsPerTraceSubqueriesBoundsEveryPerTraceScan(t *testing.T) {
	t.Parallel()
	cases := map[string]func() Node{
		"lookup": func() Node { return &MetricsAggregate{Inner: perTraceTestLookupFilter()} },
		"membership": func() Node {
			return &MetricsAggregate{Inner: &Filter{Input: perTraceTestScan(), Predicate: perTraceTestCohort()}}
		},
		// A membership conjunct below the join, as traceScopedFilter places
		// one: the shared plain row source is bounded once, not twice.
		"lookup_over_membership": func() Node {
			lookup := perTraceTestLookupFilter()
			lookup.Input = &Filter{Input: perTraceTestScan(), Predicate: perTraceTestCohort()}
			return &MetricsAggregate{Inner: lookup}
		},
		"histogram": func() Node { return &MetricsHistogramOverTime{Inner: perTraceTestLookupFilter()} },
		"compare_selection": func() Node {
			return &MetricsCompare{Inner: perTraceTestScan(), Selection: perTraceTestCohort()}
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rw := perTraceTestWindow(build())
			before := CloneNode(rw)
			out := BoundMetricsPerTraceSubqueries(rw)
			if !rw.Equal(before) {
				t.Fatal("BoundMetricsPerTraceSubqueries modified its input")
			}
			bounded, ok := out.(*RangeWindow)
			if !ok || bounded == rw {
				t.Fatalf("BoundMetricsPerTraceSubqueries returned %T (same node: %v), want a new RangeWindow", out, out == Node(rw))
			}
			tsCol := "Timestamp"
			window := findWindow(bounded.Input, tsCol)
			if window == nil {
				t.Fatal("no RangeWindow input bound in the bounded plan")
			}
			if !window.Equal(RangeWindowInputBound(rw, tsCol)) {
				t.Fatalf("bound %#v, want RangeWindowInputBound %#v", window, RangeWindowInputBound(rw, tsCol))
			}
			n, total := boundedScans(perTraceSubtrees(bounded.Input), window)
			if n != total || total == 0 {
				t.Fatalf("%d of %d per-trace scans bounded", n, total)
			}
			// The bound reaches each Filter once: a second conjunct of it
			// would be the same predicate rendered twice.
			WalkDeep(bounded.Input, func(x Node) bool {
				if f, ok := x.(*Filter); ok && countReferences(f.Predicate, window) > 1 {
					t.Fatalf("Filter carries the bound %d times", countReferences(f.Predicate, window))
				}
				return true
			})
		})
	}
}

// TestWindowPerTraceSubqueriesLeavesNonPlainSourcesAlone pins that a per-trace
// node's row source that is not a plain scan keeps reading what it read: the
// structural operands of `{ } >> { rootName = name }` are bounded by the
// RangeWindow emitter and the recursive-arm stamp, not by this pass.
func TestWindowPerTraceSubqueriesLeavesNonPlainSourcesAlone(t *testing.T) {
	t.Parallel()
	lookup := perTraceTestLookupFilter()
	lookup.Input = &StructuralJoin{Left: perTraceTestScan(), Right: perTraceTestScan(), Op: StructuralDescendant}
	window := RangeWindowInputBound(perTraceTestWindow(nil), "Timestamp")
	out, ok := WindowPerTraceSubqueries(lookup, window).(*TraceLookupFilter)
	if !ok {
		t.Fatalf("WindowPerTraceSubqueries returned %T", out)
	}
	if _, total := boundedScans(out.Input, window); total == 0 {
		t.Fatal("structural input lost its scans")
	}
	if n, _ := boundedScans(out.Input, window); n != 0 {
		t.Fatalf("%d structural operand scans took the bound, want 0", n)
	}
	if n, total := boundedScans(out.Lookups[0], window); n != total || total == 0 {
		t.Fatalf("%d of %d lookup scans bounded", n, total)
	}
}

// TestWindowPerTraceSubqueriesNoOp pins the identity returns: no window, or no
// per-trace subquery, returns the node itself.
func TestWindowPerTraceSubqueriesNoOp(t *testing.T) {
	t.Parallel()
	window := RangeWindowInputBound(perTraceTestWindow(nil), "Timestamp")
	plain := &Filter{Input: perTraceTestScan(), Predicate: &LitBool{V: true}}
	if got := WindowPerTraceSubqueries(plain, window); got != Node(plain) {
		t.Fatalf("plain filter rewritten to %T", got)
	}
	lookup := perTraceTestLookupFilter()
	if got := WindowPerTraceSubqueries(lookup, nil); got != Node(lookup) {
		t.Fatalf("nil window rewrote the lookup filter to %T", got)
	}
	windowless := perTraceTestWindow(&MetricsAggregate{Inner: perTraceTestLookupFilter()})
	windowless.Start = time.Time{}
	if got := BoundMetricsPerTraceSubqueries(windowless); got != Node(windowless) {
		t.Fatalf("windowless RangeWindow rewritten to %T", got)
	}
}

// perTraceSubtrees collects, under one Project, every per-trace subquery of n
// and the plain row source each filters — the scans the bound must reach.
func perTraceSubtrees(n Node) Node {
	var parts []Node
	WalkDeep(n, func(x Node) bool {
		switch v := x.(type) {
		case *TraceLookupFilter:
			parts = append(parts, v.Lookups...)
			parts = append(parts, v.Input)
		case *Filter:
			if len(CohortSubqueries(v.Predicate)) > 0 {
				parts = append(parts, v)
				return false
			}
		case *MetricsCompare:
			for _, in := range CohortSubqueries(v.Selection) {
				parts = append(parts, in.Subquery)
			}
		}
		return true
	})
	if len(parts) == 0 {
		return nil
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out = &CrossJoin{Left: out, Right: p}
	}
	return out
}

// findWindow returns the first `tsCol > … AND tsCol <= …` predicate conjunct
// pair a Filter in n carries.
func findWindow(n Node, tsCol string) Expr {
	var found Expr
	WalkDeep(n, func(x Node) bool {
		if found != nil {
			return false
		}
		f, ok := x.(*Filter)
		if !ok {
			return true
		}
		InspectExpr(f.Predicate, func(e Expr) bool {
			b, ok := e.(*Binary)
			if found == nil && ok && b.Op == OpAnd && isTsBound(b.Left, OpGt, tsCol) && isTsBound(b.Right, OpLe, tsCol) {
				found = b
			}
			return found == nil
		})
		return found == nil
	})
	return found
}

func isTsBound(e Expr, op BinaryOp, tsCol string) bool {
	b, ok := e.(*Binary)
	if !ok || b.Op != op {
		return false
	}
	col, ok := b.Left.(*ColumnRef)
	return ok && col.Name == tsCol
}

func countReferences(e, target Expr) int {
	n := 0
	InspectExpr(e, func(x Expr) bool {
		if x == target {
			n++
		}
		return true
	})
	return n
}

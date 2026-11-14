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

// perTraceTestLookback is the PerTraceLookback the metrics nodes under test
// carry: the maximum trace duration a per-trace subquery reads beyond the
// RangeWindow input span.
const perTraceTestLookback = time.Hour

func TestPerTraceWindowBound(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	const offset = 5 * time.Minute
	rw := &RangeWindow{Range: 2 * time.Minute, Step: time.Minute, Offset: offset, Start: start, End: end}
	want := &Binary{
		Op:    OpAnd,
		Left:  TimestampBound(OpGt, start.Add(-offset-2*time.Minute-perTraceTestLookback), "Timestamp"),
		Right: TimestampBound(OpLe, end.Add(-offset+perTraceTestLookback), "Timestamp"),
	}
	if got := PerTraceWindowBound(rw, "Timestamp", perTraceTestLookback); got == nil || !got.Equal(want) {
		t.Fatalf("PerTraceWindowBound = %#v, want %#v", got, want)
	}
	if got := PerTraceWindowBound(rw, "Timestamp", 0); got == nil || !got.Equal(RangeWindowInputBound(rw, "Timestamp")) {
		t.Fatalf("PerTraceWindowBound with no lookback = %#v, want RangeWindowInputBound", got)
	}
	if got := PerTraceWindowBound(&RangeWindow{Step: time.Minute, End: end}, "Timestamp", perTraceTestLookback); got != nil {
		t.Fatalf("no start: PerTraceWindowBound = %#v, want nil", got)
	}
}

// scanBound is one spans Scan of a bounded plan: the Timestamp bound its
// enclosing Filter carries (nil when there is none), and whether the scan sits
// inside a per-trace subquery — a lookup relation or a cohort subquery —
// rather than in a plain row source.
type scanBound struct {
	bound    Expr
	perTrace bool
}

// collectScanBounds records a scanBound for every Scan reachable from n,
// through Expr slots too. A MetricsCompare's RootLookup is the compare
// emitter's to bound, so it is not descended.
func collectScanBounds(n Node, perTrace bool, tsCol string, out *[]scanBound) {
	cohorts := func(e Expr) {
		for _, in := range CohortSubqueries(e) {
			collectScanBounds(in.Subquery, true, tsCol, out)
		}
	}
	switch v := n.(type) {
	case nil:
		return
	case *Scan:
		*out = append(*out, scanBound{perTrace: perTrace})
		return
	case *Filter:
		cohorts(v.Predicate)
		if _, ok := v.Input.(*Scan); ok {
			*out = append(*out, scanBound{bound: timestampBoundOf(v.Predicate, tsCol), perTrace: perTrace})
			return
		}
		collectScanBounds(v.Input, perTrace, tsCol, out)
		return
	case *TraceLookupFilter:
		cohorts(v.Predicate)
		for _, lookup := range v.Lookups {
			collectScanBounds(lookup, true, tsCol, out)
		}
		collectScanBounds(v.Input, perTrace, tsCol, out)
		return
	case *MetricsCompare:
		cohorts(v.Selection)
		collectScanBounds(v.Inner, perTrace, tsCol, out)
		return
	}
	for _, c := range n.Children() {
		collectScanBounds(c, perTrace, tsCol, out)
	}
}

// timestampBoundOf rebuilds the `tsCol > … AND tsCol <= …` window pred
// carries among its top-level conjuncts, or nil when it carries none. It
// fails the caller's expectations, rather than picking one, when pred carries
// the window twice: the same predicate rendered twice is a defect too.
func timestampBoundOf(pred Expr, tsCol string) Expr {
	var lower, upper []Expr
	for _, c := range flattenConjuncts(pred) {
		switch {
		case isTsBound(c, OpGt, tsCol):
			lower = append(lower, c)
		case isTsBound(c, OpLe, tsCol):
			upper = append(upper, c)
		}
	}
	if len(lower) != 1 || len(upper) != 1 {
		return nil
	}
	return &Binary{Op: OpAnd, Left: lower[0], Right: upper[0]}
}

// TestBoundMetricsPerTraceSubqueriesBoundsEveryPerTraceScan pins that every
// spans scan behind a per-trace subquery — each lookup and each membership
// subquery — takes the RangeWindow input bound widened by the metrics node's
// PerTraceLookback, that the plain row source each one filters takes the
// unwidened input bound exactly once, and that the input plan is left as it
// was.
func TestBoundMetricsPerTraceSubqueriesBoundsEveryPerTraceScan(t *testing.T) {
	t.Parallel()
	cases := map[string]func() Node{
		"lookup": func() Node {
			return &MetricsAggregate{Inner: perTraceTestLookupFilter(), PerTraceLookback: perTraceTestLookback}
		},
		"membership": func() Node {
			return &MetricsAggregate{
				Inner:            &Filter{Input: perTraceTestScan(), Predicate: perTraceTestCohort()},
				PerTraceLookback: perTraceTestLookback,
			}
		},
		// A membership conjunct below the join, as traceScopedFilter places
		// one: the shared plain row source is bounded once, not twice.
		"lookup_over_membership": func() Node {
			lookup := perTraceTestLookupFilter()
			lookup.Input = &Filter{Input: perTraceTestScan(), Predicate: perTraceTestCohort()}
			return &MetricsAggregate{Inner: lookup, PerTraceLookback: perTraceTestLookback}
		},
		"histogram": func() Node {
			return &MetricsHistogramOverTime{Inner: perTraceTestLookupFilter(), PerTraceLookback: perTraceTestLookback}
		},
		"compare_selection": func() Node {
			return &MetricsCompare{Inner: perTraceTestScan(), Selection: perTraceTestCohort(), PerTraceLookback: perTraceTestLookback}
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
			const tsCol = "Timestamp"
			rows := RangeWindowInputBound(rw, tsCol)
			perTrace := PerTraceWindowBound(rw, tsCol, perTraceTestLookback)
			if rows.Equal(perTrace) {
				t.Fatal("the widened bound equals the input bound; the case cannot tell them apart")
			}
			var scans []scanBound
			collectScanBounds(bounded.Input, false, tsCol, &scans)
			// A compare() Inner that carries no per-trace subquery of its
			// own is the RangeWindow emitter's to bound, not this pass's.
			_, isCompare := bounded.Input.(*MetricsCompare)
			var perTraceScans, rowScans int
			for i, sc := range scans {
				if isCompare && !sc.perTrace {
					continue
				}
				want := rows
				if sc.perTrace {
					want = perTrace
					perTraceScans++
				} else {
					rowScans++
				}
				if sc.bound == nil || !sc.bound.Equal(want) {
					t.Fatalf("scan %d (per-trace %v) carries bound %#v, want %#v", i, sc.perTrace, sc.bound, want)
				}
			}
			if perTraceScans == 0 {
				t.Fatal("no per-trace scan reached")
			}
			if !isCompare && rowScans == 0 {
				t.Fatal("no plain row source reached")
			}
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
	out, ok := WindowPerTraceSubqueries(lookup, window, window).(*TraceLookupFilter)
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
	if got := WindowPerTraceSubqueries(plain, window, window); got != Node(plain) {
		t.Fatalf("plain filter rewritten to %T", got)
	}
	lookup := perTraceTestLookupFilter()
	if got := WindowPerTraceSubqueries(lookup, nil, window); got != Node(lookup) {
		t.Fatalf("nil row window rewrote the lookup filter to %T", got)
	}
	if got := WindowPerTraceSubqueries(lookup, window, nil); got != Node(lookup) {
		t.Fatalf("nil per-trace window rewrote the lookup filter to %T", got)
	}
	windowless := perTraceTestWindow(&MetricsAggregate{Inner: perTraceTestLookupFilter()})
	windowless.Start = time.Time{}
	if got := BoundMetricsPerTraceSubqueries(windowless); got != Node(windowless) {
		t.Fatalf("windowless RangeWindow rewritten to %T", got)
	}
}

func isTsBound(e Expr, op BinaryOp, tsCol string) bool {
	b, ok := e.(*Binary)
	if !ok || b.Op != op {
		return false
	}
	col, ok := b.Left.(*ColumnRef)
	return ok && col.Name == tsCol
}

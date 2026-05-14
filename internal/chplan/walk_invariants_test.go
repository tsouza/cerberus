package chplan_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file adds Walk-invariant coverage for every Node type in chplan
// that has children. Each node-type test:
//
//   - builds a tree where each child is a Scan tagged with a unique
//     sentinel Table name;
//   - calls chplan.Walk(root, visit) collecting visited Scan tables;
//   - asserts every sentinel was visited exactly once.
//
// Scan has no children — covered separately as a no-op assertion.
//
// The recursion contract is defined by chplan.Walk in node.go: visit
// the node first (pre-order), then recurse into Children() left-to-right,
// honouring the false-skip signal returned by visit. The base
// TestWalk in node_test.go already covers the false-skip path; here we
// focus on coverage of every concrete Node's Children() shape.

// visitScans walks `root` and returns the list of Scan.Table sentinels
// it encountered, in pre-order traversal order. Sentinels uniquely
// identify each leaf so the test can assert visit count + order.
func visitScans(root chplan.Node) []string {
	var got []string
	chplan.Walk(root, func(n chplan.Node) bool {
		if s, ok := n.(*chplan.Scan); ok {
			got = append(got, s.Table)
		}
		return true
	})
	return got
}

// assertSentinels asserts the observed sentinel slice matches the
// expected one (order-sensitive); helper for short, focused failures.
func assertSentinels(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("visited %d sentinels, want %d (got=%v want=%v)", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Walk[%d] = %q, want %q (got=%v)", i, got[i], want[i], got)
		}
	}
}

func TestScan_Walk_NoChildren(t *testing.T) {
	t.Parallel()
	root := &chplan.Scan{Table: "sentinel"}
	kids := root.Children()
	if kids != nil {
		t.Errorf("Scan.Children() must be nil, got %v", kids)
	}
	got := visitScans(root)
	assertSentinels(t, got, []string{"sentinel"})
}

// TestOneRow_Walk_NoChildren — OneRow is a leaf like Scan; it carries
// no inputs because the `SELECT 1` emission doesn't consume rows from
// anywhere. The Walk traversal must report zero Scan sentinels.
func TestOneRow_Walk_NoChildren(t *testing.T) {
	t.Parallel()
	root := &chplan.OneRow{}
	kids := root.Children()
	if kids != nil {
		t.Errorf("OneRow.Children() must be nil, got %v", kids)
	}
	got := visitScans(root)
	assertSentinels(t, got, nil)
}

func TestFilter_Walk_VisitsInput(t *testing.T) {
	t.Parallel()
	root := &chplan.Filter{
		Input:     &chplan.Scan{Table: "filter_input"},
		Predicate: &chplan.LitBool{V: true},
	}
	assertSentinels(t, visitScans(root), []string{"filter_input"})
}

func TestProject_Walk_VisitsInput(t *testing.T) {
	t.Parallel()
	root := &chplan.Project{
		Input:       &chplan.Scan{Table: "project_input"},
		Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "x"}}},
	}
	assertSentinels(t, visitScans(root), []string{"project_input"})
}

func TestAggregate_Walk_VisitsInput(t *testing.T) {
	t.Parallel()
	root := &chplan.Aggregate{
		Input:   &chplan.Scan{Table: "agg_input"},
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Job"}},
	}
	assertSentinels(t, visitScans(root), []string{"agg_input"})
}

func TestRangeWindow_Walk_VisitsInput(t *testing.T) {
	t.Parallel()
	root := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "rw_input"},
		Func:            "rate",
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	assertSentinels(t, visitScans(root), []string{"rw_input"})
}

func TestLimit_Walk_VisitsInput(t *testing.T) {
	t.Parallel()
	root := &chplan.Limit{Input: &chplan.Scan{Table: "limit_input"}, Count: 10}
	assertSentinels(t, visitScans(root), []string{"limit_input"})
}

func TestOrderBy_Walk_VisitsInput(t *testing.T) {
	t.Parallel()
	root := &chplan.OrderBy{
		Input: &chplan.Scan{Table: "ob_input"},
		Keys:  []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "TS"}, Desc: true}},
	}
	assertSentinels(t, visitScans(root), []string{"ob_input"})
}

func TestHistogramQuantile_Walk_VisitsInput(t *testing.T) {
	t.Parallel()
	root := &chplan.HistogramQuantile{
		Input: &chplan.Scan{Table: "hq_input"},
		Phi:   0.95,
	}
	assertSentinels(t, visitScans(root), []string{"hq_input"})
}

func TestHistogramQuantileNative_Walk_VisitsInput(t *testing.T) {
	t.Parallel()
	root := &chplan.HistogramQuantileNative{
		Input: &chplan.Scan{Table: "hqn_input"},
		Phi:   0.99,
	}
	assertSentinels(t, visitScans(root), []string{"hqn_input"})
}

func TestMetricsAggregate_Walk_VisitsInner(t *testing.T) {
	t.Parallel()
	root := &chplan.MetricsAggregate{
		Op:    chplan.MetricsOpRate,
		Inner: &chplan.Scan{Table: "ma_inner"},
	}
	assertSentinels(t, visitScans(root), []string{"ma_inner"})
}

func TestMetricsHistogramOverTime_Walk_VisitsInner(t *testing.T) {
	t.Parallel()
	root := &chplan.MetricsHistogramOverTime{
		Attr:  &chplan.ColumnRef{Name: "Duration"},
		Inner: &chplan.Scan{Table: "mhot_inner"},
	}
	assertSentinels(t, visitScans(root), []string{"mhot_inner"})
}

func TestSetOperation_Walk_VisitsBothSides(t *testing.T) {
	t.Parallel()
	root := &chplan.SetOperation{
		Left:  &chplan.Scan{Table: "setop_left"},
		Right: &chplan.Scan{Table: "setop_right"},
		Op:    chplan.SetIntersect,
	}
	// Pre-order: Left first, Right second.
	assertSentinels(t, visitScans(root), []string{"setop_left", "setop_right"})
}

func TestStructuralJoin_Walk_VisitsBothSides(t *testing.T) {
	t.Parallel()
	root := &chplan.StructuralJoin{
		Left:  &chplan.Scan{Table: "sj_left"},
		Right: &chplan.Scan{Table: "sj_right"},
		Op:    chplan.StructuralChild,
	}
	assertSentinels(t, visitScans(root), []string{"sj_left", "sj_right"})
}

func TestVectorJoin_Walk_VisitsBothSides(t *testing.T) {
	t.Parallel()
	root := &chplan.VectorJoin{
		Left:  &chplan.Scan{Table: "vj_left"},
		Right: &chplan.Scan{Table: "vj_right"},
		Op:    chplan.OpAdd,
	}
	assertSentinels(t, visitScans(root), []string{"vj_left", "vj_right"})
}

// TestWalk_VisitCountExactlyOnce — for a multi-layer tree, every node
// must be visited exactly once. Stress-tests both the depth-first order
// and the no-duplicate-visits property. Each sentinel appears once in
// the tree so the observed list must be a unique-element pre-order
// traversal.
func TestWalk_VisitCountExactlyOnce(t *testing.T) {
	t.Parallel()
	root := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input: &chplan.Project{
				Input: &chplan.RangeWindow{
					Input:           &chplan.Scan{Table: "leaf"},
					Func:            "rate",
					TimestampColumn: "TimeUnix",
					ValueColumn:     "Value",
				},
			},
		},
		Predicate: &chplan.LitBool{V: true},
	}
	counts := map[string]int{
		"Filter":      0,
		"Aggregate":   0,
		"Project":     0,
		"RangeWindow": 0,
		"Scan":        0,
	}
	chplan.Walk(root, func(n chplan.Node) bool {
		switch n.(type) {
		case *chplan.Filter:
			counts["Filter"]++
		case *chplan.Aggregate:
			counts["Aggregate"]++
		case *chplan.Project:
			counts["Project"]++
		case *chplan.RangeWindow:
			counts["RangeWindow"]++
		case *chplan.Scan:
			counts["Scan"]++
		}
		return true
	})
	for name, n := range counts {
		if n != 1 {
			t.Errorf("Walk visited %s %d times, want exactly 1", name, n)
		}
	}
}

// TestWalk_NilRoot — Walk on a nil root must not panic and must invoke
// the visit callback zero times.
func TestWalk_NilRoot(t *testing.T) {
	t.Parallel()
	calls := 0
	chplan.Walk(nil, func(chplan.Node) bool {
		calls++
		return true
	})
	if calls != 0 {
		t.Errorf("Walk(nil): visit called %d times, want 0", calls)
	}
}

// TestSetOperation_Walk_DeepSentinels — confirm Walk recurses through
// both halves of a two-sided join when the children themselves have
// children (not just leaf Scans). Catches a bug where the visitor
// short-circuits on the first non-Scan layer.
func TestSetOperation_Walk_DeepSentinels(t *testing.T) {
	t.Parallel()
	root := &chplan.SetOperation{
		Left:  &chplan.Filter{Input: &chplan.Scan{Table: "deep_left"}, Predicate: &chplan.LitBool{V: true}},
		Right: &chplan.Filter{Input: &chplan.Scan{Table: "deep_right"}, Predicate: &chplan.LitBool{V: true}},
		Op:    chplan.SetUnion,
	}
	assertSentinels(t, visitScans(root), []string{"deep_left", "deep_right"})
}

// TestStructuralJoin_Walk_DeepSentinels — same property for StructuralJoin.
func TestStructuralJoin_Walk_DeepSentinels(t *testing.T) {
	t.Parallel()
	root := &chplan.StructuralJoin{
		Left:  &chplan.Filter{Input: &chplan.Scan{Table: "sj_deep_left"}, Predicate: &chplan.LitBool{V: true}},
		Right: &chplan.Filter{Input: &chplan.Scan{Table: "sj_deep_right"}, Predicate: &chplan.LitBool{V: true}},
		Op:    chplan.StructuralDescendant,
	}
	assertSentinels(t, visitScans(root), []string{"sj_deep_left", "sj_deep_right"})
}

// TestVectorJoin_Walk_DeepSentinels — same property for VectorJoin.
func TestVectorJoin_Walk_DeepSentinels(t *testing.T) {
	t.Parallel()
	root := &chplan.VectorJoin{
		Left:  &chplan.Filter{Input: &chplan.Scan{Table: "vj_deep_left"}, Predicate: &chplan.LitBool{V: true}},
		Right: &chplan.Filter{Input: &chplan.Scan{Table: "vj_deep_right"}, Predicate: &chplan.LitBool{V: true}},
		Op:    chplan.OpMul,
	}
	assertSentinels(t, visitScans(root), []string{"vj_deep_left", "vj_deep_right"})
}

// TestChildren_FilterReturnsExactlyInput — every node's Children() must
// return precisely the Node pointers held by load-bearing fields.
// Pointer identity, not Equal, is the contract — the optimizer needs to
// rewrite specific instances. These tests are intentionally short.
func TestChildren_FilterReturnsExactlyInput(t *testing.T) {
	t.Parallel()
	input := &chplan.Scan{Table: "t"}
	f := &chplan.Filter{Input: input, Predicate: &chplan.LitBool{V: true}}
	kids := f.Children()
	if len(kids) != 1 || kids[0] != input {
		t.Errorf("Filter.Children() should return [Input], got %v", kids)
	}
}

func TestChildren_ProjectReturnsExactlyInput(t *testing.T) {
	t.Parallel()
	input := &chplan.Scan{Table: "t"}
	p := &chplan.Project{Input: input}
	kids := p.Children()
	if len(kids) != 1 || kids[0] != input {
		t.Errorf("Project.Children() should return [Input], got %v", kids)
	}
}

func TestChildren_AggregateReturnsExactlyInput(t *testing.T) {
	t.Parallel()
	input := &chplan.Scan{Table: "t"}
	a := &chplan.Aggregate{Input: input}
	kids := a.Children()
	if len(kids) != 1 || kids[0] != input {
		t.Errorf("Aggregate.Children() should return [Input], got %v", kids)
	}
}

func TestChildren_RangeWindowReturnsExactlyInput(t *testing.T) {
	t.Parallel()
	input := &chplan.Scan{Table: "t"}
	r := &chplan.RangeWindow{Input: input, Func: "rate"}
	kids := r.Children()
	if len(kids) != 1 || kids[0] != input {
		t.Errorf("RangeWindow.Children() should return [Input], got %v", kids)
	}
}

func TestChildren_LimitReturnsExactlyInput(t *testing.T) {
	t.Parallel()
	input := &chplan.Scan{Table: "t"}
	l := &chplan.Limit{Input: input, Count: 1}
	kids := l.Children()
	if len(kids) != 1 || kids[0] != input {
		t.Errorf("Limit.Children() should return [Input], got %v", kids)
	}
}

func TestChildren_OrderByReturnsExactlyInput(t *testing.T) {
	t.Parallel()
	input := &chplan.Scan{Table: "t"}
	o := &chplan.OrderBy{Input: input}
	kids := o.Children()
	if len(kids) != 1 || kids[0] != input {
		t.Errorf("OrderBy.Children() should return [Input], got %v", kids)
	}
}

func TestChildren_HistogramQuantileReturnsExactlyInput(t *testing.T) {
	t.Parallel()
	input := &chplan.Scan{Table: "t"}
	h := &chplan.HistogramQuantile{Input: input}
	kids := h.Children()
	if len(kids) != 1 || kids[0] != input {
		t.Errorf("HistogramQuantile.Children() should return [Input], got %v", kids)
	}
}

func TestChildren_HistogramQuantileNativeReturnsExactlyInput(t *testing.T) {
	t.Parallel()
	input := &chplan.Scan{Table: "t"}
	h := &chplan.HistogramQuantileNative{Input: input}
	kids := h.Children()
	if len(kids) != 1 || kids[0] != input {
		t.Errorf("HistogramQuantileNative.Children() should return [Input], got %v", kids)
	}
}

func TestChildren_MetricsAggregateReturnsExactlyInner(t *testing.T) {
	t.Parallel()
	inner := &chplan.Scan{Table: "t"}
	m := &chplan.MetricsAggregate{Op: chplan.MetricsOpRate, Inner: inner}
	kids := m.Children()
	if len(kids) != 1 || kids[0] != inner {
		t.Errorf("MetricsAggregate.Children() should return [Inner], got %v", kids)
	}
}

func TestChildren_MetricsHistogramOverTimeReturnsExactlyInner(t *testing.T) {
	t.Parallel()
	inner := &chplan.Scan{Table: "t"}
	m := &chplan.MetricsHistogramOverTime{Attr: &chplan.ColumnRef{Name: "X"}, Inner: inner}
	kids := m.Children()
	if len(kids) != 1 || kids[0] != inner {
		t.Errorf("MetricsHistogramOverTime.Children() should return [Inner], got %v", kids)
	}
}

func TestChildren_SetOperationReturnsLeftThenRight(t *testing.T) {
	t.Parallel()
	left := &chplan.Scan{Table: "l"}
	right := &chplan.Scan{Table: "r"}
	s := &chplan.SetOperation{Left: left, Right: right, Op: chplan.SetIntersect}
	kids := s.Children()
	if len(kids) != 2 || kids[0] != left || kids[1] != right {
		t.Errorf("SetOperation.Children() should return [Left, Right], got %v", kids)
	}
}

func TestChildren_StructuralJoinReturnsLeftThenRight(t *testing.T) {
	t.Parallel()
	left := &chplan.Scan{Table: "l"}
	right := &chplan.Scan{Table: "r"}
	j := &chplan.StructuralJoin{Left: left, Right: right, Op: chplan.StructuralChild}
	kids := j.Children()
	if len(kids) != 2 || kids[0] != left || kids[1] != right {
		t.Errorf("StructuralJoin.Children() should return [Left, Right], got %v", kids)
	}
}

func TestChildren_VectorJoinReturnsLeftThenRight(t *testing.T) {
	t.Parallel()
	left := &chplan.Scan{Table: "l"}
	right := &chplan.Scan{Table: "r"}
	v := &chplan.VectorJoin{Left: left, Right: right, Op: chplan.OpAdd}
	kids := v.Children()
	if len(kids) != 2 || kids[0] != left || kids[1] != right {
		t.Errorf("VectorJoin.Children() should return [Left, Right], got %v", kids)
	}
}

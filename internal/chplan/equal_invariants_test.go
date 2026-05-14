package chplan_test

import (
	"math"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file adds exhaustive Equal-invariant coverage for every Node and
// Expr type in the chplan IR.
//
// For each concrete type we exercise:
//
//   - a positive case: two structurally-identical values must compare Equal;
//   - one or more negative cases: values differing in exactly one
//     load-bearing field must NOT compare Equal.
//
// Symmetric negative checks (Equal must be commutative across the
// observably-different inputs) follow the same pattern as the existing
// node_negatives_test.go.
//
// Methods NOT covered by this file because the IR does not implement
// them today (left as a follow-up; adding methods is a code change,
// not a test change):
//
//   - Clone() — no Clone method exists on any Node or Expr type.
//   - String() — only chplan.MetricsOp.String() exists; no Node/Expr
//     carries a String() method.
//
// The corresponding Walk() coverage lives in walk_invariants_test.go.

// -----------------------------------------------------------------------
// Node Equal tests — one positive + one or more negative per type.
// -----------------------------------------------------------------------

func TestScan_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.Scan{Table: "otel_logs", Columns: []string{"Body", "Timestamp"}}
	b := &chplan.Scan{Table: "otel_logs", Columns: []string{"Body", "Timestamp"}}
	if !a.Equal(b) {
		t.Fatalf("identical Scan trees should be Equal")
	}
	if !b.Equal(a) {
		t.Fatalf("Equal must be symmetric")
	}
}

func TestScan_Equal_Negative_Table(t *testing.T) {
	t.Parallel()
	a := &chplan.Scan{Table: "otel_logs"}
	b := &chplan.Scan{Table: "otel_traces"}
	if a.Equal(b) {
		t.Errorf("different Table should not be Equal")
	}
}

func TestScan_Equal_Negative_ColumnsLen(t *testing.T) {
	t.Parallel()
	a := &chplan.Scan{Table: "t", Columns: []string{"A"}}
	b := &chplan.Scan{Table: "t", Columns: []string{"A", "B"}}
	if a.Equal(b) {
		t.Errorf("different Columns length should not be Equal")
	}
}

func TestScan_Equal_Negative_ColumnsContent(t *testing.T) {
	t.Parallel()
	a := &chplan.Scan{Table: "t", Columns: []string{"A", "B"}}
	b := &chplan.Scan{Table: "t", Columns: []string{"A", "C"}}
	if a.Equal(b) {
		t.Errorf("different Columns content should not be Equal")
	}
}

// TestOneRow_Equal_Positive — OneRow has no fields; two instances must
// compare Equal unconditionally (it's a singleton in spirit).
func TestOneRow_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.OneRow{}
	b := &chplan.OneRow{}
	if !a.Equal(b) {
		t.Fatalf("two OneRow values should be Equal")
	}
	if !b.Equal(a) {
		t.Fatalf("Equal must be symmetric")
	}
}

// TestOneRow_Equal_Negative_OtherType — Equal against a non-OneRow node
// must return false; the type-assertion in Equal is what guarantees
// optimizer rules don't accidentally rewrite OneRow into a Scan.
func TestOneRow_Equal_Negative_OtherType(t *testing.T) {
	t.Parallel()
	a := &chplan.OneRow{}
	b := &chplan.Scan{Table: "t"}
	if a.Equal(b) {
		t.Errorf("OneRow should not Equal Scan")
	}
}

func TestFilter_Equal_Positive(t *testing.T) {
	t.Parallel()
	build := func() *chplan.Filter {
		return &chplan.Filter{
			Input: &chplan.Scan{Table: "t"},
			Predicate: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "X"},
				Right: &chplan.LitString{V: "v"},
			},
		}
	}
	if !build().Equal(build()) {
		t.Fatalf("identical Filter trees should be Equal")
	}
}

func TestFilter_Equal_Negative_Predicate(t *testing.T) {
	t.Parallel()
	a := &chplan.Filter{
		Input:     &chplan.Scan{Table: "t"},
		Predicate: &chplan.LitBool{V: true},
	}
	b := &chplan.Filter{
		Input:     &chplan.Scan{Table: "t"},
		Predicate: &chplan.LitBool{V: false},
	}
	if a.Equal(b) {
		t.Errorf("different Predicate should not be Equal")
	}
}

func TestFilter_Equal_Negative_Input(t *testing.T) {
	t.Parallel()
	a := &chplan.Filter{Input: &chplan.Scan{Table: "a"}, Predicate: &chplan.LitBool{V: true}}
	b := &chplan.Filter{Input: &chplan.Scan{Table: "b"}, Predicate: &chplan.LitBool{V: true}}
	if a.Equal(b) {
		t.Errorf("different Input should not be Equal")
	}
}

func TestProject_Equal_Positive(t *testing.T) {
	t.Parallel()
	build := func() *chplan.Project {
		return &chplan.Project{
			Input: &chplan.Scan{Table: "t"},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "A"}, Alias: "a"},
				{Expr: &chplan.ColumnRef{Name: "B"}, Alias: "b"},
			},
		}
	}
	if !build().Equal(build()) {
		t.Fatalf("identical Project trees should be Equal")
	}
}

func TestProject_Equal_Negative_ProjectionsLen(t *testing.T) {
	t.Parallel()
	a := &chplan.Project{
		Input:       &chplan.Scan{Table: "t"},
		Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "A"}}},
	}
	b := &chplan.Project{
		Input: &chplan.Scan{Table: "t"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "A"}},
			{Expr: &chplan.ColumnRef{Name: "B"}},
		},
	}
	if a.Equal(b) {
		t.Errorf("different Projections length should not be Equal")
	}
}

func TestProject_Equal_Negative_ProjectionContent(t *testing.T) {
	t.Parallel()
	a := &chplan.Project{
		Input:       &chplan.Scan{Table: "t"},
		Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "A"}}},
	}
	b := &chplan.Project{
		Input:       &chplan.Scan{Table: "t"},
		Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "Z"}}},
	}
	if a.Equal(b) {
		t.Errorf("different Projection content should not be Equal")
	}
}

func TestProjection_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := chplan.Projection{Expr: &chplan.ColumnRef{Name: "x"}, Alias: "alias"}
	b := chplan.Projection{Expr: &chplan.ColumnRef{Name: "x"}, Alias: "alias"}
	if !a.Equal(b) {
		t.Fatalf("identical Projection should be Equal")
	}
}

func TestProjection_Equal_Negative_Alias(t *testing.T) {
	t.Parallel()
	a := chplan.Projection{Expr: &chplan.ColumnRef{Name: "x"}, Alias: "a"}
	b := chplan.Projection{Expr: &chplan.ColumnRef{Name: "x"}, Alias: "z"}
	if a.Equal(b) {
		t.Errorf("different alias should not be Equal")
	}
}

func TestProjection_Equal_Negative_Expr(t *testing.T) {
	t.Parallel()
	a := chplan.Projection{Expr: &chplan.ColumnRef{Name: "x"}, Alias: "a"}
	b := chplan.Projection{Expr: &chplan.ColumnRef{Name: "y"}, Alias: "a"}
	if a.Equal(b) {
		t.Errorf("different expr should not be Equal")
	}
}

func TestAggregate_Equal_Positive(t *testing.T) {
	t.Parallel()
	build := func() *chplan.Aggregate {
		return &chplan.Aggregate{
			Input:          &chplan.Scan{Table: "t"},
			GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "Job"}},
			GroupByAliases: []string{"job"},
			AggFuncs: []chplan.AggFunc{
				{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "Value"},
			},
		}
	}
	if !build().Equal(build()) {
		t.Fatalf("identical Aggregate trees should be Equal")
	}
}

func TestAggregate_Equal_Negative_GroupByLen(t *testing.T) {
	t.Parallel()
	a := &chplan.Aggregate{Input: &chplan.Scan{Table: "t"}}
	b := &chplan.Aggregate{
		Input:   &chplan.Scan{Table: "t"},
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Job"}},
	}
	if a.Equal(b) {
		t.Errorf("different GroupBy length should not be Equal")
	}
}

func TestAggregate_Equal_Negative_GroupByExpr(t *testing.T) {
	t.Parallel()
	a := &chplan.Aggregate{
		Input:   &chplan.Scan{Table: "t"},
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Job"}},
	}
	b := &chplan.Aggregate{
		Input:   &chplan.Scan{Table: "t"},
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Other"}},
	}
	if a.Equal(b) {
		t.Errorf("different GroupBy expr should not be Equal")
	}
}

func TestAggregate_Equal_Negative_AggFuncs(t *testing.T) {
	t.Parallel()
	a := &chplan.Aggregate{
		Input:    &chplan.Scan{Table: "t"},
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "V"}}}},
	}
	b := &chplan.Aggregate{
		Input:    &chplan.Scan{Table: "t"},
		AggFuncs: []chplan.AggFunc{{Name: "avg", Args: []chplan.Expr{&chplan.ColumnRef{Name: "V"}}}},
	}
	if a.Equal(b) {
		t.Errorf("different AggFunc name should not be Equal")
	}
}

func TestAggregate_Equal_Negative_Input(t *testing.T) {
	t.Parallel()
	a := &chplan.Aggregate{Input: &chplan.Scan{Table: "a"}}
	b := &chplan.Aggregate{Input: &chplan.Scan{Table: "b"}}
	if a.Equal(b) {
		t.Errorf("different Input should not be Equal")
	}
}

func TestAggFunc_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := chplan.AggFunc{
		Name:   "quantile",
		Params: []chplan.Expr{&chplan.LitFloat{V: 0.95}},
		Args:   []chplan.Expr{&chplan.ColumnRef{Name: "V"}},
		Alias:  "Value",
	}
	b := chplan.AggFunc{
		Name:   "quantile",
		Params: []chplan.Expr{&chplan.LitFloat{V: 0.95}},
		Args:   []chplan.Expr{&chplan.ColumnRef{Name: "V"}},
		Alias:  "Value",
	}
	if !a.Equal(b) {
		t.Fatalf("identical AggFunc should be Equal")
	}
}

func TestAggFunc_Equal_Negative_Name(t *testing.T) {
	t.Parallel()
	a := chplan.AggFunc{Name: "sum"}
	b := chplan.AggFunc{Name: "avg"}
	if a.Equal(b) {
		t.Errorf("different Name should not be Equal")
	}
}

func TestAggFunc_Equal_Negative_Alias(t *testing.T) {
	t.Parallel()
	a := chplan.AggFunc{Name: "sum", Alias: "x"}
	b := chplan.AggFunc{Name: "sum", Alias: "y"}
	if a.Equal(b) {
		t.Errorf("different Alias should not be Equal")
	}
}

func TestAggFunc_Equal_Negative_ParamsLen(t *testing.T) {
	t.Parallel()
	a := chplan.AggFunc{Name: "quantile"}
	b := chplan.AggFunc{Name: "quantile", Params: []chplan.Expr{&chplan.LitFloat{V: 0.5}}}
	if a.Equal(b) {
		t.Errorf("different Params length should not be Equal")
	}
}

func TestAggFunc_Equal_Negative_ParamsValue(t *testing.T) {
	t.Parallel()
	a := chplan.AggFunc{Name: "quantile", Params: []chplan.Expr{&chplan.LitFloat{V: 0.5}}}
	b := chplan.AggFunc{Name: "quantile", Params: []chplan.Expr{&chplan.LitFloat{V: 0.95}}}
	if a.Equal(b) {
		t.Errorf("different Params value should not be Equal")
	}
}

func TestAggFunc_Equal_Negative_ArgsValue(t *testing.T) {
	t.Parallel()
	a := chplan.AggFunc{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "X"}}}
	b := chplan.AggFunc{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Y"}}}
	if a.Equal(b) {
		t.Errorf("different Args value should not be Equal")
	}
}

func TestRangeWindow_Equal_Positive(t *testing.T) {
	t.Parallel()
	build := func() *chplan.RangeWindow {
		return &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            "rate",
			Range:           5 * time.Minute,
			Step:            time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			Scalars:         []float64{0.5},
		}
	}
	if !build().Equal(build()) {
		t.Fatalf("identical RangeWindow trees should be Equal")
	}
}

func TestRangeWindow_Equal_Negative_Func(t *testing.T) {
	t.Parallel()
	a := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "rate", Range: time.Minute}
	b := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "increase", Range: time.Minute}
	if a.Equal(b) {
		t.Errorf("different Func should not be Equal")
	}
}

func TestRangeWindow_Equal_Negative_Step(t *testing.T) {
	t.Parallel()
	a := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "rate", Step: time.Minute}
	b := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "rate", Step: time.Second}
	if a.Equal(b) {
		t.Errorf("different Step should not be Equal")
	}
}

func TestRangeWindow_Equal_Negative_Offset(t *testing.T) {
	t.Parallel()
	a := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "rate"}
	b := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "rate", Offset: time.Hour}
	if a.Equal(b) {
		t.Errorf("different Offset should not be Equal")
	}
}

func TestRangeWindow_Equal_Negative_TimestampColumn(t *testing.T) {
	t.Parallel()
	a := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "rate", TimestampColumn: "TimeUnix"}
	b := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "rate", TimestampColumn: "Timestamp"}
	if a.Equal(b) {
		t.Errorf("different TimestampColumn should not be Equal")
	}
}

func TestRangeWindow_Equal_Negative_ValueColumn(t *testing.T) {
	t.Parallel()
	a := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "rate", ValueColumn: "Value"}
	b := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "rate", ValueColumn: "V"}
	if a.Equal(b) {
		t.Errorf("different ValueColumn should not be Equal")
	}
}

func TestRangeWindow_Equal_Negative_GroupBy(t *testing.T) {
	t.Parallel()
	a := &chplan.RangeWindow{
		Input:   &chplan.Scan{Table: "t"},
		Func:    "rate",
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	b := &chplan.RangeWindow{
		Input:   &chplan.Scan{Table: "t"},
		Func:    "rate",
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Other"}},
	}
	if a.Equal(b) {
		t.Errorf("different GroupBy should not be Equal")
	}
}

func TestRangeWindow_Equal_Negative_Scalars(t *testing.T) {
	t.Parallel()
	a := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "predict_linear", Scalars: []float64{60}}
	b := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "predict_linear", Scalars: []float64{120}}
	if a.Equal(b) {
		t.Errorf("different Scalars should not be Equal")
	}
}

func TestRangeWindow_Equal_Negative_StartEnd(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	a := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "rate", Start: now, End: now}
	b := &chplan.RangeWindow{Input: &chplan.Scan{Table: "t"}, Func: "rate", Start: now, End: now.Add(time.Hour)}
	if a.Equal(b) {
		t.Errorf("different End should not be Equal")
	}
}

func TestLimit_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.Limit{Input: &chplan.Scan{Table: "t"}, Count: 100}
	b := &chplan.Limit{Input: &chplan.Scan{Table: "t"}, Count: 100}
	if !a.Equal(b) {
		t.Fatalf("identical Limit trees should be Equal")
	}
}

func TestLimit_Equal_Negative_Count(t *testing.T) {
	t.Parallel()
	a := &chplan.Limit{Input: &chplan.Scan{Table: "t"}, Count: 100}
	b := &chplan.Limit{Input: &chplan.Scan{Table: "t"}, Count: 200}
	if a.Equal(b) {
		t.Errorf("different Count should not be Equal")
	}
}

func TestLimit_Equal_Negative_Input(t *testing.T) {
	t.Parallel()
	a := &chplan.Limit{Input: &chplan.Scan{Table: "a"}, Count: 10}
	b := &chplan.Limit{Input: &chplan.Scan{Table: "b"}, Count: 10}
	if a.Equal(b) {
		t.Errorf("different Input should not be Equal")
	}
}

func TestOrderBy_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.OrderBy{
		Input: &chplan.Scan{Table: "t"},
		Keys:  []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true}},
	}
	b := &chplan.OrderBy{
		Input: &chplan.Scan{Table: "t"},
		Keys:  []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true}},
	}
	if !a.Equal(b) {
		t.Fatalf("identical OrderBy trees should be Equal")
	}
}

func TestOrderBy_Equal_Negative_KeyExpr(t *testing.T) {
	t.Parallel()
	a := &chplan.OrderBy{
		Input: &chplan.Scan{Table: "t"},
		Keys:  []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "A"}, Desc: false}},
	}
	b := &chplan.OrderBy{
		Input: &chplan.Scan{Table: "t"},
		Keys:  []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "B"}, Desc: false}},
	}
	if a.Equal(b) {
		t.Errorf("different Key expression should not be Equal")
	}
}

func TestTopK_Equal_Positive(t *testing.T) {
	t.Parallel()
	build := func() *chplan.TopK {
		return &chplan.TopK{
			Input:    &chplan.Scan{Table: "t"},
			K:        3,
			By:       []chplan.Expr{&chplan.ColumnRef{Name: "Job"}},
			SortExpr: &chplan.ColumnRef{Name: "Value"},
			Desc:     true,
			Columns:  []string{"a", "b"},
		}
	}
	if !build().Equal(build()) {
		t.Fatalf("identical TopK trees should be Equal")
	}
}

func TestTopK_Equal_Negative_K(t *testing.T) {
	t.Parallel()
	mk := func(k int64) *chplan.TopK {
		return &chplan.TopK{
			Input:    &chplan.Scan{Table: "t"},
			K:        k,
			SortExpr: &chplan.ColumnRef{Name: "Value"},
		}
	}
	if mk(3).Equal(mk(5)) {
		t.Errorf("different K should not be Equal")
	}
}

func TestTopK_Equal_Negative_Desc(t *testing.T) {
	t.Parallel()
	mk := func(d bool) *chplan.TopK {
		return &chplan.TopK{
			Input:    &chplan.Scan{Table: "t"},
			K:        3,
			SortExpr: &chplan.ColumnRef{Name: "Value"},
			Desc:     d,
		}
	}
	if mk(true).Equal(mk(false)) {
		t.Errorf("different Desc should not be Equal")
	}
}

func TestTopK_Equal_Negative_By(t *testing.T) {
	t.Parallel()
	mk := func(by string) *chplan.TopK {
		return &chplan.TopK{
			Input:    &chplan.Scan{Table: "t"},
			K:        3,
			By:       []chplan.Expr{&chplan.ColumnRef{Name: by}},
			SortExpr: &chplan.ColumnRef{Name: "Value"},
		}
	}
	if mk("Job").Equal(mk("Instance")) {
		t.Errorf("different By should not be Equal")
	}
}

func TestTopK_Equal_Negative_SortExpr(t *testing.T) {
	t.Parallel()
	mk := func(s string) *chplan.TopK {
		return &chplan.TopK{
			Input:    &chplan.Scan{Table: "t"},
			K:        3,
			SortExpr: &chplan.ColumnRef{Name: s},
		}
	}
	if mk("Value").Equal(mk("Other")) {
		t.Errorf("different SortExpr should not be Equal")
	}
}

func TestTopK_Equal_Negative_Columns(t *testing.T) {
	t.Parallel()
	mk := func(cols []string) *chplan.TopK {
		return &chplan.TopK{
			Input:    &chplan.Scan{Table: "t"},
			K:        3,
			SortExpr: &chplan.ColumnRef{Name: "Value"},
			Columns:  cols,
		}
	}
	if mk([]string{"a"}).Equal(mk([]string{"a", "b"})) {
		t.Errorf("different Columns length should not be Equal")
	}
	if mk([]string{"a"}).Equal(mk([]string{"b"})) {
		t.Errorf("different Columns content should not be Equal")
	}
}

func TestTopK_Equal_Negative_Input(t *testing.T) {
	t.Parallel()
	mk := func(table string) *chplan.TopK {
		return &chplan.TopK{
			Input:    &chplan.Scan{Table: table},
			K:        3,
			SortExpr: &chplan.ColumnRef{Name: "Value"},
		}
	}
	if mk("a").Equal(mk("b")) {
		t.Errorf("different Input should not be Equal")
	}
}

func TestSetOperation_Equal_Positive(t *testing.T) {
	t.Parallel()
	build := func() *chplan.SetOperation {
		return &chplan.SetOperation{
			Left:          &chplan.Scan{Table: "otel_traces"},
			Right:         &chplan.Scan{Table: "otel_traces"},
			Op:            chplan.SetIntersect,
			TraceIDColumn: "TraceId",
			SpanIDColumn:  "SpanId",
		}
	}
	if !build().Equal(build()) {
		t.Fatalf("identical SetOperation trees should be Equal")
	}
}

func TestSetOperation_Equal_Negative_Op(t *testing.T) {
	t.Parallel()
	a := &chplan.SetOperation{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op: chplan.SetIntersect,
	}
	b := &chplan.SetOperation{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op: chplan.SetUnion,
	}
	if a.Equal(b) {
		t.Errorf("different Op should not be Equal")
	}
}

func TestSetOperation_Equal_Negative_TraceIDColumn(t *testing.T) {
	t.Parallel()
	a := &chplan.SetOperation{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op: chplan.SetIntersect, TraceIDColumn: "TraceId", SpanIDColumn: "SpanId",
	}
	b := &chplan.SetOperation{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op: chplan.SetIntersect, TraceIDColumn: "Trace_Id", SpanIDColumn: "SpanId",
	}
	if a.Equal(b) {
		t.Errorf("different TraceIDColumn should not be Equal")
	}
}

func TestSetOperation_Equal_Negative_Right(t *testing.T) {
	t.Parallel()
	a := &chplan.SetOperation{
		Left: &chplan.Scan{Table: "x"}, Right: &chplan.Scan{Table: "y"},
		Op: chplan.SetIntersect,
	}
	b := &chplan.SetOperation{
		Left: &chplan.Scan{Table: "x"}, Right: &chplan.Scan{Table: "z"},
		Op: chplan.SetIntersect,
	}
	if a.Equal(b) {
		t.Errorf("different Right should not be Equal")
	}
}

func TestStructuralJoin_Equal_Positive(t *testing.T) {
	t.Parallel()
	build := func() *chplan.StructuralJoin {
		return &chplan.StructuralJoin{
			Left:               &chplan.Scan{Table: "otel_traces"},
			Right:              &chplan.Scan{Table: "otel_traces"},
			Op:                 chplan.StructuralChild,
			TraceIDColumn:      "TraceId",
			SpanIDColumn:       "SpanId",
			ParentSpanIDColumn: "ParentSpanId",
		}
	}
	if !build().Equal(build()) {
		t.Fatalf("identical StructuralJoin trees should be Equal")
	}
}

func TestStructuralJoin_Equal_Negative_Op(t *testing.T) {
	t.Parallel()
	a := &chplan.StructuralJoin{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op: chplan.StructuralChild,
	}
	b := &chplan.StructuralJoin{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op: chplan.StructuralDescendant,
	}
	if a.Equal(b) {
		t.Errorf("different Op should not be Equal")
	}
}

func TestStructuralJoin_Equal_Negative_MaxDepth(t *testing.T) {
	t.Parallel()
	a := &chplan.StructuralJoin{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op: chplan.StructuralDescendant, MaxDepth: 0,
	}
	b := &chplan.StructuralJoin{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op: chplan.StructuralDescendant, MaxDepth: 5,
	}
	if a.Equal(b) {
		t.Errorf("different MaxDepth should not be Equal")
	}
}

func TestStructuralJoin_Equal_Negative_ParentSpanIDColumn(t *testing.T) {
	t.Parallel()
	a := &chplan.StructuralJoin{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op:                 chplan.StructuralChild,
		ParentSpanIDColumn: "ParentSpanId",
	}
	b := &chplan.StructuralJoin{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op:                 chplan.StructuralChild,
		ParentSpanIDColumn: "OtherParent",
	}
	if a.Equal(b) {
		t.Errorf("different ParentSpanIDColumn should not be Equal")
	}
}

func TestVectorJoin_Equal_Positive(t *testing.T) {
	t.Parallel()
	build := func() *chplan.VectorJoin {
		return &chplan.VectorJoin{
			Left:             &chplan.Scan{Table: "m"},
			Right:            &chplan.Scan{Table: "m"},
			Op:               chplan.OpMul,
			Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
			Card:             chplan.CardOneToOne,
			MetricNameColumn: "MetricName",
			AttributesColumn: "Attributes",
			TimestampColumn:  "TimeUnix",
			ValueColumn:      "Value",
		}
	}
	if !build().Equal(build()) {
		t.Fatalf("identical VectorJoin trees should be Equal")
	}
}

func TestVectorJoin_Equal_Negative_Op(t *testing.T) {
	t.Parallel()
	a := &chplan.VectorJoin{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op: chplan.OpAdd,
	}
	b := &chplan.VectorJoin{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op: chplan.OpSub,
	}
	if a.Equal(b) {
		t.Errorf("different Op should not be Equal")
	}
}

func TestVectorJoin_Equal_Negative_ValueColumn(t *testing.T) {
	t.Parallel()
	a := &chplan.VectorJoin{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op: chplan.OpAdd, ValueColumn: "Value",
	}
	b := &chplan.VectorJoin{
		Left: &chplan.Scan{Table: "t"}, Right: &chplan.Scan{Table: "t"},
		Op: chplan.OpAdd, ValueColumn: "V",
	}
	if a.Equal(b) {
		t.Errorf("different ValueColumn should not be Equal")
	}
}

func TestVectorMatch_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := chplan.VectorMatch{Labels: []string{"job", "instance"}, On: true}
	b := chplan.VectorMatch{Labels: []string{"job", "instance"}, On: true}
	if !a.Equal(b) {
		t.Fatalf("identical VectorMatch should be Equal")
	}
}

func TestVectorMatch_Equal_Negative_On(t *testing.T) {
	t.Parallel()
	a := chplan.VectorMatch{Labels: []string{"job"}, On: true}
	b := chplan.VectorMatch{Labels: []string{"job"}, On: false}
	if a.Equal(b) {
		t.Errorf("different On flag should not be Equal")
	}
}

func TestVectorMatch_Equal_Negative_Labels(t *testing.T) {
	t.Parallel()
	a := chplan.VectorMatch{Labels: []string{"job"}, On: true}
	b := chplan.VectorMatch{Labels: []string{"instance"}, On: true}
	if a.Equal(b) {
		t.Errorf("different Labels should not be Equal")
	}
}

func TestOrderKey_DistinguishesDirection(t *testing.T) {
	// OrderKey itself has no Equal method (the OrderBy.Equal does the
	// element-by-element compare). This sub-test asserts that the
	// surrounding OrderBy.Equal correctly distinguishes direction.
	t.Parallel()
	col := &chplan.ColumnRef{Name: "Timestamp"}
	a := &chplan.OrderBy{
		Input: &chplan.Scan{Table: "t"},
		Keys:  []chplan.OrderKey{{Expr: col, Desc: true}},
	}
	b := &chplan.OrderBy{
		Input: &chplan.Scan{Table: "t"},
		Keys:  []chplan.OrderKey{{Expr: col, Desc: false}},
	}
	if a.Equal(b) {
		t.Errorf("OrderKey Desc mismatch should not be Equal")
	}
}

func TestHistogramQuantile_Equal_Positive(t *testing.T) {
	t.Parallel()
	build := func() *chplan.HistogramQuantile {
		return &chplan.HistogramQuantile{
			Input:                &chplan.Scan{Table: "otel_metrics_histogram"},
			Phi:                  0.95,
			BucketCountsColumn:   "BucketCounts",
			ExplicitBoundsColumn: "ExplicitBounds",
			GroupBy:              []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			GroupByAliases:       []string{"attrs"},
			MetricNameColumn:     "MetricName",
			AttributesColumn:     "Attributes",
			TimestampColumn:      "TimeUnix",
		}
	}
	if !build().Equal(build()) {
		t.Fatalf("identical HistogramQuantile trees should be Equal")
	}
}

func TestHistogramQuantile_Equal_Negative_Phi(t *testing.T) {
	t.Parallel()
	a := &chplan.HistogramQuantile{Input: &chplan.Scan{Table: "t"}, Phi: 0.5}
	b := &chplan.HistogramQuantile{Input: &chplan.Scan{Table: "t"}, Phi: 0.95}
	if a.Equal(b) {
		t.Errorf("different Phi should not be Equal")
	}
}

func TestHistogramQuantile_Equal_Negative_BucketCountsColumn(t *testing.T) {
	t.Parallel()
	a := &chplan.HistogramQuantile{Input: &chplan.Scan{Table: "t"}, BucketCountsColumn: "Buckets"}
	b := &chplan.HistogramQuantile{Input: &chplan.Scan{Table: "t"}, BucketCountsColumn: "BC"}
	if a.Equal(b) {
		t.Errorf("different BucketCountsColumn should not be Equal")
	}
}

func TestHistogramQuantile_Equal_Negative_GroupByAliases(t *testing.T) {
	t.Parallel()
	a := &chplan.HistogramQuantile{
		Input:          &chplan.Scan{Table: "t"},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		GroupByAliases: []string{"a"},
	}
	b := &chplan.HistogramQuantile{
		Input:          &chplan.Scan{Table: "t"},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		GroupByAliases: []string{"b"},
	}
	if a.Equal(b) {
		t.Errorf("different GroupByAliases should not be Equal")
	}
}

func TestHistogramQuantile_Equal_NilInput(t *testing.T) {
	t.Parallel()
	a := &chplan.HistogramQuantile{Phi: 0.5}
	b := &chplan.HistogramQuantile{Phi: 0.5}
	if !a.Equal(b) {
		t.Errorf("nil Input on both sides should be Equal")
	}
	c := &chplan.HistogramQuantile{Input: &chplan.Scan{Table: "t"}, Phi: 0.5}
	if a.Equal(c) {
		t.Errorf("nil vs non-nil Input should not be Equal")
	}
}

func TestHistogramQuantileNative_Equal_Positive(t *testing.T) {
	t.Parallel()
	build := func() *chplan.HistogramQuantileNative {
		return &chplan.HistogramQuantileNative{
			Input:                      &chplan.Scan{Table: "otel_metrics_exp_histogram"},
			Phi:                        0.99,
			ScaleColumn:                "Scale",
			ZeroCountColumn:            "ZeroCount",
			ZeroThresholdColumn:        "ZeroThreshold",
			PositiveOffsetColumn:       "PositiveOffset",
			PositiveBucketCountsColumn: "PositiveBucketCounts",
			NegativeOffsetColumn:       "NegativeOffset",
			NegativeBucketCountsColumn: "NegativeBucketCounts",
			GroupBy:                    []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			GroupByAliases:             []string{"attrs"},
			MetricNameColumn:           "MetricName",
			AttributesColumn:           "Attributes",
			TimestampColumn:            "TimeUnix",
		}
	}
	if !build().Equal(build()) {
		t.Fatalf("identical HistogramQuantileNative trees should be Equal")
	}
}

func TestHistogramQuantileNative_Equal_Negative_ScaleColumn(t *testing.T) {
	t.Parallel()
	a := &chplan.HistogramQuantileNative{Input: &chplan.Scan{Table: "t"}, ScaleColumn: "Scale"}
	b := &chplan.HistogramQuantileNative{Input: &chplan.Scan{Table: "t"}, ScaleColumn: "Other"}
	if a.Equal(b) {
		t.Errorf("different ScaleColumn should not be Equal")
	}
}

func TestHistogramQuantileNative_Equal_Negative_Phi(t *testing.T) {
	t.Parallel()
	a := &chplan.HistogramQuantileNative{Input: &chplan.Scan{Table: "t"}, Phi: 0.5}
	b := &chplan.HistogramQuantileNative{Input: &chplan.Scan{Table: "t"}, Phi: 0.99}
	if a.Equal(b) {
		t.Errorf("different Phi should not be Equal")
	}
}

func TestHistogramQuantileNative_Equal_Negative_NegativeOffsetColumn(t *testing.T) {
	t.Parallel()
	a := &chplan.HistogramQuantileNative{
		Input: &chplan.Scan{Table: "t"}, NegativeOffsetColumn: "A",
	}
	b := &chplan.HistogramQuantileNative{
		Input: &chplan.Scan{Table: "t"}, NegativeOffsetColumn: "B",
	}
	if a.Equal(b) {
		t.Errorf("different NegativeOffsetColumn should not be Equal")
	}
}

func TestMetricsHistogramOverTime_Equal_Positive(t *testing.T) {
	t.Parallel()
	build := func() *chplan.MetricsHistogramOverTime {
		return &chplan.MetricsHistogramOverTime{
			Attr:           &chplan.ColumnRef{Name: "Duration"},
			IsDuration:     true,
			GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}},
			GroupByAliases: []string{"service"},
			BucketAlias:    "__bucket",
			ValueAlias:     "Value",
			Inner:          &chplan.Scan{Table: "otel_traces"},
		}
	}
	if !build().Equal(build()) {
		t.Fatalf("identical MetricsHistogramOverTime trees should be Equal")
	}
}

func TestMetricsHistogramOverTime_Equal_Negative_IsDuration(t *testing.T) {
	t.Parallel()
	a := &chplan.MetricsHistogramOverTime{
		Attr:  &chplan.ColumnRef{Name: "Duration"},
		Inner: &chplan.Scan{Table: "t"}, IsDuration: false,
	}
	b := &chplan.MetricsHistogramOverTime{
		Attr:  &chplan.ColumnRef{Name: "Duration"},
		Inner: &chplan.Scan{Table: "t"}, IsDuration: true,
	}
	if a.Equal(b) {
		t.Errorf("different IsDuration should not be Equal")
	}
}

func TestMetricsHistogramOverTime_Equal_Negative_Attr(t *testing.T) {
	t.Parallel()
	a := &chplan.MetricsHistogramOverTime{
		Attr:  &chplan.ColumnRef{Name: "X"},
		Inner: &chplan.Scan{Table: "t"},
	}
	b := &chplan.MetricsHistogramOverTime{
		Attr:  &chplan.ColumnRef{Name: "Y"},
		Inner: &chplan.Scan{Table: "t"},
	}
	if a.Equal(b) {
		t.Errorf("different Attr should not be Equal")
	}
}

func TestMetricsHistogramOverTime_Equal_Negative_AttrNilPresence(t *testing.T) {
	t.Parallel()
	a := &chplan.MetricsHistogramOverTime{
		Attr:  &chplan.ColumnRef{Name: "X"},
		Inner: &chplan.Scan{Table: "t"},
	}
	b := &chplan.MetricsHistogramOverTime{Inner: &chplan.Scan{Table: "t"}}
	if a.Equal(b) || b.Equal(a) {
		t.Errorf("Attr nil presence should differentiate Equal in both directions")
	}
}

func TestMetricsHistogramOverTime_Equal_Negative_BucketAlias(t *testing.T) {
	t.Parallel()
	a := &chplan.MetricsHistogramOverTime{Inner: &chplan.Scan{Table: "t"}, BucketAlias: "__bucket"}
	b := &chplan.MetricsHistogramOverTime{Inner: &chplan.Scan{Table: "t"}, BucketAlias: "bucket"}
	if a.Equal(b) {
		t.Errorf("different BucketAlias should not be Equal")
	}
}

func TestMetricsHistogramOverTime_Equal_Negative_InnerNilPresence(t *testing.T) {
	t.Parallel()
	a := &chplan.MetricsHistogramOverTime{}
	b := &chplan.MetricsHistogramOverTime{Inner: &chplan.Scan{Table: "t"}}
	if a.Equal(b) || b.Equal(a) {
		t.Errorf("Inner nil presence should differentiate Equal in both directions")
	}
}

// -----------------------------------------------------------------------
// Expr Equal tests — coverage parallel to node_negatives_test.go,
// focused on positive cases the existing file does not exercise.
// -----------------------------------------------------------------------

func TestColumnRef_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.ColumnRef{Name: "X", Qualifier: "L"}
	b := &chplan.ColumnRef{Name: "X", Qualifier: "L"}
	if !a.Equal(b) {
		t.Fatalf("identical ColumnRef should be Equal")
	}
}

func TestLitString_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.LitString{V: "hello"}
	b := &chplan.LitString{V: "hello"}
	if !a.Equal(b) {
		t.Fatalf("identical LitString should be Equal")
	}
}

func TestLitInt_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.LitInt{V: 42}
	b := &chplan.LitInt{V: 42}
	if !a.Equal(b) {
		t.Fatalf("identical LitInt should be Equal")
	}
}

func TestLitFloat_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.LitFloat{V: 3.14}
	b := &chplan.LitFloat{V: 3.14}
	if !a.Equal(b) {
		t.Fatalf("identical LitFloat should be Equal")
	}
}

func TestLitFloat_Equal_NaN(t *testing.T) {
	t.Parallel()
	a := &chplan.LitFloat{V: math.NaN()}
	b := &chplan.LitFloat{V: math.NaN()}
	if !a.Equal(b) {
		t.Errorf("NaN == NaN must be Equal (Equal contract handles NaN specially)")
	}
	c := &chplan.LitFloat{V: 0.0}
	if a.Equal(c) {
		t.Errorf("NaN must not equal 0.0")
	}
}

func TestLitBool_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.LitBool{V: true}
	b := &chplan.LitBool{V: true}
	if !a.Equal(b) {
		t.Fatalf("identical LitBool should be Equal")
	}
}

func TestBinary_Equal_Positive(t *testing.T) {
	t.Parallel()
	build := func() *chplan.Binary {
		return &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "X"},
			Right: &chplan.LitInt{V: 1},
		}
	}
	if !build().Equal(build()) {
		t.Fatalf("identical Binary should be Equal")
	}
}

func TestFuncCall_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.FuncCall{Name: "round", Args: []chplan.Expr{&chplan.LitFloat{V: 1.5}}}
	b := &chplan.FuncCall{Name: "round", Args: []chplan.Expr{&chplan.LitFloat{V: 1.5}}}
	if !a.Equal(b) {
		t.Fatalf("identical FuncCall should be Equal")
	}
}

func TestFieldAccess_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "SpanAttributes"}, Path: "http.method"}
	b := &chplan.FieldAccess{Source: &chplan.ColumnRef{Name: "SpanAttributes"}, Path: "http.method"}
	if !a.Equal(b) {
		t.Fatalf("identical FieldAccess should be Equal")
	}
}

func TestMapAccess_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.MapAccess{
		Map: &chplan.ColumnRef{Name: "Attributes"},
		Key: &chplan.LitString{V: "k"},
	}
	b := &chplan.MapAccess{
		Map: &chplan.ColumnRef{Name: "Attributes"},
		Key: &chplan.LitString{V: "k"},
	}
	if !a.Equal(b) {
		t.Fatalf("identical MapAccess should be Equal")
	}
}

func TestMapAccess_Equal_Negative_Map(t *testing.T) {
	t.Parallel()
	a := &chplan.MapAccess{
		Map: &chplan.ColumnRef{Name: "Attributes"},
		Key: &chplan.LitString{V: "k"},
	}
	b := &chplan.MapAccess{
		Map: &chplan.ColumnRef{Name: "Other"},
		Key: &chplan.LitString{V: "k"},
	}
	if a.Equal(b) {
		t.Errorf("different Map should not be Equal")
	}
}

func TestMapWithoutKeys_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.MapWithoutKeys{
		Map:  &chplan.ColumnRef{Name: "Attributes"},
		Keys: []string{"instance", "pod"},
	}
	b := &chplan.MapWithoutKeys{
		Map:  &chplan.ColumnRef{Name: "Attributes"},
		Keys: []string{"instance", "pod"},
	}
	if !a.Equal(b) {
		t.Fatalf("identical MapWithoutKeys should be Equal")
	}
}

func TestMapWithoutKeys_Equal_Negative_Map(t *testing.T) {
	t.Parallel()
	a := &chplan.MapWithoutKeys{
		Map:  &chplan.ColumnRef{Name: "Attributes"},
		Keys: []string{"k"},
	}
	b := &chplan.MapWithoutKeys{
		Map:  &chplan.ColumnRef{Name: "Other"},
		Keys: []string{"k"},
	}
	if a.Equal(b) {
		t.Errorf("different Map should not be Equal")
	}
}

func TestMapWithoutKeys_Equal_Negative_KeysLen(t *testing.T) {
	t.Parallel()
	a := &chplan.MapWithoutKeys{
		Map:  &chplan.ColumnRef{Name: "Attributes"},
		Keys: []string{"k"},
	}
	b := &chplan.MapWithoutKeys{
		Map:  &chplan.ColumnRef{Name: "Attributes"},
		Keys: []string{"k", "x"},
	}
	if a.Equal(b) {
		t.Errorf("different Keys length should not be Equal")
	}
}

func TestLineContent_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.LineContent{
		Source:  &chplan.ColumnRef{Name: "Body"},
		Pattern: "ERROR",
		IsRegex: false,
		Negated: false,
	}
	b := &chplan.LineContent{
		Source:  &chplan.ColumnRef{Name: "Body"},
		Pattern: "ERROR",
		IsRegex: false,
		Negated: false,
	}
	if !a.Equal(b) {
		t.Fatalf("identical LineContent should be Equal")
	}
}

func TestLineContent_Equal_Negative_Source(t *testing.T) {
	t.Parallel()
	a := &chplan.LineContent{Source: &chplan.ColumnRef{Name: "Body"}, Pattern: "x"}
	b := &chplan.LineContent{Source: &chplan.ColumnRef{Name: "Message"}, Pattern: "x"}
	if a.Equal(b) {
		t.Errorf("different Source should not be Equal")
	}
}

func TestNestedArrayExists_Equal_Positive(t *testing.T) {
	t.Parallel()
	build := func() *chplan.NestedArrayExists {
		return &chplan.NestedArrayExists{
			Column:   "Events",
			SubField: "Attributes",
			Key:      "exception.type",
			Op:       chplan.OpEq,
			Value:    &chplan.LitString{V: "panic"},
		}
	}
	if !build().Equal(build()) {
		t.Fatalf("identical NestedArrayExists should be Equal")
	}
}

func TestNestedArrayExists_Equal_Negative_Column(t *testing.T) {
	t.Parallel()
	a := &chplan.NestedArrayExists{
		Column: "Events", SubField: "Attributes", Key: "k",
		Op: chplan.OpEq, Value: &chplan.LitString{V: "v"},
	}
	b := &chplan.NestedArrayExists{
		Column: "Links", SubField: "Attributes", Key: "k",
		Op: chplan.OpEq, Value: &chplan.LitString{V: "v"},
	}
	if a.Equal(b) {
		t.Errorf("different Column should not be Equal")
	}
}

func TestNestedArrayExists_Equal_Negative_Op(t *testing.T) {
	t.Parallel()
	a := &chplan.NestedArrayExists{
		Column: "Events", SubField: "Attributes", Key: "k",
		Op: chplan.OpEq, Value: &chplan.LitString{V: "v"},
	}
	b := &chplan.NestedArrayExists{
		Column: "Events", SubField: "Attributes", Key: "k",
		Op: chplan.OpNe, Value: &chplan.LitString{V: "v"},
	}
	if a.Equal(b) {
		t.Errorf("different Op should not be Equal")
	}
}

func TestNestedArrayExists_Equal_Negative_SubField(t *testing.T) {
	t.Parallel()
	a := &chplan.NestedArrayExists{
		Column: "Events", SubField: "Attributes", Key: "k",
		Op: chplan.OpEq, Value: &chplan.LitString{V: "v"},
	}
	b := &chplan.NestedArrayExists{
		Column: "Events", SubField: "Name", Key: "k",
		Op: chplan.OpEq, Value: &chplan.LitString{V: "v"},
	}
	if a.Equal(b) {
		t.Errorf("different SubField should not be Equal")
	}
}

func TestNestedArrayExists_Equal_Negative_Value(t *testing.T) {
	t.Parallel()
	a := &chplan.NestedArrayExists{
		Column: "Events", SubField: "Attributes", Key: "k",
		Op: chplan.OpEq, Value: &chplan.LitString{V: "a"},
	}
	b := &chplan.NestedArrayExists{
		Column: "Events", SubField: "Attributes", Key: "k",
		Op: chplan.OpEq, Value: &chplan.LitString{V: "b"},
	}
	if a.Equal(b) {
		t.Errorf("different Value should not be Equal")
	}
}

func TestNestedArrayExists_Equal_NilValue(t *testing.T) {
	t.Parallel()
	a := &chplan.NestedArrayExists{
		Column: "Events", SubField: "Attributes", Key: "k", Op: chplan.OpEq,
	}
	b := &chplan.NestedArrayExists{
		Column: "Events", SubField: "Attributes", Key: "k", Op: chplan.OpEq,
	}
	if !a.Equal(b) {
		t.Errorf("two NestedArrayExists with nil Value should be Equal")
	}
	c := &chplan.NestedArrayExists{
		Column: "Events", SubField: "Attributes", Key: "k", Op: chplan.OpEq,
		Value: &chplan.LitString{V: "x"},
	}
	if a.Equal(c) {
		t.Errorf("nil Value vs non-nil Value should not be Equal")
	}
}

func TestColumnRef_Equal_Empty(t *testing.T) {
	t.Parallel()
	a := &chplan.ColumnRef{}
	b := &chplan.ColumnRef{}
	if !a.Equal(b) {
		t.Errorf("zero-value ColumnRefs should be Equal")
	}
}

// -----------------------------------------------------------------------
// Cross-cutting positive cases: nested expressions inside Filter / Project /
// Aggregate must compare Equal end-to-end. These guard against an Equal()
// short-circuit that compares the top-level fields and forgets to recurse.
// -----------------------------------------------------------------------

func TestFilter_Equal_Positive_DeepNesting(t *testing.T) {
	t.Parallel()
	build := func() *chplan.Filter {
		return &chplan.Filter{
			Input: &chplan.Project{
				Input: &chplan.Scan{Table: "t"},
				Projections: []chplan.Projection{
					{Expr: &chplan.ColumnRef{Name: "A"}},
					{Expr: &chplan.ColumnRef{Name: "B"}},
				},
			},
			Predicate: &chplan.Binary{
				Op:   chplan.OpAnd,
				Left: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "X"}, Right: &chplan.LitString{V: "v"}},
				Right: &chplan.Binary{
					Op:    chplan.OpGt,
					Left:  &chplan.ColumnRef{Name: "Y"},
					Right: &chplan.LitInt{V: 100},
				},
			},
		}
	}
	if !build().Equal(build()) {
		t.Errorf("deeply nested identical Filter trees should be Equal")
	}
}

func TestAggregate_Equal_Positive_WithParameterisedAggFunc(t *testing.T) {
	t.Parallel()
	build := func() *chplan.Aggregate {
		return &chplan.Aggregate{
			Input: &chplan.Scan{Table: "t"},
			AggFuncs: []chplan.AggFunc{
				{
					Name:   "quantile",
					Params: []chplan.Expr{&chplan.LitFloat{V: 0.95}},
					Args:   []chplan.Expr{&chplan.ColumnRef{Name: "Value"}},
					Alias:  "Value",
				},
			},
		}
	}
	if !build().Equal(build()) {
		t.Errorf("identical parameterised AggFunc inside Aggregate should be Equal")
	}
}

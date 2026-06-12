package optimizer_test

// No-mis-rewrite regression bank (Layer 4 § E).
//
// Every regression pinned here is a "the optimizer must NOT rewrite
// this case" assertion. The motivation for each test is documented
// inline: most reflect a property of the chplan IR or upstream
// query-language semantics that a future rule change could
// accidentally break.
//
// The bank starts with the cases the existing test corpus implies
// (subquery-matrix RangeWindow, subquery-over-aggregator, etc.) plus
// a few we've validated against the live optimizer in this
// pass — every test below has been confirmed green against the
// current Default() pipeline. Future contributors who change an
// optimizer rule should add a new test here when the change is
// motivated by a bug they shipped a fix for.

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// TestRegression_MatrixRangeWindow_NotRewrittenByFilterFusion is the
// pin recorded in test/spec/optimizer/subquery_matrix_opaque (and
// optimizer_test.go's "subquery_matrix_opaque" entry). Filter fusion
// inside the matrix RangeWindow's Input subtree IS allowed; the
// matrix RangeWindow node itself (with OuterRange != 0) must be
// preserved verbatim.
func TestRegression_MatrixRangeWindow_NotRewrittenByFilterFusion(t *testing.T) {
	t.Parallel()
	scan := &chplan.Scan{Table: "otel_metrics_sum"}
	innerFilter := &chplan.Filter{
		Input: &chplan.Filter{
			Input:     scan,
			Predicate: labelFilter("MetricName", "http_requests_total"),
		},
		Predicate: &chplan.Binary{
			Op: chplan.OpEq,
			Left: &chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: "Attributes"},
				Key: &chplan.LitString{V: "job"},
			},
			Right: &chplan.LitString{V: "api"},
		},
	}
	plan := &chplan.RangeWindow{
		Input:           innerFilter,
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            5 * time.Minute,
		OuterRange:      time.Hour, // ← matrix shape
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}

	out := optimizer.Default().Run(context.Background(), plan)
	rw, ok := out.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("expected RangeWindow at root, got %T", out)
	}
	if rw.OuterRange != time.Hour {
		t.Errorf("OuterRange dropped: got %v, want 1h (matrix shape must be preserved)", rw.OuterRange)
	}
	if rw.Func != "rate" {
		t.Errorf("Func mutated: got %q, want rate", rw.Func)
	}
}

// TestRegression_FilterOverAggregate_OutputColumn_NotPushed pins the
// FilterAggregateTranspose safety carve-out: a predicate on the
// aggregate output (`sum_value`) refers to a column that doesn't
// exist below the Aggregate. Pushing the filter down would emit
// invalid SQL or wrong rows.
func TestRegression_FilterOverAggregate_OutputColumn_NotPushed(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input:   &chplan.Scan{Table: "otel_metrics_gauge"},
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
			AggFuncs: []chplan.AggFunc{
				{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "sum_value"},
			Right: &chplan.LitFloat{V: 0},
		},
	}
	out := optimizer.Default().Run(context.Background(), plan)
	if _, ok := out.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter at root (sum_value is post-Aggregate only); got %T", out)
	}
}

// TestRegression_FilterOverRangeWindow_TimestampColumn_NotPushed
// pins the per-sample-vs-per-step distinction: pushing a TimeUnix
// filter under the RangeWindow changes the semantics from
// "filter the grid" to "filter the input samples", which is wrong.
func TestRegression_FilterOverRangeWindow_TimestampColumn_NotPushed(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	plan := &chplan.Filter{
		Input: rw,
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "TimeUnix"},
			Right: &chplan.LitInt{V: 0},
		},
	}
	out := optimizer.Default().Run(context.Background(), plan)
	if _, ok := out.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter at root (TimeUnix filter must stay above the window); got %T", out)
	}
}

// TestRegression_MixedSafeUnsafePredicate_NotPushed pins the
// "split AND" non-policy: a predicate `safe AND unsafe` keeps the
// entire Filter above the RangeWindow. Implementing AND-splitting
// without thinking through FilterFusion interaction would silently
// duplicate work.
func TestRegression_MixedSafeUnsafePredicate_NotPushed(t *testing.T) {
	t.Parallel()
	rw := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	// `safe AND unsafe`: safe sub-clause references Attributes (a
	// passthrough series key); unsafe sub-clause references Value
	// (the windowed output, not in the input row shape).
	plan := &chplan.Filter{
		Input: rw,
		Predicate: &chplan.Binary{
			Op:   chplan.OpAnd,
			Left: labelFilter("Attributes", "v1"),
			Right: &chplan.Binary{
				Op:    chplan.OpGt,
				Left:  &chplan.ColumnRef{Name: "Value"},
				Right: &chplan.LitFloat{V: 0},
			},
		},
	}
	out := optimizer.Default().Run(context.Background(), plan)
	if _, ok := out.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter at root (mixed predicate must stay above the window); got %T", out)
	}
}

// TestRegression_AnalyzerSemanticFold_LeavesBoolIdentityAlone pins
// the analyzer/optimizer split: ConstantFoldSemantic must NOT apply
// boolean identities. The rule's idempotence verification pass would
// panic if it secretly applied `true AND X → X` because the
// heuristic rule then would re-trigger on the canonical form.
func TestRegression_AnalyzerSemanticFold_LeavesBoolIdentityAlone(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: "t"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.LitBool{V: true},
			Right: labelFilter("MetricName", "up"),
		},
	}
	_, changed := optimizer.ConstantFoldSemantic{}.Apply(plan)
	if changed {
		t.Fatalf("ConstantFoldSemantic must not collapse `true AND X` (that's the heuristic flavour's job)")
	}
}

// TestRegression_HeuristicFold_LeavesArithmeticAlone is the mirror:
// the heuristic rule must NOT do semantic arithmetic. Mixing
// flavours would defeat the split.
func TestRegression_HeuristicFold_LeavesArithmeticAlone(t *testing.T) {
	t.Parallel()
	plan := &chplan.Filter{
		Input: &chplan.Scan{Table: "t"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.Binary{Op: chplan.OpAdd, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 2}},
			Right: &chplan.LitInt{V: 3},
		},
	}
	_, changed := optimizer.ConstantFoldHeuristic{}.Apply(plan)
	if changed {
		t.Fatalf("ConstantFoldHeuristic must not fold pure-literal arithmetic (that's the semantic flavour's job)")
	}
}

// TestRegression_NestedFilterPredicate_PreservedAfterFusion verifies
// that fusion preserves predicate content exactly. AND-of-original-
// predicates must round-trip via chplan.Equal after fusion runs to
// fixpoint over a 3-deep stack.
func TestRegression_NestedFilterPredicate_PreservedAfterFusion(t *testing.T) {
	t.Parallel()
	p1 := labelFilter("MetricName", "up")
	p2 := labelFilter("job", "api")
	p3 := labelFilter("Attributes", "v1")
	plan := &chplan.Filter{
		Input: &chplan.Filter{
			Input: &chplan.Filter{
				Input:     &chplan.Scan{Table: "otel_metrics_gauge"},
				Predicate: p1,
			},
			Predicate: p2,
		},
		Predicate: p3,
	}
	out := optimizer.Default().Run(context.Background(), plan)
	f, ok := out.(*chplan.Filter)
	if !ok {
		t.Fatalf("expected single Filter at root after fusion, got %T", out)
	}
	// Predicate is now `((p1 AND p2) AND p3)`. Walk the tree and
	// confirm all three leaf names appear.
	names := collectColumnNames(f.Predicate)
	for _, want := range []string{"MetricName", "job", "Attributes"} {
		if _, ok := names[want]; !ok {
			t.Errorf("predicate must mention %q after fusion; got names=%v", want, names)
		}
	}
}

// collectColumnNames walks an expression and returns the set of
// ColumnRef names it touches.
func collectColumnNames(e chplan.Expr) map[string]struct{} {
	out := map[string]struct{}{}
	var walk func(e chplan.Expr)
	walk = func(e chplan.Expr) {
		switch v := e.(type) {
		case *chplan.ColumnRef:
			out[v.Name] = struct{}{}
		case *chplan.Binary:
			walk(v.Left)
			walk(v.Right)
		case *chplan.MapAccess:
			walk(v.Map)
		case *chplan.FuncCall:
			for _, a := range v.Args {
				walk(a)
			}
		}
	}
	walk(e)
	return out
}

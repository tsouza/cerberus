package promql

import (
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/capmutant"
)

// A syntactically valid mixed fold must reject a zero-length window even
// when evaluation time is available. Parser validation is not this helper's
// contract: callers can pass an already constructed expression tree.
func TestMixedOuterAggregateFoldRejectsZeroRange(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expr := mustParse(t, `sum(rate((latency_exp_hist or other_metric)[5m:1m]))`)
	agg, ok := expr.(*parser.AggregateExpr)
	if !ok {
		t.Fatalf("expression = %T, want aggregate", expr)
	}
	call, ok := agg.Expr.(*parser.Call)
	if !ok {
		t.Fatalf("aggregate operand = %T, want call", agg.Expr)
	}
	sub, ok := call.Args[0].(*parser.SubqueryExpr)
	if !ok {
		t.Fatalf("fold operand = %T, want subquery", call.Args[0])
	}
	ctx := lowerCtx{start: at, end: at}
	if _, _, _, _, matched := sumOrAvgOverMixedOrSubqueryFoldFn(expr, s, ctx); !matched {
		t.Fatal("positive-range control was not recognized")
	}
	sub.Range = 0
	if _, _, _, _, matched := sumOrAvgOverMixedOrSubqueryFoldFn(expr, s, ctx); matched {
		t.Fatal("zero-range mixed fold was recognized")
	}
}

// Default subquery steps and outer query alignment are independent. The
// inner grid needs a positive default even for an instant outer query;
// only query_range may mark the recombined result StepAligned.
func TestMixedOuterAggregateFoldDefaultStepAndAlignment(t *testing.T) {
	t.Parallel()
	const outerRange = 10 * time.Minute
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		step time.Duration
	}{
		{name: "instant"},
		{name: "range", step: 2 * defaultSubqueryStep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expr := mustParse(t, `sum(rate((latency_exp_hist or other_metric)[5m:]))`)
			end := at
			if tc.step > 0 {
				end = at.Add(outerRange)
			}
			ctx := lowerCtx{
				start: at, end: end, step: tc.step,
				lowerers:       RangeLowerers{}.withDefaults(),
				resourceBounds: ResourceBounds{}.withDefaults(),
			}
			agg, sub, windowFn, binary, matched := sumOrAvgOverMixedOrSubqueryFoldFn(expr, s, ctx)
			if !matched {
				t.Fatal("mixed fold with default subquery step was not recognized")
			}
			plan, err := lowerSumOrAvgOverMixedOrSubqueryFoldFn(agg, sub, windowFn, binary, s, ctx)
			if err != nil {
				t.Fatalf("lower mixed fold: %v", err)
			}
			union, ok := plan.(*chplan.VectorSetOp)
			if !ok {
				t.Fatalf("fold result = %T, want VectorSetOp", plan)
			}
			if want := tc.step > 0; union.StepAligned != want {
				t.Errorf("StepAligned = %v, want %v", union.StepAligned, want)
			}
			innerGrids := 0
			chplan.WalkDeep(plan, func(node chplan.Node) bool {
				carrier, ok := node.(chplan.GridCarrier)
				if !ok {
					return true
				}
				start, _, step := carrier.EvalGrid()
				// The subquery extends before the outer request's start;
				// outer fold carriers begin at that start instead.
				if !start.IsZero() && start.Before(ctx.start) {
					innerGrids++
					if step != defaultSubqueryStep {
						t.Errorf("inner %T step = %s, want default %s", node, step, defaultSubqueryStep)
					}
				}
				return true
			})
			if innerGrids == 0 {
				t.Fatal("no materialized inner subquery grid was inspected")
			}
		})
	}
}

// The partition expression slice escapes through WindowExpr.PartitionBy.
// Appending the anchor must reserve the full group key without regrowing it.
func TestHistogramMergeScalePartitionCapacity(t *testing.T) {
	t.Parallel()
	const groupKeys = 5
	capmutant.AssertKilled(t, capmutant.Hint{
		Construct: "exp_histogram_merge_summap.go:`len(groupBy)+1`",
		Positions: []capmutant.Position{{Name: "anchor slot", Op: "+"}},
		Eval: func(t testing.TB, ops []string) (int, bool) {
			return capmutant.Eval(t, []int{groupKeys, 1}, ops)
		},
		Observe: func(t *testing.T) (int, int) {
			keys := make([]chplan.Expr, groupKeys)
			for i := range keys {
				keys[i] = &chplan.ColumnRef{Name: "group"}
			}
			partition := expHistogramMergeScaleWindowPartitionBy(&chplan.ColumnRef{Name: "anchor"}, keys)
			return len(partition), cap(partition)
		},
		Build: func(hint int) (int, int) {
			partition := make([]chplan.Expr, 0, hint)
			partition = append(partition, &chplan.ColumnRef{Name: "anchor"})
			partition = append(partition, make([]chplan.Expr, groupKeys)...)
			return len(partition), cap(partition)
		},
	})
}

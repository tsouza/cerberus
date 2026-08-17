package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_SetOpIsHistogramValued pins cerberus issue
// #2324: `and` / `or` / `unless` between two exp-histogram-valued
// operands lowers successfully to a chplan.VectorSetOp whose Histogram
// flag is set, so the chsql emitter carries the nine Histogram*Column
// outputs through instead of dropping them.
func TestLower_ExpHistogram_SetOpIsHistogramValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	queries := []string{
		`latency_exp_hist and latency_exp_hist`,
		`latency_exp_hist or other_exp_hist`,
		`latency_exp_hist unless other_exp_hist`,
		`latency_exp_hist and on(service) other_exp_hist`,
		`latency_exp_hist and ignoring(service) other_exp_hist`,
	}

	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want histogram", query, shape)
			}
			setOp, ok := plan.(*chplan.VectorSetOp)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.VectorSetOp", query, plan)
			}
			if !setOp.Histogram {
				t.Fatalf("lower(%q): VectorSetOp.Histogram = false, want true", query)
			}
			if _, ok := setOp.Left.(*chplan.HistogramProjection); !ok {
				t.Fatalf("lower(%q): VectorSetOp.Left is %T, want *chplan.HistogramProjection", query, setOp.Left)
			}
			if _, ok := setOp.Right.(*chplan.HistogramProjection); !ok {
				t.Fatalf("lower(%q): VectorSetOp.Right is %T, want *chplan.HistogramProjection", query, setOp.Right)
			}
		})
	}
}

// TestLower_ExpHistogram_SetOpMixedOperandStillRejects pins that a set
// op with only ONE histogram-valued operand does not spuriously match
// [expHistogramSetOp] (which requires BOTH sides histogram-valued) and
// still rejects cleanly — via the pre-existing
// expHistogramSelectorRouting message the plain vector-selector path
// hits for the histogram-shaped operand — rather than a panic or a
// silently wrong plan.
func TestLower_ExpHistogram_SetOpMixedOperandStillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := `latency_exp_hist and up`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	if _, err := promql.LowerAt(context.Background(), expr, s, at, at); err == nil {
		t.Fatalf("lower(%q): expected an error, got none", query)
	}
}

// TestLower_ExpHistogram_SetOpComposes pins that a histogram-valued set
// op composes like any other histogram-valued shape: it can be an
// operand of an OUTER set op (a chain, `a or b or c`, parsed left-assoc
// as `(a or b) or c`) and it can be wrapped in a further `sum`/`avg` —
// both routes go through [lowerExpHistogramValuedShape]'s recursive
// dispatch rather than [lowerExpHistogramSetOp]'s stricter sibling
// [lowerExpHistogramValuedOperand] (which requires a literal
// *chplan.HistogramProjection operand and would reject a nested
// *chplan.VectorSetOp).
func TestLower_ExpHistogram_SetOpComposes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("chained or", func(t *testing.T) {
		t.Parallel()
		query := `latency_exp_hist or other_exp_hist or latency_exp_hist`
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
		if err != nil {
			t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
		}
		if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
			t.Fatalf("lower(%q): plan root publishes %s, want histogram", query, shape)
		}
		outer, ok := plan.(*chplan.VectorSetOp)
		if !ok {
			t.Fatalf("lower(%q): plan root is %T, want *chplan.VectorSetOp", query, plan)
		}
		if !outer.Histogram {
			t.Fatalf("lower(%q): outer VectorSetOp.Histogram = false, want true", query)
		}
		inner, ok := outer.Left.(*chplan.VectorSetOp)
		if !ok {
			t.Fatalf("lower(%q): outer.Left is %T, want a nested *chplan.VectorSetOp", query, outer.Left)
		}
		if !inner.Histogram {
			t.Fatalf("lower(%q): inner VectorSetOp.Histogram = false, want true", query)
		}
	})

	t.Run("wrapped in sum", func(t *testing.T) {
		t.Parallel()
		query := `sum(latency_exp_hist and latency_exp_hist)`
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
		if err != nil {
			t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
		}
		if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
			t.Fatalf("lower(%q): plan root publishes %s, want histogram", query, shape)
		}
		hp, ok := plan.(*chplan.HistogramProjection)
		if !ok {
			t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", query, plan)
		}
		if _, ok := hp.Input.(*chplan.Project); !ok {
			t.Fatalf("lower(%q): HistogramProjection.Input is %T, want *chplan.Project", query, hp.Input)
		}
	})
}

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

// TestLower_ExpHistogram_MixedSetOp_AndUnless pins cerberus issue #2325:
// `and` / `unless` between a FLOAT-valued operand (already reduced from
// a native histogram, e.g. via histogram_quantile()) and a raw
// histogram-valued selector lowers successfully in EITHER operand
// order. Reference Prometheus's set operators match purely on labels,
// never on either side's value type, so mixing is legal upstream; only
// #2324/#2326's OWN fix (routing the shape where BOTH sides are
// histogram-valued) left this gap, because [expHistogramSetOp] requires
// both sides histogram-valued and the mixed shape used to fall through
// to lowerVectorSetOp's plain lower() on each side, which sent the
// histogram-valued operand into [lowerVectorSelector] and hit
// [expHistogramSelectorRouting]'s catch-all rejection.
//
// `and` / `unless` forward exactly one side's rows verbatim (the LHS,
// filtered by whether its signature also — for `and` — or does not, for
// `unless` — appear on the RHS), so the RESULT's value type always
// matches the LHS's own: float when the float-reduced operand is on the
// left, histogram when the raw selector is.
func TestLower_ExpHistogram_MixedSetOp_AndUnless(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		query         string
		wantHistogram bool
	}{
		{`histogram_quantile(0.5, latency_exp_hist) and latency_exp_hist`, false},
		{`latency_exp_hist and histogram_quantile(0.5, latency_exp_hist)`, true},
		{`histogram_quantile(0.5, latency_exp_hist) unless latency_exp_hist`, false},
		{`latency_exp_hist unless histogram_quantile(0.5, latency_exp_hist)`, true},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tt.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tt.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error: %v", tt.query, err)
			}
			wantShape := chplan.SampleRowShape
			if tt.wantHistogram {
				wantShape = chplan.HistogramRowShape
			}
			if shape := chplan.RowShapeOf(plan); shape != wantShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s", tt.query, shape, wantShape)
			}
			setOp, ok := plan.(*chplan.VectorSetOp)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.VectorSetOp", tt.query, plan)
			}
			if setOp.Histogram != tt.wantHistogram {
				t.Fatalf("lower(%q): VectorSetOp.Histogram = %v, want %v", tt.query, setOp.Histogram, tt.wantHistogram)
			}
		})
	}
}

// TestLower_ExpHistogram_SetOpComposes pins that a histogram-valued set
// op composes like any other histogram-valued shape: it can be an
// operand of an OUTER set op (a chain, `a or b or c`, parsed left-assoc
// as `(a or b) or c`) and it can be wrapped in a further `sum`/`avg` —
// both routes go through [lowerExpHistogramValuedShape]'s recursive
// dispatch. [lowerExpHistogramValuedOperand] (the +/-/==/!= binop sibling
// of [lowerExpHistogramSetOpOperand]) also accepts a nested
// *chplan.VectorSetOp operand since cerberus issue #2559 — see
// histogram_native_binop_test.go's own set-op-operand coverage for that
// path.
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

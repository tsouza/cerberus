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

// TestLower_ExpHistogram_LabelCallOverSetOp pins cerberus issue #2468:
// label_replace() / label_join() wrapping a both-histogram `and`/`or`/
// `unless` set-op (cerberus issue #2324) used to panic-free but ERROR at
// lowering time — histogram_native_label_replace.go's
// lowerLabelCallOverExpHistogram hard-asserted its inner histogram-valued
// operand was literally a *chplan.HistogramProjection, but
// histogram_native_set_op.go's lowerExpHistogramSetOp (also matched by
// isExpHistogramValuedShape for a both-histogram set-op) builds a
// *chplan.VectorSetOp{Histogram: true} instead, which is a DIFFERENT Go
// type publishing the identical chplan.HistogramRowShape contract. The
// fix widens rewriteHistogramProjectionAttributes to accept any
// chplan.Node answering that shape rather than asserting the narrower
// concrete type.
func TestLower_ExpHistogram_LabelCallOverSetOp(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	queries := []string{
		`label_replace(latency_exp_hist or other_exp_hist, "x", "y", "z", ".*")`,
		`label_replace(latency_exp_hist and latency_exp_hist, "x", "y", "z", ".*")`,
		`label_replace(latency_exp_hist unless other_exp_hist, "x", "y", "z", ".*")`,
		`label_join(latency_exp_hist or other_exp_hist, "copy", "-", "service")`,
	}

	for _, query := range queries {
		query := query
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
			outer, ok := plan.(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("lower(%q): plan root = %T, want *chplan.HistogramProjection", query, plan)
			}

			// The label rewrite's guard machinery sits between the outer
			// HistogramProjection and the inner VectorSetOp: Aggregate
			// (duplicate-labelset guard) -> Project (attribute rewrite) ->
			// the VectorSetOp #2324 built. Walk down to confirm the set op
			// survived the round trip rather than being silently dropped.
			agg, ok := outer.Input.(*chplan.Aggregate)
			if !ok {
				t.Fatalf("lower(%q): HistogramProjection.Input = %T, want *chplan.Aggregate", query, outer.Input)
			}
			project, ok := agg.Input.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): Aggregate.Input = %T, want *chplan.Project", query, agg.Input)
			}
			setOp, ok := project.Input.(*chplan.VectorSetOp)
			if !ok {
				t.Fatalf("lower(%q): Project.Input = %T, want *chplan.VectorSetOp", query, project.Input)
			}
			if !setOp.Histogram {
				t.Fatalf("lower(%q): VectorSetOp.Histogram = false, want true", query)
			}
		})
	}
}

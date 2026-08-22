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

// TestLower_ExpHistogram_FloatVectorScalingBinopAnswersScaledHistogram
// pins cerberus issues #2339 and #2342: MUL (either operand order) and
// histogram-left DIV between a histogram-valued operand and a genuine
// (non-literal) float-VECTOR operand — under default (full-Attributes)
// one-to-one matching, on()/ignoring() reduced-key one-to-one matching,
// and the "histogram is the many side" group_left()/group_right()
// broadcast — now answer a scaled histogram — a
// *chplan.HistogramProjection rooted in a *chplan.HistogramFloatVectorJoin
// — rather than the pre-existing catch-all rejection
// TestLower_ExpHistogram_FloatVectorScalingShapesStillRejected used to
// pin (histogram_native_float_vector_binop_test.go, now superseded).
func TestLower_ExpHistogram_FloatVectorScalingBinopAnswersScaledHistogram(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	cases := []struct {
		name  string
		query string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{
			name:  "histogram times float vector",
			query: `latency_exp_hist * histogram_quantile(0.5, latency_exp_hist)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "float vector times histogram (commutative)",
			query: `histogram_quantile(0.5, latency_exp_hist) * latency_exp_hist`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "histogram divided by float vector",
			query: `latency_exp_hist / histogram_quantile(0.5, latency_exp_hist)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "histogram times float vector, range",
			query: `latency_exp_hist * histogram_quantile(0.5, latency_exp_hist)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			// cerberus issue #2342: on() reduced-key one-to-one
			// matching, histogram on the syntactic LHS.
			name:  "histogram times float vector, on()",
			query: `latency_exp_hist * on(service) histogram_quantile(0.5, latency_exp_hist)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			// cerberus issue #2342: ignoring() reduced-key one-to-one
			// matching, histogram on the syntactic LHS.
			name:  "histogram times float vector, ignoring()",
			query: `latency_exp_hist * ignoring(service) histogram_quantile(0.5, latency_exp_hist)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			// cerberus issue #2342: group_left() broadcast, histogram
			// on the syntactic LHS (the "many" side under group_left).
			name:  "histogram times float vector, group_left()",
			query: `latency_exp_hist * on(service) group_left() histogram_quantile(0.5, latency_exp_hist)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			// cerberus issue #2342: group_right() broadcast,
			// mirror-image operand order — histogram on the syntactic
			// RHS (the "many" side under group_right).
			name:  "float vector times histogram, group_right()",
			query: `histogram_quantile(0.5, latency_exp_hist) * on(service) group_right() latency_exp_hist`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			// cerberus issue #2342: histogram-left DIV + group_left(),
			// histogram on the syntactic LHS (the only DIV shape this
			// recognizer answers, per its own header doc).
			name:  "histogram divided by float vector, group_left()",
			query: `latency_exp_hist / on(service) group_left() histogram_quantile(0.5, latency_exp_hist)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := tc.lower(expr)
			if err != nil {
				t.Fatalf("lower(%q): unexpected error: %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want histogram", tc.query, shape)
			}
			hp, ok := plan.(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", tc.query, plan)
			}
			name, ok := hp.GroupBy[0].(*chplan.LitString)
			if !ok || name.V != "" {
				t.Fatalf("lower(%q): metric-name projection is %#v, want empty literal", tc.query, hp.GroupBy[0])
			}
			if !planContainsHistogramFloatVectorJoin(hp) {
				t.Fatalf("lower(%q): plan does not root in a *chplan.HistogramFloatVectorJoin", tc.query)
			}
		})
	}
}

// planContainsHistogramFloatVectorJoin walks n's Input/Children chain
// looking for a *chplan.HistogramFloatVectorJoin — confirming the
// scaling lowering actually took the JOIN path rather than, say,
// silently degenerating to the literal-scalar scaling machinery.
func planContainsHistogramFloatVectorJoin(n chplan.Node) bool {
	for cur := n; cur != nil; {
		if _, ok := cur.(*chplan.HistogramFloatVectorJoin); ok {
			return true
		}
		children := cur.Children()
		if len(children) == 0 {
			return false
		}
		cur = children[0]
	}
	return false
}

// TestLower_ExpHistogram_FloatVectorScalingBinopStillRejected pins the
// residual boundary this file's header doc leaves in place now that
// cerberus issue #2342 has widened on()/ignoring()/group_left()/
// group_right() support: chplan.HistogramFloatVectorJoin.Left is ALWAYS
// the histogram operand, so a cardinality/matching shape that would
// require the histogram side to play the "one" role — broadcasting
// against many float rows, or reducing an on()/ignoring() key off the
// FLOAT operand's own Attributes rather than the histogram's — is not
// supported and still hits the pre-existing catch-all rejection.
func TestLower_ExpHistogram_FloatVectorScalingBinopStillRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	queries := []string{
		// CardOneToOne + on() with the histogram on the syntactic RHS:
		// Prometheus's resultMetric reduces the syntactic LHS (here the
		// float operand)'s own labels, which this recognizer does not
		// support (see this file's header doc).
		`histogram_quantile(0.5, latency_exp_hist) * on(service) latency_exp_hist`,
		// group_right() keeps the syntactic RHS many; the histogram
		// operand is on the LHS here, so it would play the "one" role.
		`latency_exp_hist * on(service) group_right() histogram_quantile(0.5, latency_exp_hist)`,
		// group_left() keeps the syntactic LHS many; the histogram
		// operand is on the RHS here, so it would play the "one" role.
		`histogram_quantile(0.5, latency_exp_hist) * on(service) group_left() latency_exp_hist`,
		// DIV only recognises the histogram-left shape, so the
		// histogram operand is always the syntactic LHS; group_right()
		// here would make the histogram side the "one".
		`latency_exp_hist / on(service) group_right() histogram_quantile(0.5, latency_exp_hist)`,
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			if _, err := promql.LowerAt(context.Background(), expr, s, end, end); err == nil {
				t.Fatalf("LowerAt(%q): expected the pre-existing catch-all rejection, got success", q)
			}
		})
	}
}

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
// pins cerberus issues #2339, #2342, and #2537: MUL (either operand
// order) and histogram-left DIV between a histogram-valued operand and a
// genuine (non-literal) float-VECTOR operand — under default
// (full-Attributes) one-to-one matching, on()/ignoring() reduced-key
// one-to-one matching REGARDLESS of which side of the expression the
// histogram operand is written on, and group_left()/group_right()
// broadcast in EITHER direction (the histogram operand playing "many" or
// "one") — now answer a scaled histogram — a *chplan.HistogramProjection
// rooted in a *chplan.HistogramFloatVectorJoin — rather than the
// pre-existing catch-all rejection
// TestLower_ExpHistogram_FloatVectorScalingShapesStillRejected used to
// pin (histogram_native_float_vector_binop_test.go, now superseded).
//
// The four `*/on(service) group_left/right()`, `on(service)` cases with
// the histogram on the syntactic RHS were pinned as a permanent boundary
// by this file's own former TestLower_ExpHistogram_
// FloatVectorScalingBinopStillRejected — cerberus issue #2537 found and
// closed that gap; probing broadly (see the issue) turned up no other
// combination of operand order, on()/ignoring(), and group_left()/
// group_right() this recognizer still rejects, so that test's coverage
// moves here rather than being deleted.
func TestLower_ExpHistogram_FloatVectorScalingBinopAnswersScaledHistogram(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	cases := []struct {
		name     string
		query    string
		wantCard chplan.VectorCard
		lower    func(parser.Expr) (chplan.Node, error)
	}{
		{
			name:     "histogram times float vector",
			query:    `latency_exp_hist * histogram_quantile(0.5, latency_exp_hist)`,
			wantCard: chplan.CardOneToOne,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:     "float vector times histogram (commutative)",
			query:    `histogram_quantile(0.5, latency_exp_hist) * latency_exp_hist`,
			wantCard: chplan.CardOneToOne,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:     "histogram divided by float vector",
			query:    `latency_exp_hist / histogram_quantile(0.5, latency_exp_hist)`,
			wantCard: chplan.CardOneToOne,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:     "histogram times float vector, range",
			query:    `latency_exp_hist * histogram_quantile(0.5, latency_exp_hist)`,
			wantCard: chplan.CardOneToOne,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			// cerberus issue #2342: on() reduced-key one-to-one
			// matching, histogram on the syntactic LHS.
			name:     "histogram times float vector, on()",
			query:    `latency_exp_hist * on(service) histogram_quantile(0.5, latency_exp_hist)`,
			wantCard: chplan.CardOneToOne,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			// cerberus issue #2342: ignoring() reduced-key one-to-one
			// matching, histogram on the syntactic LHS.
			name:     "histogram times float vector, ignoring()",
			query:    `latency_exp_hist * ignoring(service) histogram_quantile(0.5, latency_exp_hist)`,
			wantCard: chplan.CardOneToOne,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			// cerberus issue #2537: on() reduced-key one-to-one
			// matching, histogram on the syntactic RHS this time —
			// TestLower_ExpHistogram_FloatVectorScalingBinopStillRejected
			// used to pin this exact query as a permanent rejection.
			name:     "float vector times histogram, on()",
			query:    `histogram_quantile(0.5, latency_exp_hist) * on(service) latency_exp_hist`,
			wantCard: chplan.CardOneToOne,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			// cerberus issue #2342: group_left() broadcast, histogram
			// on the syntactic LHS (the "many" side under group_left) —
			// chplan.CardManyToOne (Left/histogram many).
			name:     "histogram times float vector, group_left()",
			query:    `latency_exp_hist * on(service) group_left() histogram_quantile(0.5, latency_exp_hist)`,
			wantCard: chplan.CardManyToOne,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			// cerberus issue #2342: group_right() broadcast,
			// mirror-image operand order — histogram on the syntactic
			// RHS (the "many" side under group_right) — chplan.
			// CardManyToOne (Left/histogram many, same physical role as
			// the group_left() case above despite the opposite PromQL
			// keyword).
			name:     "float vector times histogram, group_right()",
			query:    `histogram_quantile(0.5, latency_exp_hist) * on(service) group_right() latency_exp_hist`,
			wantCard: chplan.CardManyToOne,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			// cerberus issue #2537: group_right() broadcast with the
			// histogram on the syntactic LHS — group_right() keeps the
			// RHS (float) "many", so the histogram plays "one" and
			// broadcasts across every matching float row —
			// chplan.CardOneToMany (Left/histogram one). Formerly pinned
			// as a permanent rejection by TestLower_ExpHistogram_
			// FloatVectorScalingBinopStillRejected.
			name:     "histogram times float vector, group_right()",
			query:    `latency_exp_hist * on(service) group_right() histogram_quantile(0.5, latency_exp_hist)`,
			wantCard: chplan.CardOneToMany,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			// cerberus issue #2537: group_left() broadcast with the
			// histogram on the syntactic RHS — group_left() keeps the
			// LHS (float) "many", so the histogram plays "one" and
			// broadcasts — chplan.CardOneToMany (Left/histogram one).
			// Formerly pinned as a permanent rejection.
			name:     "float vector times histogram, group_left()",
			query:    `histogram_quantile(0.5, latency_exp_hist) * on(service) group_left() latency_exp_hist`,
			wantCard: chplan.CardOneToMany,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			// cerberus issue #2342: histogram-left DIV + group_left(),
			// histogram on the syntactic LHS (the only DIV shape this
			// recognizer answers, per its own header doc) —
			// chplan.CardManyToOne (Left/histogram many).
			name:     "histogram divided by float vector, group_left()",
			query:    `latency_exp_hist / on(service) group_left() histogram_quantile(0.5, latency_exp_hist)`,
			wantCard: chplan.CardManyToOne,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			// cerberus issue #2537: histogram-left DIV + group_right() —
			// DIV only recognises the histogram-left shape, so the
			// histogram operand is always the syntactic LHS here;
			// group_right() keeps the RHS (float) "many", so the
			// histogram plays "one" — chplan.CardOneToMany. Formerly
			// pinned as a permanent rejection.
			name:     "histogram divided by float vector, group_right()",
			query:    `latency_exp_hist / on(service) group_right() histogram_quantile(0.5, latency_exp_hist)`,
			wantCard: chplan.CardOneToMany,
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
			join := findHistogramFloatVectorJoin(hp)
			if join == nil {
				t.Fatalf("lower(%q): plan does not root in a *chplan.HistogramFloatVectorJoin", tc.query)
			}
			if join.Card != tc.wantCard {
				t.Fatalf("lower(%q): join.Card = %v, want %v", tc.query, join.Card, tc.wantCard)
			}
		})
	}
}

// findHistogramFloatVectorJoin walks n's Input/Children chain looking
// for a *chplan.HistogramFloatVectorJoin — confirming the scaling
// lowering actually took the JOIN path rather than, say, silently
// degenerating to the literal-scalar scaling machinery, and giving the
// caller the join node itself to inspect (e.g. its Card).
func findHistogramFloatVectorJoin(n chplan.Node) *chplan.HistogramFloatVectorJoin {
	for cur := n; cur != nil; {
		if j, ok := cur.(*chplan.HistogramFloatVectorJoin); ok {
			return j
		}
		children := cur.Children()
		if len(children) == 0 {
			return nil
		}
		cur = children[0]
	}
	return nil
}

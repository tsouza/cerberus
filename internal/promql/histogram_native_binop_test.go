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

// TestLower_ExpHistogram_HistogramBinopIsHistogramValued pins cerberus
// issue #2263: `<exp-hist shape> (+|-) <exp-hist shape>` is answerable
// for the bare-selector, sum()/avg(), and rate()/increase() histogram-
// valued shapes on either side, and composes with the scalar-scaling
// shape (#2087) and a further sum() — the answer is always a
// chplan.HistogramProjection publishing the same thirteen-column
// contract every other histogram-valued shape does.
func TestLower_ExpHistogram_HistogramBinopIsHistogramValued(t *testing.T) {
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
		{name: "two bare selectors, same metric", query: `latency_exp_hist + latency_exp_hist`},
		{name: "two bare selectors, different metrics", query: `latency_exp_hist + other_exp_hist`},
		{name: "subtraction", query: `latency_exp_hist - other_exp_hist`},
		{name: "sum() plus bare selector", query: `sum(latency_exp_hist) + other_exp_hist`},
		{name: "rate() plus rate()", query: `rate(latency_exp_hist[5m]) + rate(other_exp_hist[5m])`},
		{name: "parenthesised", query: `(latency_exp_hist + other_exp_hist)`},
		{name: "composes with scalar scaling", query: `(latency_exp_hist + other_exp_hist) * 2`},
		{name: "composes with sum()", query: `sum(latency_exp_hist + other_exp_hist)`},
		{
			name:  "range query",
			query: `latency_exp_hist + other_exp_hist`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
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
			lower := tc.lower
			if lower == nil {
				lower = func(e parser.Expr) (chplan.Node, error) {
					return promql.LowerAt(context.Background(), e, s, end, end)
				}
			}
			plan, err := lower(expr)
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
		})
	}
}

// TestLower_ExpHistogram_HistogramBinopRejectsNonDefaultMatching pins
// that on()/ignoring()/group_left()/group_right() are explicitly
// rejected for now (tracked as a follow-up, see this file's sibling
// histogram_native_binop.go) rather than silently mismatching series.
func TestLower_ExpHistogram_HistogramBinopRejectsNonDefaultMatching(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	queries := []string{
		`latency_exp_hist + on(service) other_exp_hist`,
		`latency_exp_hist + ignoring(service) other_exp_hist`,
		`latency_exp_hist + on(service) group_left() other_exp_hist`,
		`latency_exp_hist + on(service) group_right() other_exp_hist`,
	}

	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			_, err = promql.LowerAt(context.Background(), expr, s, at, at)
			if err == nil {
				t.Fatalf("lower(%q): expected an error, got none", query)
			}
		})
	}
}

// TestLower_ExpHistogram_HistogramBinopMergesViaUnionAllAndCountGuard
// pins the plan SHAPE the merge builds: a chplan.UnionAll of the two
// operands feeding a chplan.Aggregate grouped by Attributes with a
// `count() = 2` Having guard — the stand-in for VectorJoin's INNER JOIN
// default one-to-one matching (see histogram_native_binop.go's doc for
// why that guard alone is sufficient here). Subtraction additionally
// wraps the RHS arm in the existing scalar-scaling machinery (issue
// #2087) with op=Mul, scale=-1, negating every count-bearing field
// before it enters the same merge as `+`.
func TestLower_ExpHistogram_HistogramBinopMergesViaUnionAllAndCountGuard(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		query      string
		rhsNegated bool
	}{
		{name: "addition", query: `latency_exp_hist + other_exp_hist`, rhsNegated: false},
		{name: "subtraction", query: `latency_exp_hist - other_exp_hist`, rhsNegated: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", tc.query, err)
			}
			hp, ok := plan.(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.HistogramProjection", tc.query, plan)
			}
			reshape, ok := hp.Input.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): HistogramProjection.Input is %T, want *chplan.Project", tc.query, hp.Input)
			}
			agg, ok := reshape.Input.(*chplan.Aggregate)
			if !ok {
				t.Fatalf("lower(%q): reshape.Input is %T, want *chplan.Aggregate", tc.query, reshape.Input)
			}
			if agg.Having == nil {
				t.Fatalf("lower(%q): Aggregate.Having is nil, want the both-sides-matched count() = 2 guard", tc.query)
			}
			having, ok := agg.Having.(*chplan.Binary)
			if !ok || having.Op != chplan.OpEq {
				t.Fatalf("lower(%q): Aggregate.Having = %#v, want an Eq Binary", tc.query, agg.Having)
			}
			if _, ok := having.Left.(*chplan.FuncCall); !ok {
				t.Fatalf("lower(%q): Aggregate.Having.Left = %#v, want a count() FuncCall", tc.query, having.Left)
			}
			lit, ok := having.Right.(*chplan.LitInt)
			if !ok || lit.V != 2 {
				t.Fatalf("lower(%q): Aggregate.Having.Right = %#v, want LitInt(2)", tc.query, having.Right)
			}

			union, ok := agg.Input.(*chplan.UnionAll)
			if !ok || len(union.Inputs) != 2 {
				t.Fatalf("lower(%q): Aggregate.Input = %#v, want a two-arm UnionAll", tc.query, agg.Input)
			}
			if _, ok := union.Inputs[0].(*chplan.HistogramProjection); !ok {
				t.Fatalf("lower(%q): UnionAll.Inputs[0] is %T, want *chplan.HistogramProjection", tc.query, union.Inputs[0])
			}
			rhsArm, ok := union.Inputs[1].(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("lower(%q): UnionAll.Inputs[1] is %T, want *chplan.HistogramProjection", tc.query, union.Inputs[1])
			}
			_, rhsWrapped := rhsArm.Input.(*chplan.Project)
			if rhsWrapped != tc.rhsNegated {
				t.Fatalf("lower(%q): RHS arm wrapped in a negating Project = %v, want %v", tc.query, rhsWrapped, tc.rhsNegated)
			}
		})
	}
}

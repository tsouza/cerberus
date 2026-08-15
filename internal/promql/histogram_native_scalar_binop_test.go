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

// TestLower_ExpHistogram_ScalarBinopIsHistogramValued pins cerberus
// issue #2087: `<exp-hist shape> (*|/) <scalar>` is answerable over all
// three histogram-VALUED lowerings — a bare selector, sum()/avg(), and
// rate()/increase() — and the answer is a chplan.HistogramProjection
// publishing the same thirteen-column contract every other
// histogram-valued shape does.
//
// `+` / `-` / `^` / `%` and the comparison family stay rejected
// (TestLower_ExpHistogram_UnsupportedShapesRejectExplicitly's
// "sum under arithmetic" / "increase under arithmetic" cases): reference
// answers those with an empty result plus an info annotation rather than
// a value, so admitting them here would answer where reference does not.
func TestLower_ExpHistogram_ScalarBinopIsHistogramValued(t *testing.T) {
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
			name:  "bare selector times scalar",
			query: `latency_exp_hist * 2`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "scalar times bare selector (commutative)",
			query: `2 * latency_exp_hist`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "bare selector divided by scalar",
			query: `latency_exp_hist / 4`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "bare selector times scalar, range",
			query: `latency_exp_hist * 2`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, 30*time.Second)
			},
		},
		{
			name:  "sum divided by scalar",
			query: `sum(latency_exp_hist) / 2`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "sum by(...) times scalar",
			query: `sum by (service) (latency_exp_hist) * 3`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "avg divided by scalar",
			query: `avg(latency_exp_hist) / 2`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "rate times scalar",
			query: `rate(latency_exp_hist[5m]) * 2`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "increase divided by scalar",
			query: `increase(latency_exp_hist[5m]) / 2`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "rate times scalar, range",
			query: `rate(latency_exp_hist[5m]) * 2`,
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
		})
	}
}

// TestLower_ExpHistogram_ScalarBinopScalesOnlyTheCountFields pins the
// same split TestLower_ExpHistogram_AvgDividesOnlyTheCountFields pins for
// avg(): scaling a histogram by a PromQL scalar touches exactly the five
// COUNT-bearing fields — Count, Sum, ZeroCount, and both signed bucket
// ladders — and leaves Scale, ZeroThreshold and both bucket offsets
// alone, matching reference's FloatHistogram.Mul / .Div
// (histogram.FloatHistogram in the pinned fork).
//
// A plan that scaled the offsets too would still publish thirteen
// well-typed columns and still decode, which is why this checks the
// wrapping Project's expression SHAPES rather than only checking that
// lowering succeeds.
func TestLower_ExpHistogram_ScalarBinopScalesOnlyTheCountFields(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
		op    chplan.BinaryOp
	}{
		{name: "bare selector times scalar", query: `latency_exp_hist * 2`, op: chplan.OpMul},
		{name: "bare selector divided by scalar", query: `latency_exp_hist / 4`, op: chplan.OpDiv},
		{name: "sum divided by scalar", query: `sum(latency_exp_hist) / 2`, op: chplan.OpDiv},
		{name: "rate times scalar", query: `rate(latency_exp_hist[5m]) * 2`, op: chplan.OpMul},
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
			byAlias := make(map[string]chplan.Expr, len(reshape.Projections))
			for _, proj := range reshape.Projections {
				byAlias[proj.Alias] = proj.Expr
			}

			scaledScalar := func(alias string) {
				t.Helper()
				e, ok := byAlias[alias]
				if !ok {
					t.Fatalf("lower(%q): no projection for %q", tc.query, alias)
				}
				bin, ok := e.(*chplan.Binary)
				if !ok || bin.Op != tc.op {
					t.Fatalf("lower(%q): projection for %q = %#v, want a %s Binary", tc.query, alias, e, tc.op)
				}
				lit, ok := bin.Right.(*chplan.LitFloat)
				if !ok {
					t.Fatalf("lower(%q): projection for %q scales by %#v, want a LitFloat", tc.query, alias, bin.Right)
				}
				_ = lit
			}
			scaledLadder := func(alias string) {
				t.Helper()
				e, ok := byAlias[alias]
				if !ok {
					t.Fatalf("lower(%q): no projection for %q", tc.query, alias)
				}
				call, ok := e.(*chplan.FuncCall)
				if !ok || call.Name != "arrayMap" || len(call.Args) != 2 {
					t.Fatalf("lower(%q): projection for %q = %#v, want arrayMap(...)", tc.query, alias, e)
				}
				lambda, ok := call.Args[0].(*chplan.Lambda)
				if !ok {
					t.Fatalf("lower(%q): arrayMap's first arg for %q is %T, want *chplan.Lambda", tc.query, alias, call.Args[0])
				}
				bin, ok := lambda.Body.(*chplan.Binary)
				if !ok || bin.Op != tc.op {
					t.Fatalf("lower(%q): ladder lambda for %q = %#v, want a %s Binary", tc.query, alias, lambda.Body, tc.op)
				}
			}
			unscaled := func(alias string) {
				t.Helper()
				e, ok := byAlias[alias]
				if !ok {
					t.Fatalf("lower(%q): no projection for %q", tc.query, alias)
				}
				if _, isBinary := e.(*chplan.Binary); isBinary {
					t.Fatalf("lower(%q): position-bearing column %q was scaled: %#v", tc.query, alias, e)
				}
				if call, isCall := e.(*chplan.FuncCall); isCall && call.Name == "arrayMap" {
					t.Fatalf("lower(%q): position-bearing column %q was scaled: %#v", tc.query, alias, e)
				}
			}

			scaledScalar(s.CountColumn)
			scaledScalar(s.SumColumn)
			scaledScalar(s.ZeroCountColumn)
			scaledLadder(s.PositiveBucketCountsColumn)
			scaledLadder(s.NegativeBucketCountsColumn)
			unscaled(s.ScaleColumn)
			unscaled(s.PositiveOffsetColumn)
			unscaled(s.NegativeOffsetColumn)
		})
	}
}

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

// TestLower_ExpHistogram_IncompatibleFloatVectorBinopDropsSamples pins
// cerberus issue #2331: `<float-VECTOR shape> OP <exp-hist shape>` (or
// its mirror) — neither operand a compile-time scalar literal, and
// exactly one operand histogram-valued — is accepted and answers an
// empty result for the same op family
// TestLower_ExpHistogram_IncompatibleScalarBinopDropsSamples pins for a
// scalar LITERAL operand, rather than cerberus's previous hard rejection
// at expHistogramSelectorRouting's catch-all.
func TestLower_ExpHistogram_IncompatibleFloatVectorBinopDropsSamples(t *testing.T) {
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
		{name: "histogram plus float vector", query: `histogram_quantile(0.5, latency_exp_hist) + latency_exp_hist`},
		{name: "float vector plus histogram", query: `latency_exp_hist + histogram_quantile(0.5, latency_exp_hist)`},
		{name: "histogram minus float vector", query: `histogram_quantile(0.5, latency_exp_hist) - latency_exp_hist`},
		{name: "float vector minus histogram", query: `latency_exp_hist - histogram_quantile(0.5, latency_exp_hist)`},
		{name: "power", query: `histogram_quantile(0.5, latency_exp_hist) ^ latency_exp_hist`},
		{name: "modulo", query: `histogram_quantile(0.5, latency_exp_hist) % latency_exp_hist`},
		{name: "equal", query: `histogram_quantile(0.5, latency_exp_hist) == latency_exp_hist`},
		{name: "not equal", query: `histogram_quantile(0.5, latency_exp_hist) != latency_exp_hist`},
		{name: "greater", query: `histogram_quantile(0.5, latency_exp_hist) > latency_exp_hist`},
		{name: "less", query: `histogram_quantile(0.5, latency_exp_hist) < latency_exp_hist`},
		{name: "greater equal", query: `histogram_quantile(0.5, latency_exp_hist) >= latency_exp_hist`},
		{name: "less equal", query: `histogram_quantile(0.5, latency_exp_hist) <= latency_exp_hist`},
		{name: "bool comparison", query: `histogram_quantile(0.5, latency_exp_hist) > bool latency_exp_hist`},
		{name: "atan2", query: `histogram_quantile(0.5, latency_exp_hist) atan2 latency_exp_hist`},
		// Float-vector-on-left DIV of a histogram divisor drops (the
		// float side is never the numerator of a supported scaling
		// shape); histogram-left DIV of a float-vector divisor is the
		// UNSUPPORTED scaling direction and is covered separately by
		// TestLower_ExpHistogram_FloatVectorScalingShapesStillRejected.
		{name: "float vector divided by histogram", query: `histogram_quantile(0.5, latency_exp_hist) / latency_exp_hist`},
		{name: "parenthesised", query: `(histogram_quantile(0.5, latency_exp_hist)) + latency_exp_hist`},
		{name: "rate() plus float vector", query: `rate(latency_exp_hist[5m]) + histogram_quantile(0.5, latency_exp_hist)`},
		{name: "sum() minus float vector", query: `sum(latency_exp_hist) - histogram_quantile(0.5, latency_exp_hist)`},
		{
			name:  "range query",
			query: `histogram_quantile(0.5, latency_exp_hist) + latency_exp_hist`,
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
			filter, ok := plan.(*chplan.Filter)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want constant-false *chplan.Filter", tc.query, plan)
			}
			predicate, ok := filter.Predicate.(*chplan.LitBool)
			if !ok || predicate.V {
				t.Fatalf("lower(%q): filter predicate is %#v, want false literal", tc.query, filter.Predicate)
			}
			if shape := chplan.RowShapeOf(filter.Input); shape != chplan.HistogramRowShape {
				t.Fatalf("lower(%q): filtered input publishes %s, want histogram", tc.query, shape)
			}
		})
	}
}

// TestLower_ExpHistogram_FloatVectorScalingShapesStillRejected pins the
// deliberate boundary this file's header doc explains: MUL (either
// side) and histogram-left DIV between a histogram-valued operand and a
// genuine (non-literal) float-VECTOR operand are reference Prometheus's
// supported histogram-SCALING shapes, but cerberus only implements the
// scaling machinery for a compile-time scalar LITERAL — so these keep
// hitting expHistogramSelectorRouting's pre-existing catch-all
// rejection rather than being answered OR silently mis-dropped. A
// future issue can widen scaling to a genuine float-vector operand;
// until then, dropping these would answer empty where reference answers
// a scaled histogram, which is a worse divergence than a clean
// rejection.
func TestLower_ExpHistogram_FloatVectorScalingShapesStillRejected(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	queries := []string{
		`histogram_quantile(0.5, latency_exp_hist) * latency_exp_hist`,
		`latency_exp_hist * histogram_quantile(0.5, latency_exp_hist)`,
		`latency_exp_hist / histogram_quantile(0.5, latency_exp_hist)`,
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

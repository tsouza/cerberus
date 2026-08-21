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

func TestLower_ExpHistogram_FloatOnlyFunctionsDropSamples(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	queries := []string{
		`abs(latency_exp_hist)`,
		`ceil(latency_exp_hist)`,
		`floor(latency_exp_hist)`,
		`round(latency_exp_hist)`,
		`round(latency_exp_hist, 0.1)`,
		`sqrt(latency_exp_hist)`,
		`exp(latency_exp_hist)`,
		`ln(latency_exp_hist)`,
		`log2(latency_exp_hist)`,
		`log10(latency_exp_hist)`,
		`sgn(latency_exp_hist)`,
		`acos(latency_exp_hist)`,
		`acosh(latency_exp_hist)`,
		`asin(latency_exp_hist)`,
		`asinh(latency_exp_hist)`,
		`atan(latency_exp_hist)`,
		`atanh(latency_exp_hist)`,
		`cos(latency_exp_hist)`,
		`cosh(latency_exp_hist)`,
		`sin(latency_exp_hist)`,
		`sinh(latency_exp_hist)`,
		`tan(latency_exp_hist)`,
		`tanh(latency_exp_hist)`,
		`deg(latency_exp_hist)`,
		`rad(latency_exp_hist)`,

		// The consumer sees histogram-valued results, not only selectors.
		`abs(sum(latency_exp_hist))`,
		`abs(rate(latency_exp_hist[5m]))`,
		`abs(latency_exp_hist * 2)`,
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
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			assertEmptyFloatProjection(t, plan)
		})
	}
}

// TestLower_ExpHistogram_ClampFamilyDropsSamples pins issue #2345: reference
// Prometheus's shared clamp() helper (promql/functions.go) skips every
// sample whose H field is set — the same "process only float samples" rule
// simpleFloatFunc applies for abs/ceil/floor/round/etc — so clamp_max,
// clamp_min and the 3-arg clamp must drop a histogram-valued argument to an
// empty float vector rather than hard-reject it. Before the fix, the clamp
// family's vector arg never went through lowerExpHistogramValuedShape, so a
// histogram selector fell through to the generic lower() dispatch and hit
// expHistogramSelectorRouting's catch-all rejection.
func TestLower_ExpHistogram_ClampFamilyDropsSamples(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	queries := []string{
		`clamp_max(latency_exp_hist, 5)`,
		`clamp_min(latency_exp_hist, 0)`,
		`clamp(latency_exp_hist, 0, 5)`,

		// The consumer sees histogram-valued results, not only selectors.
		`clamp_max(sum(latency_exp_hist), 5)`,
		`clamp_max(rate(latency_exp_hist[5m]), 5)`,
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
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			assertEmptyFloatProjection(t, plan)
		})
	}
}

func TestLower_ExpHistogram_FloatOnlyFunctionRangeDropsSamples(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(`abs(latency_exp_hist)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan, err := promql.LowerAtRange(context.Background(), expr, s, start, start.Add(time.Minute), 15*time.Second)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	assertEmptyFloatProjection(t, plan)
}

func assertEmptyFloatProjection(t *testing.T, plan chplan.Node) {
	t.Helper()

	if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
		t.Fatalf("plan publishes %s, want sample", shape)
	}
	project, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan root = %T, want *chplan.Project", plan)
	}
	if len(project.Projections) != 4 {
		t.Fatalf("projection count = %d, want canonical sample quartet", len(project.Projections))
	}
	filter, ok := project.Input.(*chplan.Filter)
	if !ok {
		t.Fatalf("project input = %T, want *chplan.Filter", project.Input)
	}
	predicate, ok := filter.Predicate.(*chplan.LitBool)
	if !ok || predicate.V {
		t.Fatalf("filter predicate = %#v, want LitBool{false}", filter.Predicate)
	}
	if shape := chplan.RowShapeOf(filter.Input); shape != chplan.HistogramRowShape {
		t.Fatalf("filtered input publishes %s, want histogram", shape)
	}
}

package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestLower_ExpHistogram_DroppingAggregationsAreEmpty(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	queries := []string{
		`min by (service) (latency_exp_hist)`,
		`max without (instance) (latency_exp_hist)`,
		`stddev(latency_exp_hist)`,
		`stdvar(latency_exp_hist)`,
		`quantile(0.9, latency_exp_hist)`,
		`topk(3, latency_exp_hist)`,
		`bottomk(3, latency_exp_hist)`,
	}
	modes := []struct {
		name  string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{
			name: "instant",
			lower: func(expr parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), expr, s, end, end)
			},
		},
		{
			name: "range",
			lower: func(expr parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
			},
		},
	}

	for _, mode := range modes {
		for _, query := range queries {
			t.Run(mode.name+"/"+query, func(t *testing.T) {
				t.Parallel()
				expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
				if err != nil {
					t.Fatalf("ParseExpr(%q): %v", query, err)
				}
				plan, err := mode.lower(expr)
				if err != nil {
					t.Fatalf("lower(%q): %v", query, err)
				}
				filter, ok := plan.(*chplan.Filter)
				if !ok {
					t.Fatalf("lower(%q) root = %T, want *chplan.Filter", query, plan)
				}
				pred, ok := filter.Predicate.(*chplan.LitBool)
				if !ok || pred.V {
					t.Fatalf("lower(%q) predicate = %#v, want false literal", query, filter.Predicate)
				}
				if shape := chplan.RowShapeOf(filter.Input); shape != chplan.HistogramRowShape {
					t.Fatalf("lower(%q) filtered input shape = %s, want histogram", query, shape)
				}
			})
		}
	}
}

func TestLower_ExpHistogram_DroppingTopKPreservesParameterDomain(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	expr, err := p.ParseExpr(`topk(NaN, latency_exp_hist)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	_, err = promql.LowerAt(context.Background(), expr, s, at, at)
	if err == nil || !strings.Contains(err.Error(), "Parameter value is NaN") {
		t.Fatalf("LowerAt(topk NaN): error = %v, want parameter-domain rejection", err)
	}

	expr, err = p.ParseExpr(`bottomk(scalar(vector(2)), latency_exp_hist)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	var guards []promql.ScalarGuard
	plan, err := promql.LowerAtRangeOpts(
		context.Background(), expr, s, at, at.Add(time.Minute), time.Minute,
		promql.LowerOpts{Guards: &guards},
	)
	if err != nil {
		t.Fatalf("LowerAtRangeOpts: %v", err)
	}
	if _, ok := plan.(*chplan.Filter); !ok {
		t.Fatalf("plan root = %T, want empty-result Filter", plan)
	}
	if len(guards) != 1 || guards[0].Name != "bottomk K" {
		t.Fatalf("guards = %#v, want one bottomk K guard", guards)
	}
}

// TestLower_ExpHistogram_DroppingAggregationComposesUnderWrappers pins
// cerberus issue #2562: a "dropping" aggregation (min/max/stddev/stdvar/
// quantile/topk/bottomk) directly over a histogram-VALUED shape — not
// merely one of [droppingAggregationOverExpHistogram]'s ROOT-only matches,
// but NESTED as the argument of a further wrapper —
// `histogram_quantile(0.5, topk(1, demo_latency_exp_hist))`,
// `sum(min(demo_latency_exp_hist))`, `abs(stddev(rate(...)))`. Before this
// issue's fix, [lowerTopK] (topk/bottomk) and the five other ops' own
// generic `lower(a.Expr, ...)` fallback in [lowerAggregate] reached the
// histogram selector underneath without ANY histogram-aware recognizer,
// hard-rejecting via expHistogramSelectorRouting's catch-all instead of
// reference's real "drop the sample, evaluate to empty" answer — the same
// class TestLower_ExpHistogram_DroppingAggregationsAreEmpty already pins
// for the query-ROOT case.
func TestLower_ExpHistogram_DroppingAggregationComposesUnderWrappers(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	queries := []string{
		// The issue's own trigger query.
		`histogram_quantile(0.5, topk(1, latency_exp_hist))`,
		`histogram_quantile(0.9, bottomk(2, latency_exp_hist))`,
		// Every dropping op, nested under an ordinary wrapper.
		`sum(topk(3, latency_exp_hist))`,
		`sum(bottomk(3, latency_exp_hist))`,
		`abs(min(latency_exp_hist))`,
		`abs(max(latency_exp_hist))`,
		`sort(stddev(latency_exp_hist))`,
		`sort(stdvar(latency_exp_hist))`,
		`ceil(quantile(0.9, latency_exp_hist))`,
		// Two levels of composition, and a range-vector operand under the
		// dropping aggregation.
		`histogram_quantile(0.5, topk(1, rate(latency_exp_hist[5m])))`,
		`abs(sum(topk(3, latency_exp_hist)))`,
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
				t.Fatalf("LowerAt(%q): unexpected error (this exact shape hard-rejected via expHistogramSelectorRouting's catch-all before cerberus issue #2562's fix): %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("LowerAt(%q) row shape = %s, want sample (canonical float)", query, shape)
			}
			// The plan must actually be empty (a constant-false Filter
			// reachable somewhere in the tree), not merely float-shaped —
			// distinguishing "recognised and dropped" from a lowering
			// that accidentally answers a non-empty float result.
			foundEmptyFilter := false
			chplan.Walk(plan, func(n chplan.Node) bool {
				if f, ok := n.(*chplan.Filter); ok {
					if lit, ok := f.Predicate.(*chplan.LitBool); ok && !lit.V {
						foundEmptyFilter = true
					}
				}
				return true
			})
			if !foundEmptyFilter {
				t.Fatalf("LowerAt(%q) plan has no constant-false Filter; want a dropped (empty) result:\n%#v", query, plan)
			}
		})
	}
}

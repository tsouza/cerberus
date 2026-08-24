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

// TestLower_ExpHistogram_DroppingShapeComposesUnderWrappers pins cerberus
// issue #2528: a nested exp-histogram "drop"-family binop
// (`latency_exp_hist + 1`, reference's incompatible-types drop) used as
// the ARGUMENT of a wrapper — an aggregation of any operator, sort(),
// limitk(), a unary math function, clamp, round(v, n), scalar(),
// info(), label_replace()/label_join(), timestamp(), a date-component
// function — now answers reference's real "drop the sample, evaluate to
// empty" result instead of hard-rejecting via
// expHistogramSelectorRouting's catch-all. Every case here previously
// errored before this issue's fix.
func TestLower_ExpHistogram_DroppingShapeComposesUnderWrappers(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	queries := []string{
		// Aggregations of every reducer family, not just the ones
		// reference's own evaluator ignores histograms for outright.
		`sum(latency_exp_hist + 1)`,
		`avg(latency_exp_hist + 1)`,
		`count(latency_exp_hist + 1)`,
		`group(latency_exp_hist + 1)`,
		`min(latency_exp_hist + 1)`,
		`max(latency_exp_hist + 1)`,
		`stddev(latency_exp_hist + 1)`,
		`stdvar(latency_exp_hist + 1)`,
		`quantile(0.9, latency_exp_hist + 1)`,
		`topk(3, latency_exp_hist + 1)`,
		`bottomk(3, latency_exp_hist + 1)`,
		`count_values("v", latency_exp_hist + 1)`,
		`sum by (service) (latency_exp_hist + 1)`,
		// The other two drop-family binop legs, also wrapped. `*` between
		// two histogram-valued shapes is the histogram/histogram DROP
		// leg (expHistogramDroppingHistogramBinop); `+`/`-` between two
		// is the disjoint MERGE leg (expHistogramHistogramBinop) instead,
		// so it is deliberately not used here.
		`sum(latency_exp_hist * latency_exp_hist)`,
		`sum(latency_exp_hist + scalar(vector(1)))`,
		// limitk/limit_ratio: their own dedicated opt-in
		// (lowerLimitKInput), not the generic aggregation branch.
		`limitk(3, latency_exp_hist + 1)`,
		`limit_ratio(0.5, latency_exp_hist + 1)`,
		// Call-based wrappers.
		`sort(latency_exp_hist + 1)`,
		`sort_desc(latency_exp_hist + 1)`,
		`sort_by_label(latency_exp_hist + 1, "service")`,
		`abs(latency_exp_hist + 1)`,
		`ceil(latency_exp_hist + 1)`,
		`clamp_min(latency_exp_hist + 1, 0)`,
		`clamp(latency_exp_hist + 1, 0, 100)`,
		`round(latency_exp_hist + 1, 5)`,
		`info(latency_exp_hist + 1)`,
		`label_replace(latency_exp_hist + 1, "copy", "$1", "service", "(.*)")`,
		`label_join(latency_exp_hist + 1, "copy", "-", "service")`,
		`timestamp(latency_exp_hist + 1)`,
		`day_of_month(latency_exp_hist + 1)`,
		// Two levels of composition: an outer wrapper around an inner
		// wrapper around the drop-family leaf.
		`abs(sum(latency_exp_hist + 1))`,
		`sort(topk(3, latency_exp_hist + 1))`,
		`sort(limitk(3, latency_exp_hist + 1))`,
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

// TestLower_ExpHistogram_DroppingShapeScalarArgAnswersNaN pins
// `scalar(<drop-family shape>)`, which — unlike every other wrapper in
// TestLower_ExpHistogram_DroppingShapeComposesUnderWrappers — does not
// stay a zero-row vector: scalar()'s own top-level lowering always
// materialises exactly one synthetic row (the reference
// count()==1 ? value : NaN reduction), so an already-empty argument
// answers a single NaN sample rather than an empty result. This exercises
// the SAME threaded check (lowerScalarVectorArg) as the vector-returning
// wrappers, just confirms the different observable shape scalar()'s own
// wrapping reduction was already designed to produce.
func TestLower_ExpHistogram_DroppingShapeScalarArgAnswersNaN(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	expr, err := p.ParseExpr(`scalar(latency_exp_hist + 1)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt: unexpected error: %v", err)
	}
	if _, ok := plan.(*chplan.Project); !ok {
		t.Fatalf("plan root = %T, want *chplan.Project (the synthetic one-row scalar shape)", plan)
	}
}

// TestLower_ExpHistogram_DroppingShapeRangeMode pins the same nested
// composition in range-query mode, where the drop-family recognisers
// gate on ctx.step rather than the instant default.
func TestLower_ExpHistogram_DroppingShapeRangeMode(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)

	queries := []string{
		`sum(latency_exp_hist + 1)`,
		`topk(3, latency_exp_hist + 1)`,
		`sort(latency_exp_hist + 1)`,
		`limitk(3, latency_exp_hist + 1)`,
	}
	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): unexpected error: %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("LowerAtRange(%q) row shape = %s, want sample (canonical float)", query, shape)
			}
		})
	}
}

// TestLower_ExpHistogram_DroppingShapeAggregationValidatesParamsFirst pins
// that the new generic "aggregation wraps a drop-family argument" branch
// still validates the aggregation's own parameter BEFORE answering empty
// — reference validates K/phi/the count_values label before ever walking
// input samples, so an invalid parameter must surface as an error even
// though the argument is already empty. Mirrors
// TestLower_ExpHistogram_DroppingTopKPreservesParameterDomain for the
// (already-shipped) histogram-VALUED-argument case.
func TestLower_ExpHistogram_DroppingShapeAggregationValidatesParamsFirst(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{name: "topk NaN K", query: `topk(NaN, latency_exp_hist + 1)`, wantErr: "Parameter value is NaN"},
		{name: "limitk NaN K", query: `limitk(NaN, latency_exp_hist + 1)`, wantErr: "Parameter value is NaN"},
		{name: "count_values empty label", query: `count_values("", latency_exp_hist + 1)`, wantErr: "count_values"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			_, err = promql.LowerAt(context.Background(), expr, s, at, at)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LowerAt(%q): error = %v, want it to contain %q", tc.query, err, tc.wantErr)
			}
		})
	}
}

// TestLower_ExpHistogram_DroppingShapeStructure pins the exact plan
// shape lowerExpHistogramDroppingShape builds for a representative
// wrapper: a Project (the canonical float-Sample quartet) over a
// constant-false Filter over the histogram-valued leaf's own lowering —
// the same shape dropExpHistogramSamples already produces for the
// "preserve" family, confirming the two families answer an empty result
// identically once composed under a wrapper.
func TestLower_ExpHistogram_DroppingShapeStructure(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	expr, err := p.ParseExpr(`sum(latency_exp_hist + 1)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	project, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan root = %T, want *chplan.Project", plan)
	}
	if len(project.Projections) != 4 {
		t.Fatalf("Project has %d projections, want the canonical float quartet (4)", len(project.Projections))
	}
	filter, ok := project.Input.(*chplan.Filter)
	if !ok {
		t.Fatalf("Project.Input = %T, want *chplan.Filter", project.Input)
	}
	lit, ok := filter.Predicate.(*chplan.LitBool)
	if !ok || lit.V {
		t.Fatalf("Filter.Predicate = %#v, want false literal", filter.Predicate)
	}
	if shape := chplan.RowShapeOf(filter.Input); shape != chplan.HistogramRowShape {
		t.Fatalf("Filter.Input row shape = %s, want histogram", shape)
	}
}

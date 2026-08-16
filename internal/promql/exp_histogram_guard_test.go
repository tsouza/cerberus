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

// TestLower_ExpHistogram_UnsupportedShapesRejectExplicitly pins issue #1704:
// every PromQL shape over a pinned exponential-histogram selector OTHER
// than histogram_quantile() / histogram_count() / histogram_sum(), the
// `_count` / `_sum` companion selectors, and the three top-level
// histogram-VALUED shapes issue #1967 answers — a BARE selector
// (TestLower_ExpHistogram_BareSelectorIsHistogramValued), `sum()` over
// one and its `avg()` twin (TestLower_ExpHistogram_SumIsHistogramValued,
// TestLower_ExpHistogram_AvgIsHistogramValued), the five native-histogram-
// valued range functions over one
// (TestLower_ExpHistogram_RangeFunctionsAreHistogramValued), and float-only
// functions, which accept and drop histogram samples (issue #2221) — must
// fail lowering
// with a clear error, never silently resolve against the Gauge/Sum tables
// and return an empty-but-200 result. Before the fix, TablesFor never
// yielded ExpHistogramTable for these shapes, so cerberus quietly scanned
// the wrong tables and matched zero rows.
//
// The nested cases are what keeps the #1967 narrowing HONEST. Each
// answerable shape is answerable because of what sits ABOVE it — nothing,
// for a bare selector; a bucket-ladder merge rather than a `Value` read,
// for `sum()` — so each case here wraps one of them in a consumer that
// does read a `Value` column the histogram row shape never publishes, or
// hands `sum()` an aggregand that is not a bare selector. They stay
// rejected until each grows its own histogram-aware lowering.
//
// `count` is deliberately NOT in that list any more, and the difference
// is reference's own: its aggregation switch reaches `group.groupCount++`
// with no guard on the sample's value and raises no
// HistogramIgnoredInAggregation annotation, because counting a sample
// never has to look at it. That is why it is answered
// (histogram_native_count.go) while its five neighbours are not.
func TestLower_ExpHistogram_UnsupportedShapesRejectExplicitly(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name  string
		query string
	}{
		{name: "raw range vector", query: `latency_exp_hist[5m]`},
		{name: "subquery", query: `max_over_time(latency_exp_hist[5m:1m])`},

		// `sum()` over a bare selector IS answered (see
		// TestLower_ExpHistogram_SumIsHistogramValued). These are the
		// shapes the narrowing must NOT reach: a `sum` whose aggregand is
		// not a bare selector, and a `sum` that is not the root.
		{name: "sum over rate", query: `sum(rate(latency_exp_hist[5m]))`},
		{name: "sum over increase", query: `sum(increase(latency_exp_hist[5m]))`},
		{name: "sum over delta", query: `sum(delta(latency_exp_hist[5m]))`},
		{name: "sum over irate", query: `sum(irate(latency_exp_hist[5m]))`},
		{name: "sum over idelta", query: `sum(idelta(latency_exp_hist[5m]))`},
		{name: "sum of sum", query: `sum(sum(latency_exp_hist))`},
		{name: "sum under topk", query: `topk(3, sum by (service) (latency_exp_hist))`},

		// The five histogram-valued range functions over a selector ARE answered
		// (see TestLower_ExpHistogram_RangeFunctionsAreHistogramValued). These are the
		// shapes that narrowing must NOT reach: a rate under a consumer that
		// reads a Value, and a rate under an aggregation — which needs the
		// across-series merge stacked on top of the window reduction, so
		// answering it with the window reduction alone would silently drop
		// the sum.
		{name: "rate under topk", query: `topk(3, rate(latency_exp_hist[5m]))`},
		{name: "rate under avg", query: `avg(rate(latency_exp_hist[5m]))`},
		{name: "rate over subquery", query: `rate(latency_exp_hist[5m:1m])`},
		{name: "delta under label_replace", query: `label_replace(delta(latency_exp_hist[5m]), "a", "b", "service", "(.*)")`},
		{name: "idelta under topk", query: `topk(3, idelta(latency_exp_hist[5m]))`},
		{name: "delta over subquery", query: `delta(latency_exp_hist[5m:1m])`},
		{name: "irate over subquery", query: `irate(latency_exp_hist[5m:1m])`},
		{name: "idelta over subquery", query: `idelta(latency_exp_hist[5m:1m])`},

		// `resets()` / `changes()` / `count()` over a bare selector ARE
		// answered (see TestLower_ExpHistogram_ResetsChangesCountAreFloatValued).
		// Their answer is an ordinary float sample, so unlike the
		// histogram-valued shapes above nothing about the RESULT stops a
		// consumer from reading it. What keeps these rejected is the
		// ARGUMENT: the selector nested under them still reaches
		// lowerVectorSelector through the ordinary descent, where it is
		// still an exp-histogram selector with no Value column.
		{name: "resets under arithmetic", query: `resets(latency_exp_hist[5m]) * 2`},
		{name: "resets under sum", query: `sum(resets(latency_exp_hist[5m]))`},
		{name: "changes under arithmetic", query: `changes(latency_exp_hist[5m]) + 1`},
		{name: "changes under abs", query: `abs(changes(latency_exp_hist[5m]))`},
		{name: "resets over subquery", query: `resets(latency_exp_hist[5m:1m])`},
		{name: "changes over subquery", query: `changes(latency_exp_hist[5m:1m])`},
		{name: "count under arithmetic", query: `count(latency_exp_hist) + 1`},
		{name: "count of count", query: `count(count(latency_exp_hist))`},
		{name: "count over rate", query: `count(rate(latency_exp_hist[5m]))`},
		{name: "count_values", query: `count_values("v", latency_exp_hist)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			_, err = promql.Lower(context.Background(), expr, s)
			if err == nil {
				t.Fatalf("Lower(%q): expected an explicit rejection error, got nil (silent empty result)", tc.query)
			}
			if !strings.Contains(err.Error(), "latency_exp_hist") {
				t.Fatalf("Lower(%q): error %q does not name the offending metric", tc.query, err.Error())
			}
			if !strings.Contains(err.Error(), "exponential histogram") {
				t.Fatalf("Lower(%q): error %q does not explain the exp-histogram shape mismatch", tc.query, err.Error())
			}
		})
	}
}

// TestLower_ExpHistogram_LabelReplaceIsHistogramValued pins issue #2219:
// label_replace changes only labels, so it must preserve the histogram
// payload and the root HistogramRowShape for every supported
// histogram-valued operand. The guarded Aggregate is part of the contract:
// without it a non-injective rewrite could publish two series with the same
// output label set, which reference Prometheus rejects.
func TestLower_ExpHistogram_LabelReplaceIsHistogramValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	for _, tc := range []struct {
		name  string
		query string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{
			name:  "bare instant",
			query: `label_replace(latency_exp_hist, "copy", "$1-copy", "service", "(.*)")`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "sum instant",
			query: `label_replace(sum by (service) (latency_exp_hist), "copy", "$1-copy", "service", "(.*)")`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "rate range",
			query: `label_replace(rate(latency_exp_hist[5m]), "copy", "$1-copy", "service", "(.*)")`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := tc.lower(expr)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("RowShapeOf(lower(%q)) = %s, want %s", tc.query, shape, chplan.HistogramRowShape)
			}
			hp, ok := plan.(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("lower(%q) root = %T, want *chplan.HistogramProjection", tc.query, plan)
			}
			guard, ok := hp.Input.(*chplan.Aggregate)
			if !ok {
				t.Fatalf("lower(%q) HistogramProjection.Input = %T, want duplicate-labelset *chplan.Aggregate", tc.query, hp.Input)
			}
			if guard.Having == nil {
				t.Fatalf("lower(%q) duplicate-labelset Aggregate has nil Having", tc.query)
			}
			if len(guard.AggFuncs) != 10 {
				t.Fatalf("lower(%q) guard carries %d aggregates, want Value plus nine histogram fields", tc.query, len(guard.AggFuncs))
			}
		})
	}
}

// TestLower_ExpHistogram_CompanionSuffixesStillWork pins the shapes that
// MUST keep resolving after the #1704 fix: histogram_quantile() /
// histogram_count() / histogram_sum() against a bare exp-histogram
// selector, all of which detect the exp-histogram shape via their own
// dedicated lowering before ever reaching the generic vector-selector
// path this fix changed.
func TestLower_ExpHistogram_DedicatedFunctionsStillWork(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	queries := []string{
		`histogram_quantile(0.9, latency_exp_hist)`,
		`histogram_count(latency_exp_hist)`,
		`histogram_sum(latency_exp_hist)`,
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			if _, err := promql.Lower(context.Background(), expr, s); err != nil {
				t.Fatalf("Lower(%q): unexpected error: %v", q, err)
			}
		})
	}
}

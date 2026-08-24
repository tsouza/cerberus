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
// `_count` / `_sum` companion selectors, and the top-level
// histogram-VALUED shapes issue #1967 (and its follow-ons) answer — a BARE
// selector (TestLower_ExpHistogram_BareSelectorIsHistogramValued), a bare
// top-level RANGE-VECTOR selector
// (TestLower_ExpHistogram_BareMatrixSelectorIsHistogramValued, cerberus
// issue #2548), `sum()` over one and its `avg()` twin
// (TestLower_ExpHistogram_SumIsHistogramValued,
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
//
// `resets()` / `changes()` / `count()` / `group()` wrapped by a further
// scalar op, aggregation or instant math function used to stay rejected
// too — cerberus issue #2549 — even though their own answer is an
// ordinary float sample nothing about the wrapper would object to; what
// tripped the rejection was the ARGUMENT, which reached
// [lowerVectorSelector] through the ordinary descent with no
// histogram-aware retry. [lowerRangeVectorCall] (resets/changes) and
// [lowerAggregate] (count/group) now retry those two recognisers at
// every level of nesting the same way a query's own root always could,
// so every case that moved out of this file's own rejection list lives
// in TestLower_ExpHistogram_ResetsChangesCountAreFloatValued instead —
// see that test's own "wrapped" cases.
//
// The subquery form (`resets(<selector>[range:step])`) is a genuinely
// DIFFERENT shape from a wrapper — the selector's own outer range-vector
// function IS resets()/changes() itself, routed through
// lowerOuterRangeFnOverSubquery rather than lowerRangeVectorCall — and
// stays rejected here: #2549's fix never touches that path.
func TestLower_ExpHistogram_UnsupportedShapesRejectExplicitly(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name  string
		query string
	}{
		// A bare top-level raw range vector (`latency_exp_hist[5m]`) is
		// answered since cerberus issue #2548 — see
		// TestLower_ExpHistogram_BareMatrixSelectorIsHistogramValued — so
		// it is deliberately no longer a case here.
		{name: "subquery", query: `max_over_time(latency_exp_hist[5m:1m])`},
		{name: "resets over subquery", query: `resets(latency_exp_hist[5m:1m])`},
		{name: "changes over subquery", query: `changes(latency_exp_hist[5m:1m])`},
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

package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_UnsupportedShapesRejectExplicitly pins issue #1704:
// every PromQL shape over a pinned exponential-histogram selector OTHER
// than histogram_quantile() / histogram_count() / histogram_sum() and the
// `_count` / `_sum` companion selectors must fail lowering with a clear
// error, never silently resolve against the Gauge/Sum tables and return an
// empty-but-200 result. Before the fix, TablesFor never yielded
// ExpHistogramTable for these shapes, so cerberus quietly scanned the
// wrong tables and matched zero rows.
func TestLower_ExpHistogram_UnsupportedShapesRejectExplicitly(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name  string
		query string
	}{
		{name: "bare selector", query: `latency_exp_hist`},
		{name: "bare selector with label matcher", query: `latency_exp_hist{service="api"}`},
		{name: "rate", query: `rate(latency_exp_hist[5m])`},
		{name: "resets", query: `resets(latency_exp_hist[5m])`},
		{name: "changes", query: `changes(latency_exp_hist[5m])`},
		{name: "increase", query: `increase(latency_exp_hist[5m])`},
		{name: "sum aggregation", query: `sum(latency_exp_hist)`},
		{name: "sum by", query: `sum by (service) (latency_exp_hist)`},
		{name: "absent_over_time", query: `absent_over_time(latency_exp_hist[5m])`},
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

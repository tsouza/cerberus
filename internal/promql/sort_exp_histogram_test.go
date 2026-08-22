package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_SortFamilyDropsSamples pins issue #2456: reference
// Prometheus's funcSort/funcSortDesc (promql/functions.go) both start from
// filterFloats(vectorVals[0]), which drops every sample whose H field is
// set before sorting the remainder — the same "process only float samples"
// rule the instant-math functions (#2221) and the clamp family (#2345)
// apply. sort()/sort_desc() never got the same treatment: lowerSort routed
// its vector arg straight through the generic lower() dispatch with no
// lowerExpHistogramValuedShape check, so a bare exp-histogram selector
// argument fell through to expHistogramSelectorRouting's catch-all
// rejection instead of Prom's drop-and-answer-empty semantics.
func TestLower_ExpHistogram_SortFamilyDropsSamples(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	queries := []string{
		`sort(latency_exp_hist)`,
		`sort_desc(latency_exp_hist)`,

		// The consumer sees histogram-valued results, not only selectors.
		`sort(sum(latency_exp_hist))`,
		`sort_desc(rate(latency_exp_hist[5m]))`,
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

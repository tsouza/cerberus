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

// TestLower_ExpHistogram_SortByLabelFamilyPreservesSamples pins issue
// #2462. Unlike sort()/sort_desc() (#2456), reference Prometheus's
// funcSortByLabel/funcSortByLabelDesc (promql/functions.go) do NOT start
// from filterFloats: they sort the vector in place by the named labels'
// VALUES alone, so a histogram sample sorts (and survives) right
// alongside float samples — verified directly against the vendored
// reference engine (see sort.go's lowerSortByLabel doc comment for the
// promqltest probe). `lowerSortByLabel` still needs a
// lowerExpHistogramValuedShape check because, before this fix, an
// exp-histogram argument fell through the generic lower() dispatch and
// hit expHistogramSelectorRouting's catch-all rejection before label-sort
// logic was ever reached — but the correct answer on recognising the
// shape is to PRESERVE it (ordered by label, chplan.HistogramRowShape
// forwarded through chplan.OrderBy), never to drop it to an empty float
// vector.
func TestLower_ExpHistogram_SortByLabelFamilyPreservesSamples(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	queries := []string{
		`sort_by_label(latency_exp_hist, "job")`,
		`sort_by_label_desc(latency_exp_hist, "job")`,

		// No label list: funcSortByLabel's zero-comparator stable sort —
		// still histogram-valued, no OrderBy wraps it.
		`sort_by_label(latency_exp_hist)`,

		// Multiple tie-breaker labels.
		`sort_by_label(latency_exp_hist, "job", "instance")`,

		// The consumer sees histogram-valued results, not only selectors.
		`sort_by_label(sum(latency_exp_hist), "job")`,
		`sort_by_label_desc(rate(latency_exp_hist[5m]), "job")`,
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			expr := parseExprExp(t, query)
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("LowerAt(%q) plan publishes %s, want histogram — the sample must be preserved, not dropped", query, shape)
			}
		})
	}
}

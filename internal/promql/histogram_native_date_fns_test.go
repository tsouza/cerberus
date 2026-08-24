package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_DateComponentFunctionsDropSamples pins issue #2498:
// reference Prometheus's shared dateWrapper helper (promql/functions.go),
// used by year/month/day_of_month/day_of_year/day_of_week/days_in_month/
// hour/minute, explicitly skips every sample whose H field is set before
// computing the date component from F — the same "process only float
// samples" rule simpleFloatFunc applies for abs/ceil/floor/round/etc
// (issue #2221) and the clamp/sort families already mirror (issues #2345,
// #2456). Before this fix none of these eight routed their argument
// through lowerExpHistogramValuedShape, so a histogram-valued argument fell
// through to the generic lower() dispatch and hit
// expHistogramSelectorRouting's catch-all rejection instead of Prom's
// drop-and-answer-empty semantics.
//
// timestamp() is deliberately excluded here: its value is independent of
// the sample's payload (reference reads only Point.T), so it was already
// fixed separately by issue #2474/#2478 to ANSWER over a histogram
// argument rather than drop it — see histogram_native_timestamp_test.go.
func TestLower_ExpHistogram_DateComponentFunctionsDropSamples(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	queries := []string{
		`year(latency_exp_hist)`,
		`month(latency_exp_hist)`,
		`day_of_month(latency_exp_hist)`,
		`day_of_year(latency_exp_hist)`,
		`day_of_week(latency_exp_hist)`,
		`days_in_month(latency_exp_hist)`,
		`hour(latency_exp_hist)`,
		`minute(latency_exp_hist)`,

		// The consumer sees histogram-valued results, not only selectors.
		`year(sum(latency_exp_hist))`,
		`hour(rate(latency_exp_hist[5m]))`,
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
			if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
				t.Fatalf("chsql.Emit(%q): %v", query, err)
			}
		})
	}
}

// TestLower_ExpHistogram_DateComponentFunctionRangeDropsSamples covers
// range-query mode: a bare exp-histogram selector under a date-component
// function still drops to an empty float vector rather than rejecting, one
// representative function of the eight (day_of_week exercises the modulo
// rewrite, the most distinctive of the eight expressions).
func TestLower_ExpHistogram_DateComponentFunctionRangeDropsSamples(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(`day_of_week(latency_exp_hist)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan, err := promql.LowerAtRange(context.Background(), expr, s, start, start.Add(time.Minute), 15*time.Second)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	assertEmptyFloatProjection(t, plan)
	if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}
}

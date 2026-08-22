package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_FloatOnlyRangeVectorReducersDropSamples pins issue
// #2477: reference Prometheus's seven float-only range-vector reducers —
// predict_linear(), double_exponential_smoothing() (and its holt_winters
// alias), deriv(), mad_over_time(), quantile_over_time(), stddev_over_time()
// and stdvar_over_time() — each read only Point.F from their lookback
// window (tsouza/prometheus's promql/functions.go: funcPredictLinear,
// funcDoubleExponentialSmoothing, funcDeriv, funcMadOverTime,
// funcQuantileOverTime, varianceOverTime) and answer an empty vector — no
// error — when the window holds exclusively histogram samples. None of the
// seven had the lowerExpHistogramValuedShape / dropExpHistogramSamples
// check the instant math functions (#2221), the clamp family (#2345) and
// sort()/sort_desc() (#2456) already apply, so a bare exp-histogram matrix
// selector fell through to expHistogramSelectorRouting's catch-all
// rejection instead of Prom's drop-and-answer-empty semantics.
//
// min_over_time() / max_over_time() (compareOverTime) join the same drop
// class here per cerberus issue #2480: `if len(samples.Floats) == 0 {
// return enh.Out, nil }` runs before compareOverTime ever reads
// samples.Histograms, so an all-histogram window answers empty exactly
// like the other five. #2480's own filing verified this directly against
// tsouza/prometheus's promql/functions.go rather than assuming it from
// the shared aggrOverTime plumbing — count_over_time / present_over_time
// use that same plumbing but do NOT guard on Floats, so they deliberately
// get their own lowering (histogram_native_count_present_over_time.go)
// instead of joining this list.
//
// mad_over_time / double_exponential_smoothing / holt_winters are
// experimental functions in the reference parser, so every query here
// parses through [parseExprExp] (defined in sort_by_label_test.go) rather
// than a plain parser.Options{}.
func TestLower_ExpHistogram_FloatOnlyRangeVectorReducersDropSamples(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	queries := []string{
		`predict_linear(latency_exp_hist[5m], 60)`,
		// `holt_winters(...)` is the legacy spelling of
		// double_exponential_smoothing(); the reference parser no longer
		// accepts that name at all (removed upstream — see
		// internal/promql/subquery.go's own note), so only the current
		// spelling is reachable from PromQL text. lowerHoltWinters (the
		// shared lowering both spellings dispatch through, keyed on IR
		// Func="holt_winters" regardless of source spelling) is exercised
		// via this query either way.
		`double_exponential_smoothing(latency_exp_hist[5m], 0.5, 0.5)`,
		`deriv(latency_exp_hist[5m])`,
		`mad_over_time(latency_exp_hist[5m])`,
		`quantile_over_time(0.5, latency_exp_hist[5m])`,
		`stddev_over_time(latency_exp_hist[5m])`,
		`stdvar_over_time(latency_exp_hist[5m])`,
		`min_over_time(latency_exp_hist[5m])`,
		`max_over_time(latency_exp_hist[5m])`,

		// A computed (non-literal) parameter must not bypass the check:
		// the drop happens before the parameter is even resolved.
		`predict_linear(latency_exp_hist[5m], scalar(vector(60)))`,
		`quantile_over_time(scalar(vector(0.9)), latency_exp_hist[5m])`,

		// An `@`-pinned broadcast reaches the same drop path — the
		// histogram-selector recognition happens before the grid-shape
		// switch.
		`deriv(latency_exp_hist[5m] @ 1767225601)`,
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
			assertEmptyFloatProjection(t, plan)
		})
	}
}

// TestLower_ExpHistogram_FloatOnlyRangeVectorReducersRangeMode pins the
// query_range shape for the same seven reducers: LowerAtRange threads a
// non-zero step, which would otherwise route through the gridFanout /
// applyStepGridFanout branch these reducers never reach once the
// histogram-selector check fires first.
func TestLower_ExpHistogram_FloatOnlyRangeVectorReducersRangeMode(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := time.Minute

	queries := []string{
		`predict_linear(latency_exp_hist[5m], 60)`,
		`double_exponential_smoothing(latency_exp_hist[5m], 0.5, 0.5)`,
		`deriv(latency_exp_hist[5m])`,
		`mad_over_time(latency_exp_hist[5m])`,
		`quantile_over_time(0.5, latency_exp_hist[5m])`,
		`stddev_over_time(latency_exp_hist[5m])`,
		`stdvar_over_time(latency_exp_hist[5m])`,
		`min_over_time(latency_exp_hist[5m])`,
		`max_over_time(latency_exp_hist[5m])`,
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			expr := parseExprExp(t, query)
			plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, step)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): %v", query, err)
			}
			assertEmptyFloatProjection(t, plan)
		})
	}
}

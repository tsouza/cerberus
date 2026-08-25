package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_DropFamilyEmptyOverSubqueryInner pins cerberus
// issue #2590: the eleven float-only range-vector reducers
// [dropExpHistogramSamplesForRangeVector] (range_fns.go, cerberus issue
// #2528) already answers empty for over a BARE histogram selector
// (`max_over_time(latency_exp_hist[5m])`) hard-rejected instead when the
// SAME call became a bare subquery's own inner expression
// (`max_over_time(latency_exp_hist[5m])[10m:1m]`).
// [lowerHistogramNativeSubqueryInner] (subquery.go, cerberus issue #2543)
// gave a subquery's inner the same first-chance histogram-native routing a
// query's root gets, but only ever retried the PRESERVE family
// ([lowerHistogramNativeRoot]) — the DROP family's range-vector-Call shape
// never had a matching recogniser in that chain, so
// [lowerSubqueryOverCall]'s own generic RangeWindow-reducer path took over
// and called lowerVectorSelector directly on the bare exp-histogram
// selector, which expHistogramSelectorRouting's catch-all rejects. This is
// the SUBQUERY-INNER sibling of cerberus issue #2563's mirror-image
// "outer wraps subquery" fix pinned by
// TestLower_ExpHistogram_DropFamilyEmptyOverSubquery just above.
func TestLower_ExpHistogram_DropFamilyEmptyOverSubqueryInner(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "max_over_time", query: `max_over_time(latency_exp_hist[5m])[10m:1m]`},
		{name: "min_over_time", query: `min_over_time(latency_exp_hist[5m])[10m:1m]`},
		{name: "stddev_over_time", query: `stddev_over_time(latency_exp_hist[5m])[10m:1m]`},
		{name: "stdvar_over_time", query: `stdvar_over_time(latency_exp_hist[5m])[10m:1m]`},
		{name: "mad_over_time", query: `mad_over_time(latency_exp_hist[5m])[10m:1m]`},
		{name: "deriv", query: `deriv(latency_exp_hist[5m])[10m:1m]`},
		{name: "ts_of_max_over_time", query: `ts_of_max_over_time(latency_exp_hist[5m])[10m:1m]`},
		{name: "ts_of_min_over_time", query: `ts_of_min_over_time(latency_exp_hist[5m])[10m:1m]`},
		{name: "quantile_over_time", query: `quantile_over_time(0.5, latency_exp_hist[5m])[10m:1m]`},
		{name: "predict_linear", query: `predict_linear(latency_exp_hist[5m], 60)[10m:1m]`},
		{name: "holt_winters", query: `double_exponential_smoothing(latency_exp_hist[5m], 0.5, 0.5)[10m:1m]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr := parseExprExp(t, tc.query)

			plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
			if err != nil {
				t.Fatalf("lower(%q) instant: %v", tc.query, err)
			}
			if got := chplan.RowShapeOf(plan); got != chplan.SampleRowShape {
				t.Errorf("lower(%q) instant RowShape = %s, want %s", tc.query, got, chplan.SampleRowShape)
			}

			rplan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
			if err != nil {
				t.Fatalf("lower(%q) range: %v", tc.query, err)
			}
			if got := chplan.RowShapeOf(rplan); got != chplan.SampleRowShape {
				t.Errorf("lower(%q) range RowShape = %s, want %s", tc.query, got, chplan.SampleRowShape)
			}
		})
	}
}

// TestLower_ExpHistogram_DropFamilyInnerOverSubquery_ScalarParamStillValidated
// mirrors TestLower_ExpHistogram_DropFamilyOverSubquery_ScalarParamStillValidated
// for the subquery-INNER composition: holt_winters /
// double_exponential_smoothing validates its smoothing factors' (0, 1)
// domain BEFORE ever walking the window's samples, so an out-of-domain
// factor must still surface as a lowering error even though the window
// this query's subquery inner resolves to is all-histogram and would
// otherwise answer empty.
func TestLower_ExpHistogram_DropFamilyInnerOverSubquery_ScalarParamStillValidated(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	query := `double_exponential_smoothing(latency_exp_hist[5m], 1.5, 0.5)[10m:1m]`
	expr := parseExprExp(t, query)
	_, err := promql.LowerAt(context.Background(), expr, s, end, end)
	if err == nil {
		t.Fatalf("lower(%q): got nil error, want an out-of-domain smoothing-factor rejection", query)
	}
}

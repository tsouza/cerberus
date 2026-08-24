package promql_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_SelectFamilyOverSubquery pins cerberus issue
// #2545's category (a): every range-vector function reference Prometheus
// defines real histogram semantics for over an all-histogram window
// (tsouza/prometheus's promql/functions.go — funcCountOverTime,
// funcPresentOverTime, funcFirstOverTime/funcLastOverTime,
// funcResets/funcChanges, funcTsOfFirstOverTime/funcTsOfLastOverTime, plus
// sum_over_time/avg_over_time which already had FOLD-family lowering but
// were missing from [rangeFnOverExpHistogramSubquery]'s switch) now lowers
// a subquery wrapping a histogram-native bare selector, in all three grid
// shapes ([lowerExpHistogramRangeFnOverSubquery]'s own instant / range /
// `@`-pinned split).
func TestLower_ExpHistogram_SelectFamilyOverSubquery(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	pinnedAt := "1767225600" // 2026-01-01T00:00:00Z, matches `start`

	for _, tc := range []struct {
		fn          string
		wantHistRow bool // last_over_time / first_over_time preserve a histogram sample
	}{
		{fn: "count_over_time"},
		{fn: "present_over_time"},
		{fn: "last_over_time", wantHistRow: true},
		{fn: "first_over_time", wantHistRow: true},
		{fn: "resets"},
		{fn: "changes"},
		{fn: "ts_of_first_over_time"},
		{fn: "ts_of_last_over_time"},
		{fn: "sum_over_time", wantHistRow: true},
		{fn: "avg_over_time", wantHistRow: true},
	} {
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()
			wantShape := chplan.SampleRowShape
			if tc.wantHistRow {
				wantShape = chplan.HistogramRowShape
			}

			instant := fmt.Sprintf(`%s((latency_exp_hist)[5m:1m])`, tc.fn)
			expr := parseExprExp(t, instant)
			plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
			if err != nil {
				t.Fatalf("lower(%q) instant: %v", instant, err)
			}
			if got := chplan.RowShapeOf(plan); got != wantShape {
				t.Errorf("lower(%q) instant RowShape = %s, want %s", instant, got, wantShape)
			}

			rplan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
			if err != nil {
				t.Fatalf("lower(%q) range: %v", instant, err)
			}
			if got := chplan.RowShapeOf(rplan); got != wantShape {
				t.Errorf("lower(%q) range RowShape = %s, want %s", instant, got, wantShape)
			}

			pinned := fmt.Sprintf(`%s((latency_exp_hist)[5m:1m] @ %s)`, tc.fn, pinnedAt)
			pexpr := parseExprExp(t, pinned)
			pplan, err := promql.LowerAtRange(context.Background(), pexpr, s, start, end, time.Minute)
			if err != nil {
				t.Fatalf("lower(%q) pinned: %v", pinned, err)
			}
			if got := chplan.RowShapeOf(pplan); got != wantShape {
				t.Errorf("lower(%q) pinned RowShape = %s, want %s", pinned, got, wantShape)
			}
		})
	}
}

// TestLower_ExpHistogram_DropFamilyEmptyOverSubquery pins cerberus issue
// #2563: reference Prometheus's own range-vector functions that read ONLY
// `matrixVal[0].Floats` (max_over_time, min_over_time, stddev_over_time,
// stdvar_over_time, quantile_over_time, mad_over_time, deriv,
// predict_linear, double_exponential_smoothing aka holt_winters,
// ts_of_max_over_time, ts_of_min_over_time — each function's own doc in
// tsouza/prometheus's promql/functions.go) have no real histogram
// semantics to give an all-histogram subquery matrix, so reference itself
// answers an ordinary EMPTY float vector — never an error — the same
// "no data in the window" case any other function sees over an empty
// lookback. [lowerOuterRangeFnOverSubquery]'s histogram-shape guard
// (cerberus issue #2543) used to raise a hard promql-level error for this
// shape instead; #2563 replaced that with the canonical
// [dropExpHistogramSamples] empty-float lowering, gated on
// [histogramSubqueryFloatOnlyDropFunc].
func TestLower_ExpHistogram_DropFamilyEmptyOverSubquery(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	pinnedAt := "1767225600" // 2026-01-01T00:00:00Z, matches `start`

	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "max_over_time", query: `max_over_time((latency_exp_hist)[5m:1m])`},
		{name: "min_over_time", query: `min_over_time((latency_exp_hist)[5m:1m])`},
		{name: "stddev_over_time", query: `stddev_over_time((latency_exp_hist)[5m:1m])`},
		{name: "stdvar_over_time", query: `stdvar_over_time((latency_exp_hist)[5m:1m])`},
		{name: "mad_over_time", query: `mad_over_time((latency_exp_hist)[5m:1m])`},
		{name: "deriv", query: `deriv((latency_exp_hist)[5m:1m])`},
		{name: "ts_of_max_over_time", query: `ts_of_max_over_time((latency_exp_hist)[5m:1m])`},
		{name: "ts_of_min_over_time", query: `ts_of_min_over_time((latency_exp_hist)[5m:1m])`},
		{name: "quantile_over_time", query: `quantile_over_time(0.5, (latency_exp_hist)[5m:1m])`},
		{name: "predict_linear", query: `predict_linear((latency_exp_hist)[5m:1m], 60)`},
		{name: "holt_winters", query: `double_exponential_smoothing((latency_exp_hist)[5m:1m], 0.5, 0.5)`},
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

			// predict_linear / quantile_over_time / holt_winters carry the
			// subquery as an inner argument rather than the whole call, so
			// `@` cannot pin the whole expression the way the select-family
			// test above does; pin the subquery itself instead.
			var pinned string
			switch tc.name {
			case "predict_linear":
				pinned = fmt.Sprintf(`predict_linear((latency_exp_hist)[5m:1m] @ %s, 60)`, pinnedAt)
			case "quantile_over_time":
				pinned = fmt.Sprintf(`quantile_over_time(0.5, (latency_exp_hist)[5m:1m] @ %s)`, pinnedAt)
			case "holt_winters":
				pinned = fmt.Sprintf(`double_exponential_smoothing((latency_exp_hist)[5m:1m] @ %s, 0.5, 0.5)`, pinnedAt)
			default:
				pinned = fmt.Sprintf(`%s((latency_exp_hist)[5m:1m] @ %s)`, tc.name, pinnedAt)
			}
			pexpr := parseExprExp(t, pinned)
			pplan, err := promql.LowerAtRange(context.Background(), pexpr, s, start, end, time.Minute)
			if err != nil {
				t.Fatalf("lower(%q) pinned: %v", pinned, err)
			}
			if got := chplan.RowShapeOf(pplan); got != chplan.SampleRowShape {
				t.Errorf("lower(%q) pinned RowShape = %s, want %s", pinned, got, chplan.SampleRowShape)
			}
		})
	}
}

// TestLower_ExpHistogram_DropFamilyOverSubquery_ScalarParamStillValidated
// pins the param-before-drop discipline [lowerOuterRangeFnOverSubquery]'s
// empty-drop branch owes reference Prometheus: holt_winters/
// double_exponential_smoothing validates its smoothing factors' (0, 1)
// domain BEFORE ever walking the window's samples, so an out-of-domain
// factor must still surface as a lowering error even though the window
// this query's subquery resolves to is all-histogram and would otherwise
// answer empty.
func TestLower_ExpHistogram_DropFamilyOverSubquery_ScalarParamStillValidated(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	query := `double_exponential_smoothing((latency_exp_hist)[5m:1m], 1.5, 0.5)`
	expr := parseExprExp(t, query)
	_, err := promql.LowerAt(context.Background(), expr, s, end, end)
	if err == nil {
		t.Fatalf("lower(%q): got nil error, want an out-of-domain smoothing-factor rejection", query)
	}
}

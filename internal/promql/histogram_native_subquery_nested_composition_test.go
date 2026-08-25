package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_SelectFoldFamilyOverSubquery_Nested pins cerberus
// issue #2569: `<fn>((h)[5m:1m])`, for every name #2545 already handles
// when that expression is the query's own ROOT
// (TestLower_ExpHistogram_SelectFamilyOverSubquery /
// [rangeFnOverExpHistogramSubquery]'s own FOLD-family coverage), now also
// lowers successfully when NESTED under a further wrapper — an
// aggregation, an instant math function, or label_replace — matching
// reference Prometheus, which evaluates the inner expression identically
// regardless of what wraps it.
func TestLower_ExpHistogram_SelectFoldFamilyOverSubquery_Nested(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		fn          string
		wantHistRow bool // last_over_time/first_over_time/the FOLD family preserve a histogram sample
	}{
		// SELECT/COUNT family (cerberus issue #2545) — float-valued.
		{fn: "count_over_time"},
		{fn: "present_over_time"},
		{fn: "resets"},
		{fn: "changes"},
		{fn: "ts_of_first_over_time"},
		{fn: "ts_of_last_over_time"},
		// SELECT family — histogram-preserving.
		{fn: "last_over_time", wantHistRow: true},
		{fn: "first_over_time", wantHistRow: true},
		// FOLD family (cerberus issue #2545) — all histogram-preserving.
		{fn: "rate", wantHistRow: true},
		{fn: "increase", wantHistRow: true},
		{fn: "delta", wantHistRow: true},
		{fn: "irate", wantHistRow: true},
		{fn: "idelta", wantHistRow: true},
		{fn: "sum_over_time", wantHistRow: true},
		{fn: "avg_over_time", wantHistRow: true},
	} {
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()
			wantShape := chplan.SampleRowShape
			if tc.wantHistRow {
				wantShape = chplan.HistogramRowShape
			}
			inner := tc.fn + `((latency_exp_hist)[5m:1m])`

			// Nested under an aggregation — the issue's own trigger_query
			// shape (`sum(count_over_time((h)[5m:1m]))`).
			t.Run("sum", func(t *testing.T) {
				t.Parallel()
				query := "sum by (job) (" + inner + ")"
				expr := parseExprExp(t, query)
				plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
				if err != nil {
					t.Fatalf("lower(%q): %v", query, err)
				}
				if got := chplan.RowShapeOf(plan); got != wantShape {
					t.Errorf("lower(%q) RowShape = %s, want %s", query, got, wantShape)
				}
			})

			// Nested under an instant math function.
			t.Run("abs", func(t *testing.T) {
				t.Parallel()
				query := "abs(" + inner + ")"
				expr := parseExprExp(t, query)
				plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
				if err != nil {
					t.Fatalf("lower(%q): %v", query, err)
				}
				// Prometheus's simpleFloatFunc family (abs included) skips
				// every histogram sample and answers an ordinary EMPTY
				// float vector for a histogram-valued argument — abs()
				// never itself preserves a histogram regardless of what
				// wraps it, so the drop-family shape is the correct
				// answer for the histogram-preserving names here, exactly
				// as it is for the bare (non-nested) shape
				// (TestLower_ExpHistogram_DropFamilyEmptyOverSubquery).
				wantAbsShape := wantShape
				if tc.wantHistRow {
					wantAbsShape = chplan.SampleRowShape
				}
				if got := chplan.RowShapeOf(plan); got != wantAbsShape {
					t.Errorf("lower(%q) RowShape = %s, want %s", query, got, wantAbsShape)
				}
			})

			// Nested under label_replace — a non-aggregation wrapper that
			// forwards the payload unchanged.
			t.Run("label_replace", func(t *testing.T) {
				t.Parallel()
				query := `label_replace(` + inner + `, "extra", "yes", "", "")`
				expr := parseExprExp(t, query)
				plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
				if err != nil {
					t.Fatalf("lower(%q): %v", query, err)
				}
				if got := chplan.RowShapeOf(plan); got != wantShape {
					t.Errorf("lower(%q) RowShape = %s, want %s", query, got, wantShape)
				}
			})
		})
	}
}

// TestLower_ExpHistogram_DropFamilyEmptyOverSubquery_Nested pins that the
// eleven float-only range-vector reducers (cerberus issue #2563) still
// answer their canonical empty-float drop shape — never an error, and
// never regressed by #2569's own composition threading — when nested under
// a further wrapper.
func TestLower_ExpHistogram_DropFamilyEmptyOverSubquery_Nested(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "max_over_time", query: `sum(max_over_time((latency_exp_hist)[5m:1m]))`},
		{name: "min_over_time", query: `abs(min_over_time((latency_exp_hist)[5m:1m]))`},
		{name: "quantile_over_time", query: `sum(quantile_over_time(0.5, (latency_exp_hist)[5m:1m]))`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr := parseExprExp(t, tc.query)
			plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
			if err != nil {
				t.Fatalf("lower(%q): %v", tc.query, err)
			}
			if got := chplan.RowShapeOf(plan); got != chplan.SampleRowShape {
				t.Errorf("lower(%q) RowShape = %s, want %s", tc.query, got, chplan.SampleRowShape)
			}
		})
	}
}

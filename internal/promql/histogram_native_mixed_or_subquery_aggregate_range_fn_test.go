package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// sumOrAvgMixedOrSubqueryQuery builds `<fn>((sum by (series) ((<hist>) or
// (<float>)))[5m:1m])` — cerberus issue #2581's own remaining evidence
// shape, the `sum`/`avg`-wrapped sibling of
// histogram_native_mixed_or_subquery_further_setop_test.go's
// [furtherSetOpQuery].
func sumOrAvgMixedOrSubqueryQuery(fn, aggOp string) string {
	return fn + "((" + aggOp + " by (series) ((latency_exp_hist) or (num_cpus)))[5m:1m])"
}

// sumOrAvgMixedOrSelectFnNames / sumOrAvgMixedOrFoldFnNames /
// sumOrAvgMixedOrResetsChangesNames / sumOrAvgMixedOrLastFirstNames
// partition [selectFoldFamilyNames] into the four composer groups this
// package's mixed-or-subquery lowerings answer — see
// histogram_native_mixed_or_subquery_aggregate_range_fn.go's own "Scope"
// doc. All fifteen names now compose, across all three grid modes.
var (
	sumOrAvgMixedOrSelectFnNames = []string{
		"count_over_time", "present_over_time", "ts_of_first_over_time", "ts_of_last_over_time",
	}
	sumOrAvgMixedOrFoldFnNames = []string{
		"rate", "increase", "delta", "irate", "idelta", "sum_over_time", "avg_over_time",
	}
	sumOrAvgMixedOrResetsChangesNames = []string{
		"resets", "changes",
	}
	sumOrAvgMixedOrLastFirstNames = []string{
		"last_over_time", "first_over_time",
	}
)

// sumOrAvgMixedOrAllNames is every one of the fifteen SELECT/FOLD-family
// names this package's mixed-or-subquery composers answer.
func sumOrAvgMixedOrAllNames() []string {
	names := append([]string{}, sumOrAvgMixedOrSelectFnNames...)
	names = append(names, sumOrAvgMixedOrFoldFnNames...)
	names = append(names, sumOrAvgMixedOrResetsChangesNames...)
	return append(names, sumOrAvgMixedOrLastFirstNames...)
}

// TestSumOrAvgMixedOrSubquery_Composes proves `<fn>((sum by (series)
// ((h) or (f)))[5m:1m])` lowers successfully — no error — for every one
// of the fifteen names this file's own production code composes, for both
// `sum` and `avg`, at instant eval.
func TestSumOrAvgMixedOrSubquery_Composes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	for _, aggOp := range []string{"sum", "avg"} {
		for _, fn := range sumOrAvgMixedOrAllNames() {
			t.Run(aggOp+"/"+fn, func(t *testing.T) {
				t.Parallel()
				query := sumOrAvgMixedOrSubqueryQuery(fn, aggOp)
				expr := parseExprExp(t, query)
				node, err := promql.LowerAt(context.Background(), expr, s, end, end)
				if err != nil {
					t.Fatalf("lower(%q): want success, got error: %v", query, err)
				}
				if node == nil {
					t.Fatalf("lower(%q): want a non-nil plan", query)
				}
			})
		}
	}
}

// TestSumOrAvgMixedOrSubquery_RangeFanoutComposes proves EVERY one of the
// fifteen names — the FOLD family included, cerberus issue #2715's own
// fix ([lowerSumOrAvgMixedOrSubqueryFoldFnRange]) — composes under a TRUE
// query_range fan-out (no `@` pin on the subquery), for both `sum` and
// `avg`.
func TestSumOrAvgMixedOrSubquery_RangeFanoutComposes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	for _, aggOp := range []string{"sum", "avg"} {
		for _, fn := range sumOrAvgMixedOrAllNames() {
			t.Run(aggOp+"/"+fn, func(t *testing.T) {
				t.Parallel()
				query := sumOrAvgMixedOrSubqueryQuery(fn, aggOp)
				expr := parseExprExp(t, query)
				node, err := promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
				if err != nil {
					t.Fatalf("lower(%q) over query_range: want success, got error: %v", query, err)
				}
				if node == nil {
					t.Fatalf("lower(%q) over query_range: want a non-nil plan", query)
				}
			})
		}
	}
}

// TestSumOrAvgMixedOrSubquery_PinnedRangeComposes proves the FOLD family
// DOES still compose under query_range when the subquery carries an `@`
// pin — the single-window broadcast case
// [sumOrAvgMixedOrSubqueryOuterFnRecognized] admits alongside plain
// instant eval.
func TestSumOrAvgMixedOrSubquery_PinnedRangeComposes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	for _, fn := range sumOrAvgMixedOrFoldFnNames {
		t.Run(fn, func(t *testing.T) {
			t.Parallel()
			query := fn + "((sum by (series) ((latency_exp_hist) or (num_cpus)))[5m:1m] @ end())"
			expr := parseExprExp(t, query)
			node, err := promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
			if err != nil {
				t.Fatalf("lower(%q): want success, got error: %v", query, err)
			}
			if node == nil {
				t.Fatalf("lower(%q): want a non-nil plan", query)
			}
		})
	}
}

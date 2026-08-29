package promql_test

import (
	"context"
	"strings"
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
// sumOrAvgMixedOrResetsChangesNames partition [selectFoldFamilyNames]
// into the thirteen names this file's own production code
// (histogram_native_mixed_or_subquery_aggregate_range_fn.go and its
// resets/changes sibling) now composes and the two that deliberately
// still reject — see that file's own "Scope" doc.
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
	sumOrAvgMixedOrStillRejectedNames = []string{
		"last_over_time", "first_over_time",
	}
)

// TestSumOrAvgMixedOrSubquery_Composes proves `<fn>((sum by (series)
// ((h) or (f)))[5m:1m])` lowers successfully — no error — for every one
// of the thirteen names this file's own production code now composes,
// for both `sum` and `avg`.
func TestSumOrAvgMixedOrSubquery_Composes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	names := append(append([]string{}, sumOrAvgMixedOrSelectFnNames...), sumOrAvgMixedOrFoldFnNames...)
	names = append(names, sumOrAvgMixedOrResetsChangesNames...)
	for _, aggOp := range []string{"sum", "avg"} {
		for _, fn := range names {
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

// TestSumOrAvgMixedOrSubquery_StillRejects pins that the two names this
// file's own production code deliberately does not compose
// (last_over_time / first_over_time — see
// histogram_native_mixed_or_subquery_aggregate_range_fn.go's own "Scope"
// doc) still reject with the SAME pre-existing message, unchanged by this
// composition.
func TestSumOrAvgMixedOrSubquery_StillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	const wantErrSubstr = "wrapping a native-histogram-valued shape is unsupported"

	for _, aggOp := range []string{"sum", "avg"} {
		for _, fn := range sumOrAvgMixedOrStillRejectedNames {
			t.Run(aggOp+"/"+fn, func(t *testing.T) {
				t.Parallel()
				query := sumOrAvgMixedOrSubqueryQuery(fn, aggOp)
				expr := parseExprExp(t, query)
				_, err := promql.LowerAt(context.Background(), expr, s, end, end)
				if err == nil {
					t.Fatalf("lower(%q): want a clean rejection, got none", query)
				}
				if !strings.Contains(err.Error(), wantErrSubstr) {
					t.Errorf("lower(%q) error = %v, want it to contain %q", query, err, wantErrSubstr)
				}
			})
		}
	}
}

// TestSumOrAvgMixedOrSubquery_RangeFanoutStillRejects pins the FOLD
// family's own residual scope gap: a true query_range fan-out (no `@`
// pin on the subquery) still rejects for the seven window-purity-filtered
// names, because the collision-drop test is not yet scoped per outer
// anchor — see histogram_native_mixed_or_subquery_aggregate_range_fn.go's
// own "Scope" doc for exactly why.
func TestSumOrAvgMixedOrSubquery_RangeFanoutStillRejects(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	const wantErrSubstr = "wrapping a native-histogram-valued shape is unsupported"

	for _, fn := range sumOrAvgMixedOrFoldFnNames {
		t.Run(fn, func(t *testing.T) {
			t.Parallel()
			query := sumOrAvgMixedOrSubqueryQuery(fn, "sum")
			expr := parseExprExp(t, query)
			_, err := promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
			if err == nil {
				t.Fatalf("lower(%q) over query_range: want a clean rejection, got none", query)
			}
			if !strings.Contains(err.Error(), wantErrSubstr) {
				t.Errorf("lower(%q) over query_range error = %v, want it to contain %q", query, err, wantErrSubstr)
			}
		})
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

// TestSumOrAvgMixedOrSubquery_ResetsChangesRangeFanoutComposes proves
// resets/changes compose under a TRUE query_range fan-out (no `@` pin) —
// unlike this file's own FOLD family
// (TestSumOrAvgMixedOrSubquery_RangeFanoutStillRejects), which still
// rejects that shape. See
// histogram_native_mixed_or_subquery_resets_changes.go's own top-level
// doc for why resets/changes reach all three grid modes where the FOLD
// family only reaches two.
func TestSumOrAvgMixedOrSubquery_ResetsChangesRangeFanoutComposes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	for _, aggOp := range []string{"sum", "avg"} {
		for _, fn := range sumOrAvgMixedOrResetsChangesNames {
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

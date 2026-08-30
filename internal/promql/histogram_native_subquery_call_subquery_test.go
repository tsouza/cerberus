package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// callSubqueryDoublyNestedQuery builds `<fn>((<inner>)[2m:1m])[10m:1m]` —
// cerberus issue #2726's own doubly-nested shape
// ([lowerSubqueryOverCallSubquery] / [lowerHistogramOrMixedCallSubqueryInput],
// histogram_native_subquery_call_subquery.go) — for the fifteen-name sweep
// below.
func callSubqueryDoublyNestedQuery(fn, inner string) string {
	return fn + "((" + inner + ")[2m:1m])[10m:1m]"
}

// TestSubqueryCallSubquery_Composes proves `<fn>((<inner>)[2m:1m])[10m:1m]`
// lowers successfully — no error — for every one of the fifteen SELECT/
// FOLD-family names this package's doubly-nested composer answers
// ([lowerHistogramOrMixedCallSubqueryInput]), for both a HistogramRowShape
// inner (`(latency_exp_hist) and (latency_exp_hist)`) and a MixedRowShape
// inner (`(latency_exp_hist) or (num_cpus)`), at instant eval.
//
// The chDB-backed proofs in histogram_native_subquery_call_subquery_chdb_test.go
// verify real execution correctness for the same fifteen names; this file's
// own job is orthogonal — giving gremlins' default (non-chdb-tagged) run
// real statement coverage over this new code, since a `_chdb_test.go` file
// is entirely excluded from a build without the `chdb` tag, which left the
// package's own default mutation-testing lane measuring 18/18 of this
// file's mutants as NOT COVERED (all its execution paths were only ever
// reached through chdb-tagged tests) rather than exercising the dispatch
// and every aggregate-selection branch it wires together.
func TestSubqueryCallSubquery_Composes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	inners := map[string]string{
		"hist_and": `(latency_exp_hist) and (latency_exp_hist)`,
		"mixed_or": `(latency_exp_hist) or (num_cpus)`,
	}

	for shape, inner := range inners {
		for _, fn := range selectFoldFamilyNames {
			t.Run(shape+"/"+fn, func(t *testing.T) {
				t.Parallel()
				query := callSubqueryDoublyNestedQuery(fn, inner)
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

// TestSubqueryCallSubquery_RangeFanoutComposes proves the same fifteen
// names compose under a TRUE query_range fan-out (no `@` pin on the outer
// subquery) — the shape [buildOuterRangeSubqueryFanout]'s OuterRange grid
// answers, as distinct from the single pinned-instant anchor
// TestSubqueryCallSubquery_Composes exercises above.
func TestSubqueryCallSubquery_RangeFanoutComposes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	inners := map[string]string{
		"hist_and": `(latency_exp_hist) and (latency_exp_hist)`,
		"mixed_or": `(latency_exp_hist) or (num_cpus)`,
	}

	for shape, inner := range inners {
		for _, fn := range selectFoldFamilyNames {
			t.Run(shape+"/"+fn, func(t *testing.T) {
				t.Parallel()
				query := callSubqueryDoublyNestedQuery(fn, inner)
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
}

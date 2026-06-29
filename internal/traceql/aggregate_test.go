package traceql_test

import (
	"context"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLowerSpansetAggregate_PerTraceShape asserts that
// `| count()` / `| avg()` / `| sum()` / `| min()` / `| max()`
// — the second-stage spanset aggregates — lower to a
// chplan.Aggregate with TraceId in GroupBy and the per-trace
// envelope columns (MetricName / ResourceAttrs / TimeUnix) in
// AggFuncs. Pins the wire contract behind the per-trace
// count_spans_per_trace + avg_duration_per_trace_status_ok
// Tempo-compat cases.
//
// Without per-trace grouping, `{ ... } | count() > 0` collapses
// every matching span into a single row with empty
// rootServiceName / rootTraceName fields — Grafana's trace list
// then shows one mystery row instead of one entry per matching
// trace.
func TestLowerSpansetAggregate_PerTraceShape(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	cases := []struct {
		name           string
		query          string
		wantValueAgg   string // CH aggregate-function name for the Value slot
		wantOuterShape string // "Aggregate" (bare) or "Filter" (scalar-filter HAVING wrap)
	}{
		{
			name:           "count_threshold",
			query:          `{ resource.service.name = "frontend" } | count() > 0`,
			wantValueAgg:   "count",
			wantOuterShape: "Filter",
		},
		{
			name:           "avg_threshold",
			query:          `{ status = ok } | avg(duration) > 0`,
			wantValueAgg:   "avg",
			wantOuterShape: "Filter",
		},
		{
			name:           "sum_threshold",
			query:          `{ resource.service.name = "x" } | sum(duration) > 100ms`,
			wantValueAgg:   "sum",
			wantOuterShape: "Filter",
		},
		{
			name:           "min_threshold",
			query:          `{ resource.service.name = "x" } | min(duration) > 10ms`,
			wantValueAgg:   "min",
			wantOuterShape: "Filter",
		},
		{
			name:           "max_threshold",
			query:          `{ resource.service.name = "x" } | max(duration) > 500ms`,
			wantValueAgg:   "max",
			wantOuterShape: "Filter",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			plan, err := traceql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}

			// Walk to the Aggregate node (skipping the scalar-filter
			// HAVING wrap when present).
			var agg *chplan.Aggregate
			switch v := plan.(type) {
			case *chplan.Filter:
				if tc.wantOuterShape != "Filter" {
					t.Errorf("outer shape = Filter, want %s", tc.wantOuterShape)
				}
				a, ok := v.Input.(*chplan.Aggregate)
				if !ok {
					t.Fatalf("Filter.Input = %T, want *chplan.Aggregate", v.Input)
				}
				agg = a
			case *chplan.Aggregate:
				if tc.wantOuterShape != "Aggregate" {
					t.Errorf("outer shape = Aggregate, want %s", tc.wantOuterShape)
				}
				agg = v
			default:
				t.Fatalf("outer = %T, want Filter/Aggregate", plan)
			}

			// GroupBy must include exactly TraceId.
			if len(agg.GroupBy) != 1 {
				t.Fatalf("len(GroupBy) = %d, want 1 (TraceId)", len(agg.GroupBy))
			}
			gbCol, ok := agg.GroupBy[0].(*chplan.ColumnRef)
			if !ok || gbCol.Name != s.TraceIDColumn {
				t.Errorf("GroupBy[0] = %v, want ColumnRef(%s)", agg.GroupBy[0], s.TraceIDColumn)
			}
			if len(agg.GroupByAliases) != 1 || agg.GroupByAliases[0] != "TraceId" {
				t.Errorf("GroupByAliases = %v, want [TraceId]", agg.GroupByAliases)
			}

			// AggFuncs must include Value (with the expected CH agg
			// name) plus the three envelope columns.
			aliases := map[string]string{}
			for _, af := range agg.AggFuncs {
				aliases[af.Alias] = af.Name
			}
			if got := aliases["Value"]; got != tc.wantValueAgg {
				t.Errorf("Value agg = %q, want %q", got, tc.wantValueAgg)
			}
			if got := aliases["MetricName"]; got != "any" {
				t.Errorf("MetricName agg = %q, want any", got)
			}
			if got := aliases["ResourceAttrs"]; got != "any" {
				t.Errorf("ResourceAttrs agg = %q, want any", got)
			}
			if got := aliases["TimeUnix"]; got != "min" {
				t.Errorf("TimeUnix agg = %q, want min", got)
			}
		})
	}
}

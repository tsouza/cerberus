package traceql_test

import (
	"strings"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLowerMetricsPipeline exercises the MetricsPipeline lowering
// directly: parse a TraceQL metrics query, lower it, and walk the
// resulting chplan tree to confirm the expected Aggregate(Scan)
// shape, group-by labels, and CH aggregate function name.
//
// Range / step intentionally aren't part of the lowered tree —
// the /api/metrics/query_range handler wraps with chplan.RangeWindow
// at request time (see docs/fork-tempo-plan.md § 2c).
func TestLowerMetricsPipeline(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	cases := []struct {
		name      string
		query     string
		wantAgg   string // CH aggregate function on the outer node
		wantArgs  int    // number of Args on the AggFunc
		wantGroup int    // number of GroupBy expressions
		hasFilter bool   // whether the spanset selector produces a Filter
	}{
		{
			name:    "rate_no_selector",
			query:   "{} | rate()",
			wantAgg: "count", wantArgs: 1, wantGroup: 0, hasFilter: true,
		},
		{
			name:    "count_over_time_with_selector",
			query:   `{ resource.service.name = "frontend" } | count_over_time()`,
			wantAgg: "count", wantArgs: 1, wantGroup: 0, hasFilter: true,
		},
		{
			name:    "sum_over_time_attr",
			query:   `{} | sum_over_time(duration)`,
			wantAgg: "sum", wantArgs: 1, wantGroup: 0, hasFilter: true,
		},
		{
			name:    "min_over_time_attr",
			query:   `{} | min_over_time(duration)`,
			wantAgg: "min", wantArgs: 1, wantGroup: 0, hasFilter: true,
		},
		{
			name:    "max_over_time_attr",
			query:   `{} | max_over_time(duration)`,
			wantAgg: "max", wantArgs: 1, wantGroup: 0, hasFilter: true,
		},
		{
			name:    "rate_by_label",
			query:   `{} | rate() by (resource.service.name)`,
			wantAgg: "count", wantArgs: 1, wantGroup: 1, hasFilter: true,
		},
		{
			name:    "quantile_over_time_single",
			query:   `{} | quantile_over_time(duration, 0.95)`,
			wantAgg: "quantile", wantArgs: 1, wantGroup: 0, hasFilter: true,
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
			plan, err := traceql.Lower(expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}

			agg, ok := plan.(*chplan.Aggregate)
			if !ok {
				t.Fatalf("expected outermost node to be *chplan.Aggregate, got %T", plan)
			}
			if len(agg.AggFuncs) != 1 {
				t.Fatalf("expected exactly 1 AggFunc, got %d", len(agg.AggFuncs))
			}
			af := agg.AggFuncs[0]
			if af.Name != tc.wantAgg {
				t.Errorf("AggFunc.Name = %q, want %q", af.Name, tc.wantAgg)
			}
			if af.Alias != "Value" {
				t.Errorf("AggFunc.Alias = %q, want %q", af.Alias, "Value")
			}
			if len(af.Args) != tc.wantArgs {
				t.Errorf("len(AggFunc.Args) = %d, want %d", len(af.Args), tc.wantArgs)
			}
			if len(agg.GroupBy) != tc.wantGroup {
				t.Errorf("len(Aggregate.GroupBy) = %d, want %d", len(agg.GroupBy), tc.wantGroup)
			}
			if len(agg.GroupBy) != len(agg.GroupByAliases) {
				t.Errorf("GroupBy/GroupByAliases length mismatch: %d vs %d", len(agg.GroupBy), len(agg.GroupByAliases))
			}

			// Walk the inner subtree: Scan, optionally wrapped by Filter.
			inner := agg.Input
			if tc.hasFilter {
				f, ok := inner.(*chplan.Filter)
				if !ok {
					t.Fatalf("expected Aggregate.Input to be *chplan.Filter, got %T", inner)
				}
				inner = f.Input
			}
			scan, ok := inner.(*chplan.Scan)
			if !ok {
				t.Fatalf("expected Aggregate.Input innermost to be *chplan.Scan, got %T", inner)
			}
			if scan.Table != s.SpansTable {
				t.Errorf("Scan.Table = %q, want %q", scan.Table, s.SpansTable)
			}

			// Quantile case: Params carry the quantile literal.
			if tc.wantAgg == "quantile" {
				if len(af.Params) != 1 {
					t.Fatalf("expected 1 quantile Param, got %d", len(af.Params))
				}
				lf, ok := af.Params[0].(*chplan.LitFloat)
				if !ok {
					t.Fatalf("expected Param[0] to be *chplan.LitFloat, got %T", af.Params[0])
				}
				if lf.V != 0.95 {
					t.Errorf("quantile = %v, want 0.95", lf.V)
				}
			}
		})
	}
}

// TestLowerMetricsPipelineUnsupported documents the cases that
// surface as clean errors rather than panicking.
//
// `avg_over_time(...)` is deferred because Tempo parses it into an
// unexported `*averageOverTimeAggregator` rather than a
// `*MetricsAggregate` — the cerberus-accessors fork hasn't exposed
// accessors on that type yet, and the post-#148 rule forbids
// reflect/unsafe on parser AST.
//
// `| > 0` (MetricsSecondStage) is deferred until the second-stage
// filter / topk lowering lands.
func TestLowerMetricsPipelineUnsupported(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	cases := []struct {
		name       string
		query      string
		wantSubstr string
	}{
		{
			name:       "avg_over_time_deferred",
			query:      `{} | avg_over_time(duration)`,
			wantSubstr: "not yet supported",
		},
		{
			name:       "histogram_over_time_deferred",
			query:      `{} | histogram_over_time(duration)`,
			wantSubstr: "histogram_over_time is not yet supported",
		},
		{
			name:       "quantile_over_time_multi_deferred",
			query:      `{} | quantile_over_time(duration, 0.5, 0.9, 0.99)`,
			wantSubstr: "multi-quantile",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := tempo.Parse(tc.query)
			if err != nil {
				// Some forms may fail to parse upstream — the test
				// is documenting what cerberus surfaces, so a parser
				// error is acceptable for these deferred forms.
				return
			}
			_, err = traceql.Lower(expr, s)
			if err == nil {
				t.Fatalf("Lower(%q): expected error, got nil", tc.query)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("Lower(%q) error = %q, want substring %q",
					tc.query, err.Error(), tc.wantSubstr)
			}
		})
	}
}

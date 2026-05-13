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
// resulting chplan tree to confirm the expected MetricsAggregate(Scan)
// shape, group-by labels, and chplan MetricsOp.
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
		wantOp    chplan.MetricsOp
		wantAttr  bool // whether MetricsAggregate.Attr is non-nil
		wantGroup int  // number of GroupBy expressions
		hasFilter bool // whether the spanset selector produces a Filter
	}{
		{
			name:   "rate_no_selector",
			query:  "{} | rate()",
			wantOp: chplan.MetricsOpRate, wantAttr: false, wantGroup: 0, hasFilter: true,
		},
		{
			name:   "count_over_time_with_selector",
			query:  `{ resource.service.name = "frontend" } | count_over_time()`,
			wantOp: chplan.MetricsOpCountOverTime, wantAttr: false, wantGroup: 0, hasFilter: true,
		},
		{
			name:   "sum_over_time_attr",
			query:  `{} | sum_over_time(duration)`,
			wantOp: chplan.MetricsOpSumOverTime, wantAttr: true, wantGroup: 0, hasFilter: true,
		},
		{
			name:   "min_over_time_attr",
			query:  `{} | min_over_time(duration)`,
			wantOp: chplan.MetricsOpMinOverTime, wantAttr: true, wantGroup: 0, hasFilter: true,
		},
		{
			name:   "max_over_time_attr",
			query:  `{} | max_over_time(duration)`,
			wantOp: chplan.MetricsOpMaxOverTime, wantAttr: true, wantGroup: 0, hasFilter: true,
		},
		{
			name:   "rate_by_label",
			query:  `{} | rate() by (resource.service.name)`,
			wantOp: chplan.MetricsOpRate, wantAttr: false, wantGroup: 1, hasFilter: true,
		},
		{
			name:   "quantile_over_time_single",
			query:  `{} | quantile_over_time(duration, 0.95)`,
			wantOp: chplan.MetricsOpQuantileOverTime, wantAttr: true, wantGroup: 0, hasFilter: true,
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

			ma, ok := plan.(*chplan.MetricsAggregate)
			if !ok {
				t.Fatalf("expected outermost node to be *chplan.MetricsAggregate, got %T", plan)
			}
			if ma.Op != tc.wantOp {
				t.Errorf("MetricsAggregate.Op = %v, want %v", ma.Op, tc.wantOp)
			}
			if ma.ValueAlias != "Value" {
				t.Errorf("MetricsAggregate.ValueAlias = %q, want %q", ma.ValueAlias, "Value")
			}
			if (ma.Attr != nil) != tc.wantAttr {
				t.Errorf("MetricsAggregate.Attr non-nil = %v, want %v", ma.Attr != nil, tc.wantAttr)
			}
			if len(ma.GroupBy) != tc.wantGroup {
				t.Errorf("len(MetricsAggregate.GroupBy) = %d, want %d", len(ma.GroupBy), tc.wantGroup)
			}
			if len(ma.GroupBy) != len(ma.GroupByAliases) {
				t.Errorf("GroupBy/GroupByAliases length mismatch: %d vs %d", len(ma.GroupBy), len(ma.GroupByAliases))
			}

			// Walk the inner subtree: Scan, optionally wrapped by Filter.
			inner := ma.Inner
			if tc.hasFilter {
				f, ok := inner.(*chplan.Filter)
				if !ok {
					t.Fatalf("expected MetricsAggregate.Inner to be *chplan.Filter, got %T", inner)
				}
				inner = f.Input
			}
			scan, ok := inner.(*chplan.Scan)
			if !ok {
				t.Fatalf("expected MetricsAggregate.Inner innermost to be *chplan.Scan, got %T", inner)
			}
			if scan.Table != s.SpansTable {
				t.Errorf("Scan.Table = %q, want %q", scan.Table, s.SpansTable)
			}

			// Quantile case: Quantiles carries the literal.
			if tc.wantOp == chplan.MetricsOpQuantileOverTime {
				if len(ma.Quantiles) != 1 {
					t.Fatalf("expected 1 quantile, got %d", len(ma.Quantiles))
				}
				if ma.Quantiles[0] != 0.95 {
					t.Errorf("Quantiles[0] = %v, want 0.95", ma.Quantiles[0])
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

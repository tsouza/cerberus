package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_TsOfFirstLastOverTime pins cerberus issue #2482's
// ts_of_first_over_time / ts_of_last_over_time half: over a bare
// exp-histogram matrix selector, reference Prometheus's
// funcTsOfFirstOverTime / funcTsOfLastOverTime reduce to the window's own
// earliest / latest histogram-sample timestamp (see
// histogram_native_ts_of_first_last_over_time.go's doc for the full
// min(tf,th)/max(tf,th) derivation) reported as a float epoch-seconds
// VALUE with `__name__` dropped. Every representative shape below must
// lower successfully to the canonical float [chplan.SampleRowShape], both
// in instant and range mode, and the produced SQL must actually compile
// through the chsql emitter — the exact shape that used to hit
// expHistogramSelectorRouting's catch-all rejection instead.
func TestLower_ExpHistogram_TsOfFirstLastOverTime(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	start := at.Add(-time.Hour)

	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "issue representative query: ts_of_first_over_time",
			query: `ts_of_first_over_time(demo_latency_exp_hist[5m])`,
		},
		{
			name:  "issue representative query: ts_of_last_over_time",
			query: `ts_of_last_over_time(demo_latency_exp_hist[5m])`,
		},
		{
			name:  "ts_of_first_over_time with label matcher",
			query: `ts_of_first_over_time(demo_latency_exp_hist{service="api"}[5m])`,
		},
		{
			name:  "ts_of_last_over_time with label matcher",
			query: `ts_of_last_over_time(demo_latency_exp_hist{service="api"}[5m])`,
		},
		{
			name:  "parenthesised call",
			query: `(ts_of_first_over_time(demo_latency_exp_hist[5m]))`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name+"/instant", func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error: %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s", tc.query, shape, chplan.SampleRowShape)
			}
			if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
				t.Fatalf("chsql.Emit(%q): %v", tc.query, err)
			}
		})
		t.Run(tc.name+"/range", func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAtRange(context.Background(), expr, s, start, at, 30*time.Second)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): unexpected error: %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("lower(%q) range: plan root publishes %s, want %s", tc.query, shape, chplan.SampleRowShape)
			}
			if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
				t.Fatalf("chsql.Emit(%q) range: %v", tc.query, err)
			}
		})
	}
}

// TestLower_ExpHistogram_TsOfFirstLastOverTime_AtPin covers the
// range-query, `@`-pinned (gridBroadcast) shape for a bare exp-histogram
// selector: the pinned window is resolved once and its picked sample's
// timestamp value is fanned across the step grid, at each step's own
// anchor position — mirroring TestLower_ExpHistogram_Timestamp_AtPin.
func TestLower_ExpHistogram_TsOfFirstLastOverTime_AtPin(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	start := at.Add(-time.Hour)

	for _, query := range []string{
		`ts_of_first_over_time(demo_latency_exp_hist[5m] @ 1735689600)`,
		`ts_of_last_over_time(demo_latency_exp_hist[5m] @ 1735689600)`,
	} {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAtRange(context.Background(), expr, s, start, at, 30*time.Second)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): unexpected error: %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s", query, shape, chplan.SampleRowShape)
			}
			if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
				t.Fatalf("chsql.Emit(%q): %v", query, err)
			}
		})
	}
}

// TestLower_ExpHistogram_TsOfFirstLastOverTime_NameDropped pins the
// __name__-handling half of cerberus issue #2482's finding: despite
// reference Prometheus's funcTsOfFirstOverTime / funcTsOfLastOverTime
// literally assigning `Sample{Metric: el.Metric, ...}`, the engine's
// separate dropName gate (promql/engine.go:2114) strips it for every
// function except literally "last_over_time"/"first_over_time" — so the
// projected MetricName here must be the empty-string literal, matching
// cerberus's own existing (verified) classic float-selector lowering for
// these two functions, not a preserved name.
func TestLower_ExpHistogram_TsOfFirstLastOverTime_NameDropped(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, query := range []string{
		`ts_of_first_over_time(demo_latency_exp_hist[5m])`,
		`ts_of_last_over_time(demo_latency_exp_hist[5m])`,
	} {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
			}
			project, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("plan root = %T, want *chplan.Project", plan)
			}
			found := false
			for _, proj := range project.Projections {
				if proj.Alias != s.MetricNameColumn {
					continue
				}
				found = true
				lit, ok := proj.Expr.(*chplan.LitString)
				if !ok || lit.V != "" {
					t.Fatalf("MetricName projection = %#v, want empty LitString", proj.Expr)
				}
			}
			if !found {
				t.Fatalf("no MetricName projection found in %#v", project.Projections)
			}
		})
	}
}

package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_CountPresentOverTimeIsFloatValued pins cerberus
// issue #2480: count_over_time() / present_over_time() over a bare
// exp-histogram selector answer a FLOAT-valued plan — never
// expHistogramSelectorRouting's rejection, and never the empty-vector drop
// [rangeVectorFloatOnlyDropFuncs] applies to min_over_time / max_over_time
// — because reference Prometheus's funcCountOverTime / funcPresentOverTime
// never guard on `len(samples.Floats) == 0` before answering.
func TestLower_ExpHistogram_CountPresentOverTimeIsFloatValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	for _, tc := range []struct {
		name  string
		query string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{
			name:  "instant count",
			query: `count_over_time(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant present",
			query: `present_over_time(latency_exp_hist{service="api"}[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "count offset",
			query: `count_over_time(latency_exp_hist[5m] offset 10m)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "count range",
			query: `count_over_time(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
		},
		{
			name:  "present range at pin",
			query: `present_over_time(latency_exp_hist[5m] @ 1767225600)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := tc.lower(expr)
			if err != nil {
				t.Fatalf("lower(%q): %v", tc.query, err)
			}
			proj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q) root = %T, want *chplan.Project", tc.query, plan)
			}
			if got := len(proj.Projections); got != 4 {
				t.Fatalf("lower(%q) projections = %d, want 4 (canonical float Sample quartet)", tc.query, got)
			}
			name, ok := proj.Projections[0].Expr.(*chplan.LitString)
			if !ok || name.V != "" {
				t.Fatalf("lower(%q) MetricName projection = %#v, want empty derived name", tc.query, proj.Projections[0].Expr)
			}
			if alias := proj.Projections[3].Alias; alias != s.ValueColumn {
				t.Fatalf("lower(%q) 4th projection alias = %q, want %q", tc.query, alias, s.ValueColumn)
			}
		})
	}
}

// TestLower_ExpHistogram_CountOverTimeEmitsRowCount pins the SQL shape:
// count_over_time() must emit an actual count() aggregate over the
// windowed rows, and present_over_time() must emit the constant-1
// any(toFloat64(1)) pattern [lowerExpHistogramCountOrGroupOverPlan]
// already uses for GROUP() — never a literal 0 (the drop shape) and never
// each other's aggregate.
func TestLower_ExpHistogram_CountOverTimeEmitsRowCount(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	for _, tc := range []struct {
		query    string
		wantSQL  string
		unwanted string
	}{
		{query: `count_over_time(latency_exp_hist[5m])`, wantSQL: "count(", unwanted: "any(toFloat64("},
		{query: `present_over_time(latency_exp_hist[5m])`, wantSQL: "any(toFloat64(", unwanted: "count("},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			p := parser.NewParser(parser.Options{})
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt: %v", err)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, tc.wantSQL) {
				t.Errorf("SQL missing %q:\n%s", tc.wantSQL, sql)
			}
			if strings.Contains(sql, tc.unwanted) {
				t.Errorf("SQL unexpectedly contains %q:\n%s", tc.unwanted, sql)
			}
			if strings.Contains(sql, "WHERE false") || strings.Contains(sql, "WHERE FALSE") {
				t.Errorf("SQL applies the drop-to-empty shape, want a real windowed count:\n%s", sql)
			}
		})
	}
}

package promql_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestLower_ExpHistogram_OverTimeIsHistogramValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	for _, tc := range []struct {
		name  string
		query string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{
			name:  "instant sum",
			query: `sum_over_time(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant avg",
			query: `avg_over_time(latency_exp_hist{service="api"}[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "offset",
			query: `sum_over_time(latency_exp_hist[5m] offset 10m)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "range",
			query: `avg_over_time(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
		},
		{
			name:  "range at pin",
			query: `sum_over_time(latency_exp_hist[5m] @ 1767225600)`,
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
			if shape := chplan.RowShapeOf(plan); shape != chplan.HistogramRowShape {
				t.Fatalf("RowShapeOf(lower(%q)) = %s, want %s", tc.query, shape, chplan.HistogramRowShape)
			}
			hp, ok := plan.(*chplan.HistogramProjection)
			if !ok {
				t.Fatalf("lower(%q) root = %T, want *chplan.HistogramProjection", tc.query, plan)
			}
			wantAliases := []string{s.MetricNameColumn, s.AttributesColumn, s.TimestampColumn, s.ValueColumn}
			if !slices.Equal(hp.GroupByAliases, wantAliases) {
				t.Fatalf("lower(%q) leading aliases = %v, want %v", tc.query, hp.GroupByAliases, wantAliases)
			}
			name, ok := hp.GroupBy[0].(*chplan.LitString)
			if !ok || name.V != "" {
				t.Fatalf("lower(%q) name projection = %#v, want empty derived name", tc.query, hp.GroupBy[0])
			}
		})
	}
}

func TestLower_ExpHistogram_OverTimeCompensatesAndPreservesWireShape(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		query   string
		wantDiv bool
	}{
		{query: `sum_over_time(latency_exp_hist[5m])`},
		{query: `avg_over_time(latency_exp_hist[5m])`, wantDiv: true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt: %v", err)
			}
			sql, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			for _, want := range []string{
				"arrayReduce(",
				"arraySort(",
				"`HistogramNegativeBucketCounts`",
			} {
				if !strings.Contains(sql, want) {
					t.Errorf("SQL missing %q:\n%s", want, sql)
				}
			}
			if got := strings.Contains(sql, "/ length("); got != tc.wantDiv {
				t.Errorf("SQL avg division present = %v, want %v:\n%s", got, tc.wantDiv, sql)
			}
			if !slices.Contains(args, "sumKahan") {
				t.Errorf("args do not carry the compensated aggregate name: %v", args)
			}
		})
	}
}

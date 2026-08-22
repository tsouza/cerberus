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

// TestLower_ExpHistogram_LastFirstOverTimeIsHistogramValued pins cerberus
// issue #2480: last_over_time() / first_over_time() over a bare
// exp-histogram selector answer a HISTOGRAM-valued plan that PRESERVES
// `__name__` — reference Prometheus's funcLastOverTime / funcFirstOverTime
// read the window's raw H/F Point and, over an all-histogram window,
// answer `Sample{Metric: el.Metric, H: h.H.Copy()}` — the source series'
// own name and its selected histogram, never dropped and never emptied.
func TestLower_ExpHistogram_LastFirstOverTimeIsHistogramValued(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	for _, tc := range []struct {
		name  string
		query string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{
			name:  "instant last",
			query: `last_over_time(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "instant first",
			query: `first_over_time(latency_exp_hist{service="api"}[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "last offset",
			query: `last_over_time(latency_exp_hist[5m] offset 10m)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "first range",
			query: `first_over_time(latency_exp_hist[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
		},
		{
			name:  "last range at pin",
			query: `last_over_time(latency_exp_hist[5m] @ 1767225600)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr := parseExprExp(t, tc.query)
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
			// __name__ PRESERVED: the MetricName projection is a
			// ColumnRef sourced from the selected row's own MetricName
			// aggregate, NOT the empty-literal every derived (dropping)
			// histogram consumer projects.
			if _, isLit := hp.GroupBy[0].(*chplan.LitString); isLit {
				t.Fatalf("lower(%q) name projection = %#v, want a preserved column reference, not an empty literal", tc.query, hp.GroupBy[0])
			}
			col, ok := hp.GroupBy[0].(*chplan.ColumnRef)
			if !ok || col.Name != s.MetricNameColumn {
				t.Fatalf("lower(%q) name projection = %#v, want ColumnRef{%s}", tc.query, hp.GroupBy[0], s.MetricNameColumn)
			}
		})
	}
}

// TestLower_ExpHistogram_FirstOverTimeSelectsOldest pins the SELECTION
// DIRECTION split: last_over_time must emit argMax(<col>, TimeUnix) (the
// same aggregate a bare exp-histogram selector already uses) while
// first_over_time must emit argMin(<col>, TimeUnix) — never the other
// direction, and never both on the same column.
func TestLower_ExpHistogram_FirstOverTimeSelectsOldest(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	for _, tc := range []struct {
		query    string
		want     string
		unwanted string
	}{
		{query: `last_over_time(latency_exp_hist[5m])`, want: "argMax(", unwanted: "argMin("},
		{query: `first_over_time(latency_exp_hist[5m])`, want: "argMin(", unwanted: ""},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			expr := parseExprExp(t, tc.query)
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt: %v", err)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, tc.want) {
				t.Errorf("SQL missing %q:\n%s", tc.want, sql)
			}
			if tc.unwanted != "" && strings.Contains(sql, tc.unwanted) {
				t.Errorf("SQL unexpectedly contains %q:\n%s", tc.unwanted, sql)
			}
		})
	}
}

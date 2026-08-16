package promql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_AbsentOverTimeIsPresenceOnly pins issue #2226:
// absent_over_time is value-agnostic, so a native-histogram selector must
// reach the dedicated absence node without passing through the scalar Value
// selector route. The modifier corpus covers both evaluation modes and every
// anchor form because all of them share this presence-only input seam.
func TestLower_ExpHistogram_AbsentOverTimeIsPresenceOnly(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	explicitAt := start.Add(2 * time.Minute)

	cases := []struct {
		name       string
		query      string
		lower      func(parser.Expr) (chplan.Node, error)
		wantEnd    time.Time
		wantOffset time.Duration
	}{
		{
			name:  "instant",
			query: `absent_over_time(latency_exp_hist{job="api"}[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
			wantEnd: end,
		},
		{
			name:  "range",
			query: `absent_over_time(latency_exp_hist{job="api"}[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
			wantEnd: end,
		},
		{
			name:  "offset",
			query: `absent_over_time(latency_exp_hist{job="api"}[5m] offset 2m)`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
			wantEnd:    end,
			wantOffset: 2 * time.Minute,
		},
		{
			name:  "explicit_at",
			query: fmt.Sprintf(`absent_over_time(latency_exp_hist{job="api"}[5m] @ %d)`, explicitAt.Unix()),
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
			wantEnd: explicitAt,
		},
		{
			name:  "at_start",
			query: `absent_over_time(latency_exp_hist{job="api"}[5m] @ start())`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
			wantEnd: start,
		},
		{
			name:  "at_end",
			query: `absent_over_time(latency_exp_hist{job="api"}[5m] @ end())`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
			wantEnd: end,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := tc.lower(expr)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}

			var absent *chplan.AbsentOverTime
			chplan.Walk(plan, func(n chplan.Node) bool {
				if a, ok := n.(*chplan.AbsentOverTime); ok {
					absent = a
				}
				return true
			})
			if absent == nil {
				t.Fatalf("Lower(%q): no *chplan.AbsentOverTime in plan", tc.query)
			}
			if !absent.End.Equal(tc.wantEnd) {
				t.Errorf("AbsentOverTime.End = %s, want %s", absent.End, tc.wantEnd)
			}
			if absent.Offset != tc.wantOffset {
				t.Errorf("AbsentOverTime.Offset = %s, want %s", absent.Offset, tc.wantOffset)
			}

			presence, ok := absent.Input.(*chplan.Project)
			if !ok {
				t.Fatalf("AbsentOverTime.Input = %T, want timestamp-only *chplan.Project", absent.Input)
			}
			if len(presence.Projections) != 1 || presence.Projections[0].Alias != s.TimestampColumn {
				t.Fatalf("presence projections = %#v, want only %q", presence.Projections, s.TimestampColumn)
			}

			var scan *chplan.Scan
			chplan.Walk(absent.Input, func(n chplan.Node) bool {
				if candidate, ok := n.(*chplan.Scan); ok {
					scan = candidate
				}
				return true
			})
			if scan == nil || scan.Table != s.ExpHistogramTable {
				t.Fatalf("presence scan = %#v, want table %q", scan, s.ExpHistogramTable)
			}

			chplan.Walk(absent.Input, func(n chplan.Node) bool {
				chplan.InspectNodeExprs(n, func(e chplan.Expr) {
					chplan.InspectExpr(e, func(inner chplan.Expr) bool {
						if col, ok := inner.(*chplan.ColumnRef); ok && col.Name == s.ValueColumn {
							t.Errorf("presence-only input references nonexistent float column %q", s.ValueColumn)
						}
						return true
					})
				})
				return true
			})

			sqlText, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}
			if strings.Contains(sqlText, "`Value` AS `Value` FROM (SELECT * FROM `"+s.ExpHistogramTable+"`") {
				t.Fatalf("native-histogram scan was routed through a float Value projection:\n%s", sqlText)
			}
		})
	}
}

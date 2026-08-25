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

// TestLower_ExpHistogram_AbsentSubqueryIsPresenceOnly pins issue #2602:
// absent()/absent_over_time() are value-agnostic (they only ever ask
// whether ANY sample existed, never what it was), so
// `absent(<exp-histogram selector>)[<range>:<step>]` must lower
// successfully the same way the un-wrapped instant `absent(v)` already
// does (#2443) and `absent_over_time(v[range])` already does (#2226).
//
// Before this fix, lowerSubqueryOverAbsent's bare-VectorSelector branch
// called lowerVectorSelector directly — the full Sample-shape pipeline a
// value-reading consumer needs — which hits expHistogramSelectorRouting's
// catch-all rejection for an exp-histogram metric even though
// chplan.AbsentOverTime never reads the scan's Value column.
func TestLower_ExpHistogram_AbsentSubqueryIsPresenceOnly(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(20 * time.Minute)

	cases := []struct {
		name  string
		query string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{
			name:  "instant",
			query: `absent(latency_exp_hist{job="api"})[10m:1m]`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "range",
			query: `absent(latency_exp_hist{job="api"})[10m:1m]`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(context.Background(), e, s, start, end, time.Minute)
			},
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
				t.Fatalf("Lower(%q): %v (this exact composition was rejected via expHistogramSelectorRouting's catch-all before cerberus issue #2602's fix)", tc.query, err)
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

			// The presence stream must never reference the exp-histogram
			// table's absent scalar Value column: AbsentOverTime.Input only
			// needs the row's existence, so a Value reference would mean
			// the lowering fell back onto the ordinary float selector
			// pipeline this fix bypasses.
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

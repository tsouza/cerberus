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

// TestLower_ExpHistogram_AbsentIsPresenceOnly pins issue #2443: instant
// `absent(v)` is value-agnostic in reference Prometheus (funcAbsent only
// asks len(vectorVals[0]) > 0, never reading a sample's H/F field), so a
// bare exponential-histogram selector argument must lower successfully
// instead of hitting expHistogramSelectorRouting's rejection — the same
// class of bug #2226 fixed for absent_over_time().
func TestLower_ExpHistogram_AbsentIsPresenceOnly(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)

	cases := []struct {
		name  string
		query string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{
			name:  "instant",
			query: `absent(latency_exp_hist{job="api"})`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "range",
			query: `absent(latency_exp_hist{job="api"})`,
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
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}

			var scan *chplan.Scan
			chplan.Walk(plan, func(n chplan.Node) bool {
				if candidate, ok := n.(*chplan.Scan); ok {
					scan = candidate
				}
				return true
			})
			if scan == nil || scan.Table != s.ExpHistogramTable {
				t.Fatalf("presence scan = %#v, want table %q", scan, s.ExpHistogramTable)
			}

			var agg *chplan.Aggregate
			chplan.Walk(plan, func(n chplan.Node) bool {
				if candidate, ok := n.(*chplan.Aggregate); ok && agg == nil {
					agg = candidate
				}
				return true
			})
			if agg == nil || len(agg.AggFuncs) != 1 || agg.AggFuncs[0].Fn != chplan.FnCount {
				t.Fatalf("presence aggregate = %#v, want a single count() aggregate", agg)
			}

			// The presence stream must never reference the exp-histogram
			// table's absent scalar Value column: absent() only counts
			// rows, so a Value reference would mean the lowering fell
			// back onto the ordinary float selector pipeline this fix
			// bypasses.
			chplan.Walk(agg, func(n chplan.Node) bool {
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

// TestLower_ExpHistogram_AbsentRegexNamePresenceHasBareArm pins the
// regex/negated-matcher sibling of #2443 (cerberus issue found in the
// pre-release audit): `absent({__name__=~"..."})` and
// `absent_over_time(...[5m])` over an UNPINNED `__name__` matcher must
// still detect a live exponential-histogram metric's own bare name.
// metricNameFromMatchers returns "" for a regex/negated matcher, so the
// pinned-name fast path in lowerAbsencePresenceSelector never fires; the
// lowering instead falls through to lowerVectorSelector ->
// lowerRegexHistogramSelector, whose union of arms must carry a
// presence-only bare exp-histogram arm (buildRegexExpHistogramBareArm,
// gated on ctx.absencePresenceSelector) or the metric silently
// contributes zero rows and absent() reports it ABSENT even though it has
// live samples.
func TestLower_ExpHistogram_AbsentRegexNamePresenceHasBareArm(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)

	cases := []struct {
		name  string
		query string
		lower func(parser.Expr) (chplan.Node, error)
	}{
		{
			name:  "absent regex name",
			query: `absent({__name__=~"latency_exp_hist"})`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "absent negated name",
			query: `absent({__name__!="other_metric",job="api"})`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
			},
		},
		{
			name:  "absent_over_time regex name",
			query: `absent_over_time({__name__=~"latency_exp_hist"}[5m])`,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, end, end)
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
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}

			// The union feeding the presence check must include a BARE
			// exp-histogram arm — a Project scanning ExpHistogramTable
			// whose MetricName projection is the bare ColumnRef
			// buildRegexExpHistogramBareArm emits, as opposed to the
			// `<base>_count`/`<base>_sum` synthetic-name concat() the
			// companion arms (buildRegexExpHistogramCompanionArm) always
			// contribute. Without the absencePresenceSelector-gated bare
			// arm, only the companion arms scan ExpHistogramTable, and
			// the metric's own bare name silently contributes zero rows.
			var sawBareExpHistogramArm bool
			chplan.Walk(plan, func(n chplan.Node) bool {
				proj, ok := n.(*chplan.Project)
				if !ok {
					return true
				}
				var scansExpHistogram bool
				chplan.Walk(proj, func(inner chplan.Node) bool {
					if scan, ok := inner.(*chplan.Scan); ok && scan.Table == s.ExpHistogramTable {
						scansExpHistogram = true
					}
					return true
				})
				if !scansExpHistogram {
					return true
				}
				for _, p := range proj.Projections {
					if p.Alias != s.MetricNameColumn {
						continue
					}
					if col, ok := p.Expr.(*chplan.ColumnRef); ok && col.Name == s.MetricNameColumn {
						sawBareExpHistogramArm = true
					}
				}
				return true
			})
			if !sawBareExpHistogramArm {
				t.Fatalf("Lower(%q): no bare exp-histogram presence arm (Project scanning %q with a bare MetricName ColumnRef) in plan", tc.query, s.ExpHistogramTable)
			}

			if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}
		})
	}
}

// TestLower_ExpHistogram_AbsentLabelsMirrorMatchers pins the output label
// rule absent() applies over a bare selector argument even when the
// selector names a native-histogram metric: Prom's
// createLabelsForAbsentFunction lifts the argument's equality matchers onto
// the synthesised `{...} 1` row, unaffected by which physical table the
// presence check reads from.
func TestLower_ExpHistogram_AbsentLabelsMirrorMatchers(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	expr, err := p.ParseExpr(`absent(latency_exp_hist{job="api",env="prod"})`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.Project", plan)
	}
	var attrsExpr chplan.Expr
	for _, p := range proj.Projections {
		if p.Alias == s.AttributesColumn {
			attrsExpr = p.Expr
		}
	}
	call, ok := attrsExpr.(*chplan.FuncCall)
	if !ok || call.Fn != chplan.FnMap {
		t.Fatalf("Attributes projection = %#v, want a map() FuncCall", attrsExpr)
	}
	got := map[string]string{}
	for i := 0; i+1 < len(call.Args); i += 2 {
		k, kok := call.Args[i].(*chplan.LitString)
		v, vok := call.Args[i+1].(*chplan.LitString)
		if !kok || !vok {
			t.Fatalf("map() arg pair %d/%d = %#v/%#v, want string literals", i, i+1, call.Args[i], call.Args[i+1])
		}
		got[k.V] = v.V
	}
	want := map[string]string{"job": "api", "env": "prod"}
	if len(got) != len(want) {
		t.Fatalf("Attributes = %#v, want %#v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Attributes[%q] = %q, want %q", k, got[k], v)
		}
	}
}

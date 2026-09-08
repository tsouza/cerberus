package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/qlcommon"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerAbsent_SelectorAppliesTheInstantStalenessWindow pins the
// semantics `absent(<selector>)` must have and did not.
//
// Reference's funcAbsent asks whether the INSTANT vector is empty, and an
// instant vector is the newest sample per series inside the staleness
// lookback. `absent(up)` therefore answers `{} 1` as soon as the series has
// had no sample for five minutes. The previous lowering asked a different
// question — "does this metric have any row in the table, ever" — so a
// metric that stopped reporting hours ago still read as present, which is
// a silently wrong answer for the single most common alerting expression
// in PromQL. It also dropped `@` and `offset` on the floor.
//
// Each assertion below fails on the old table-wide `count() = 0` shape:
// that plan carried no AbsentOverTime node at all.
func TestLowerAbsent_SelectorAppliesTheInstantStalenessWindow(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	end := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := end.Add(-time.Hour)

	pinned := time.Date(2026, 1, 1, 9, 30, 0, 0, time.UTC)

	cases := []struct {
		name       string
		query      string
		rangeMode  bool
		wantOffset time.Duration
		wantEnd    time.Time
		wantStep   time.Duration
	}{
		{
			name:    "instant_plain",
			query:   `absent(up{job="api"})`,
			wantEnd: end,
		},
		{
			name:       "instant_offset_shifts_the_window",
			query:      `absent(up{job="api"} offset 10m)`,
			wantOffset: 10 * time.Minute,
			wantEnd:    end,
		},
		{
			name:    "instant_at_pins_the_window",
			query:   `absent(up{job="api"} @ 1767259800)`, // 2026-01-01T09:30:00Z
			wantEnd: pinned,
		},
		{
			name:      "range_fans_one_verdict_per_step",
			query:     `absent(up{job="api"})`,
			rangeMode: true,
			wantEnd:   end,
			wantStep:  time.Minute,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			var plan chplan.Node
			if tc.rangeMode {
				plan, err = promql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
			} else {
				plan, err = promql.LowerAt(context.Background(), expr, s, end, end)
			}
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}

			var absent *chplan.AbsentOverTime
			chplan.Walk(plan, func(n chplan.Node) bool {
				if candidate, ok := n.(*chplan.AbsentOverTime); ok && absent == nil {
					absent = candidate
				}
				return true
			})
			if absent == nil {
				t.Fatalf(
					"absent(%q) lowered with no AbsentOverTime node, so it applies no staleness "+
						"window: a metric that stopped reporting still reads as present\n%#v",
					tc.query, plan,
				)
			}

			if absent.Range != qlcommon.InstantLookback {
				t.Errorf("window = %v, want the instant staleness lookback %v", absent.Range, qlcommon.InstantLookback)
			}
			if absent.Offset != tc.wantOffset {
				t.Errorf("Offset = %v, want %v — the selector's offset modifier is ignored", absent.Offset, tc.wantOffset)
			}
			if !absent.End.Equal(tc.wantEnd) {
				t.Errorf("End = %v, want %v — the selector's @ pin is ignored", absent.End, tc.wantEnd)
			}
			if absent.Step != tc.wantStep {
				t.Errorf("Step = %v, want %v — range mode must re-evaluate absence per step, not broadcast one verdict", absent.Step, tc.wantStep)
			}
		})
	}
}

// TestLowerAbsent_MatchesAbsentOverTimeOfTheSameWindow states the identity
// the shared lowering rests on: `absent(v)` and
// `absent_over_time(v[<instantLookback>])` are the same question, so they
// must lower to the same plan. If they ever diverge, one of the two is
// answering something reference does not.
func TestLowerAbsent_MatchesAbsentOverTimeOfTheSameWindow(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	end := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, q := range []struct{ absent, overTime string }{
		{`absent(up{job="api"})`, `absent_over_time(up{job="api"}[5m])`},
		{`absent(up{job="api"} offset 10m)`, `absent_over_time(up{job="api"}[5m] offset 10m)`},
		{`absent(up)`, `absent_over_time(up[5m])`},
	} {
		t.Run(q.absent, func(t *testing.T) {
			t.Parallel()

			lower := func(query string) chplan.Node {
				t.Helper()
				expr, err := p.ParseExpr(query)
				if err != nil {
					t.Fatalf("ParseExpr(%q): %v", query, err)
				}
				plan, err := promql.LowerAt(context.Background(), expr, s, end, end)
				if err != nil {
					t.Fatalf("Lower(%q): %v", query, err)
				}
				return plan
			}

			got, want := lower(q.absent), lower(q.overTime)
			if !got.Equal(want) {
				t.Errorf(
					"absent() and absent_over_time() of the same window lower differently:\n"+
						"absent:            %#v\nabsent_over_time:  %#v",
					got, want,
				)
			}
		})
	}
}

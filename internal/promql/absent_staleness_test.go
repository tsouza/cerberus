package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/value"
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
//
// The schema declares no Flags column, so no stale marker can exist and
// the raw-lookback window is exact; a schema with one lowers absent() over
// the instant selection instead
// (TestLowerAbsent_StaleMarkerSchemaReadsTheInstantSelection).
func TestLowerAbsent_SelectorAppliesTheInstantStalenessWindow(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	s.FlagsColumn = ""
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
//
// The identity holds only while no stale marker can exist, so the schema
// declares no Flags column: a stale marker that is the latest sample makes
// the instant vector empty while an earlier raw sample keeps
// absent_over_time's window non-empty.
func TestLowerAbsent_MatchesAbsentOverTimeOfTheSameWindow(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	s.FlagsColumn = ""
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

// TestLowerAbsent_StaleMarkerSchemaReadsTheInstantSelection pins that under
// a schema with a Flags column `absent(<selector>)` asks its question of the
// instant selection — the latest sample per series and step, with a stale
// marker that wins ending the series — rather than of every raw sample in
// the lookback. In range mode the selection's rows already sit on the step
// anchors with any offset applied, so the absence window is one step wide
// and carries no offset of its own. The chDB fixture
// test/spec/promql/stale_marker_absent_range_step.txtar pins the answer
// against reference Prometheus.
func TestLowerAbsent_StaleMarkerSchemaReadsTheInstantSelection(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	end := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := end.Add(-time.Hour)
	const step = time.Minute
	cases := []struct {
		name       string
		query      string
		rangeMode  bool
		wantRange  time.Duration
		wantOffset time.Duration
	}{
		{name: "instant", query: `absent(up{job="api"})`, wantRange: qlcommon.InstantLookback},
		{name: "instant_offset", query: `absent(up{job="api"} offset 10m)`, wantRange: qlcommon.InstantLookback, wantOffset: 10 * time.Minute},
		{name: "range", query: `absent(up{job="api"})`, rangeMode: true, wantRange: step},
		{name: "range_offset", query: `absent(up{job="api"} offset 10m)`, rangeMode: true, wantRange: step},
		{name: "range_at", query: `absent(up{job="api"} @ 1767259800)`, rangeMode: true, wantRange: qlcommon.InstantLookback},
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
				plan, err = promql.LowerAtRange(context.Background(), expr, s, start, end, step)
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
				t.Fatalf("absent(%q) lowered with no AbsentOverTime node:\n%#v", tc.query, plan)
			}
			if absent.Range != tc.wantRange || absent.Offset != tc.wantOffset {
				t.Errorf("window = %v offset %v, want %v offset %v", absent.Range, absent.Offset, tc.wantRange, tc.wantOffset)
			}
			if !isStaleLatestSampleFilter(absent.Input) {
				t.Errorf("absence input is not the instant selection's stale-filtered latest sample:\n%#v", absent.Input)
			}
		})
	}
}

// isStaleLatestSampleFilter reports whether n is the Filter the instant
// selection ends with: `reinterpretAsUInt64(Value) != <stale-marker bits>`.
func isStaleLatestSampleFilter(n chplan.Node) bool {
	f, ok := n.(*chplan.Filter)
	if !ok {
		return false
	}
	b, ok := f.Predicate.(*chplan.Binary)
	if !ok || b.Op != chplan.OpNe {
		return false
	}
	call, ok := b.Left.(*chplan.FuncCall)
	if !ok || call.Fn != chplan.FnReinterpretAsUInt64 {
		return false
	}
	bits, ok := b.Right.(*chplan.LitInt)
	return ok && uint64(bits.V) == value.StaleNaN
}

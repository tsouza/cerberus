package logql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLogLineLimitPushdown_ShapeAndGating is the pipeline-shape
// characterization proof for cerberus issue #2829: for every LogQL
// pipeline shape the lowering supports, a positive LogLineLimit either
// pushes a real `Limit(OrderBy(Timestamp))` on top of the EXACT plan
// [logql.Lower] would otherwise build (proven via chplan.Node.Equal, not
// merely "a plan came back" — the wrap must be purely additive, changing
// no existing node), or leaves the plan byte-identical to the
// LogLineLimit==0 case.
//
// The unsafe cases (a `| pattern` stage followed by a `__error__` /
// `__error_details__` label filter) are the ONLY shapes gated off — see
// internal/logql/lower.go's pipelineCanDropRowsInGo doc comment for why:
// internal/api/loki/post_process.go's newLabelFilterStep is the one
// lineTransform step that can still drop a row in Go, and it only runs
// for exactly that gate. Every other stage kind this package supports
// (line filters, ordinary label filters, every parser family — json,
// logfmt, regexp, unpack, and pattern itself absent a downstream
// error-family filter — line_format, decolorize, label_format, drop,
// keep) either lowers to a real SQL predicate or is a post-fetch
// transform that never drops a row, so pushing a SQL LIMIT on top of the
// unchanged Filter is result-identical to today's decode-everything-then-
// Go-clamp path.
func TestLogLineLimitPushdown_ShapeAndGating(t *testing.T) {
	s := schema.DefaultOTelLogs()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	const pushLimit = 50

	cases := []struct {
		name       string
		query      string
		wantPushed bool
	}{
		{name: "bare stream selector", query: `{app="x"}`, wantPushed: true},
		{name: "line filter |=", query: `{app="x"} |= "boom"`, wantPushed: true},
		{name: "line filter !~", query: `{app="x"} !~ "boom.*"`, wantPushed: true},
		{name: "ordinary label filter (pre-parser)", query: `{app="x"} | level="error"`, wantPushed: true},
		{name: "bare json + label filter on extracted key", query: `{app="x"} | json | status="500"`, wantPushed: true},
		{name: "typed json + label filter", query: `{app="x"} | json code="response.code" | code="500"`, wantPushed: true},
		{name: "bare logfmt + label filter", query: `{app="x"} | logfmt | level="error"`, wantPushed: true},
		{name: "regexp parser + label filter", query: `{app="x"} | regexp "(?P<lvl>[a-z]+)" | lvl="error"`, wantPushed: true},
		{name: "unpack + ordinary label filter", query: `{app="x"} | unpack | level="error"`, wantPushed: true},
		{name: "line_format post-fetch stage", query: `{app="x"} | line_format "{{.msg}}"`, wantPushed: true},
		{name: "decolorize post-fetch stage", query: `{app="x"} | decolorize`, wantPushed: true},
		{name: "label_format post-fetch stage", query: `{app="x"} | label_format renamed=level`, wantPushed: true},
		{name: "drop labels post-fetch stage", query: `{app="x"} | drop level`, wantPushed: true},
		{name: "keep labels post-fetch stage", query: `{app="x"} | keep app`, wantPushed: true},
		{
			name:       "pattern alone, no downstream error filter",
			query:      `{app="x"} | pattern "<lvl> <_>"`,
			wantPushed: true,
		},
		{
			name:       "pattern + ordinary label filter downstream",
			query:      `{app="x"} | pattern "<lvl> <_>" | lvl="error"`,
			wantPushed: true,
		},
		{
			name:       "unpack + __error__ filter (unpack's __error__ IS SQL-lowered, not Go-only)",
			query:      `{app="x"} | unpack | __error__=""`,
			wantPushed: true,
		},

		// The one unsafe shape: pattern's own extraction runs entirely in Go
		// (see internal/logql/lower.go's IsDynamicLabelStage — only `| pattern`
		// sets dynamicLabels), so a downstream __error__/__error_details__
		// filter is evaluated ONLY in Go
		// (internal/api/loki/post_process.go's newLabelFilterStep) — the SQL
		// Filter never encodes it, so pushing a SQL LIMIT ahead of that Go
		// filter could truncate away rows the filter would have kept while
		// admitting rows it would have dropped. See
		// internal/api/loki/limit_pushdown_chdb_test.go for the concrete
		// wrong-result counterexample.
		{
			name:       "pattern + __error__ filter downstream: UNSAFE, not pushed",
			query:      `{app="x"} | pattern "<lvl> <_>" | __error__=""`,
			wantPushed: false,
		},
		{
			name:       "pattern + __error_details__ filter downstream: UNSAFE, not pushed",
			query:      `{app="x"} | pattern "<lvl> <_>" | __error_details__=""`,
			wantPushed: false,
		},
		{
			name:       "pattern + error filter buried in an AND: UNSAFE, not pushed",
			query:      `{app="x"} | pattern "<lvl> <_>" | lvl="error" and __error__=""`,
			wantPushed: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := logql.ParseExprPermissive(tc.query)
			if err != nil {
				t.Fatalf("ParseExprPermissive(%q): %v", tc.query, err)
			}

			basePlan, err := logql.LowerAt(context.Background(), expr, s, start, end)
			if err != nil {
				t.Fatalf("LowerAt (no opts): %v", err)
			}

			pushedPlan, err := logql.LowerAtRangeOpts(context.Background(), expr, s, start, end, 0, logql.LowerOpts{
				LogLineLimit:    pushLimit,
				LogLineBackward: true,
			})
			if err != nil {
				t.Fatalf("LowerAtRangeOpts (with limit): %v", err)
			}

			if !tc.wantPushed {
				if !pushedPlan.Equal(basePlan) {
					t.Fatalf("shape %q: expected LogLineLimit to be a no-op (unsafe shape), but the plan changed:\nbase:   %#v\npushed: %#v", tc.name, basePlan, pushedPlan)
				}
				return
			}

			lim, ok := pushedPlan.(*chplan.Limit)
			if !ok {
				t.Fatalf("shape %q: top node = %T, want *chplan.Limit", tc.name, pushedPlan)
			}
			if lim.Count != pushLimit {
				t.Errorf("shape %q: Limit.Count = %d, want %d", tc.name, lim.Count, pushLimit)
			}
			ob, ok := lim.Input.(*chplan.OrderBy)
			if !ok {
				t.Fatalf("shape %q: Limit.Input = %T, want *chplan.OrderBy", tc.name, lim.Input)
			}
			if len(ob.Keys) != 1 {
				t.Fatalf("shape %q: OrderBy.Keys has %d entries, want 1", tc.name, len(ob.Keys))
			}
			if !ob.Keys[0].Desc {
				t.Errorf("shape %q: OrderBy.Keys[0].Desc = false, want true (LogLineBackward)", tc.name)
			}
			col, ok := ob.Keys[0].Expr.(*chplan.ColumnRef)
			if !ok || col.Name != s.TimestampColumn {
				t.Errorf("shape %q: OrderBy.Keys[0].Expr = %#v, want ColumnRef{Name: %q}", tc.name, ob.Keys[0].Expr, s.TimestampColumn)
			}

			// The wrap must be PURELY additive: peeling off Limit(OrderBy(...))
			// must land on the exact plan Lower() builds without the option —
			// same Scan, same Filter predicate, nothing rewritten underneath.
			if !ob.Input.Equal(basePlan) {
				t.Errorf("shape %q: Limit(OrderBy(...)).Input is not the unmodified base plan:\nbase:  %#v\ninner: %#v", tc.name, basePlan, ob.Input)
			}
		})
	}
}

// TestLogLineLimitPushdown_DirectionMapsToOrderDirection confirms
// LogLineBackward (Loki's default "backward" = most-recent-first
// direction) maps to ORDER BY Timestamp DESC, and false ("forward") maps
// to ASC — the direction convention [parseLogDirection] /
// [clampLogRows] already establish in internal/api/loki/handler.go.
func TestLogLineLimitPushdown_DirectionMapsToOrderDirection(t *testing.T) {
	s := schema.DefaultOTelLogs()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	expr, err := logql.ParseExprPermissive(`{app="x"}`)
	if err != nil {
		t.Fatalf("ParseExprPermissive: %v", err)
	}

	for _, tc := range []struct {
		name     string
		backward bool
		wantDesc bool
	}{
		{name: "backward (default) -> DESC", backward: true, wantDesc: true},
		{name: "forward -> ASC", backward: false, wantDesc: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := logql.LowerAtRangeOpts(context.Background(), expr, s, start, end, 0, logql.LowerOpts{
				LogLineLimit:    10,
				LogLineBackward: tc.backward,
			})
			if err != nil {
				t.Fatalf("LowerAtRangeOpts: %v", err)
			}
			lim, ok := plan.(*chplan.Limit)
			if !ok {
				t.Fatalf("top node = %T, want *chplan.Limit", plan)
			}
			ob := lim.Input.(*chplan.OrderBy)
			if ob.Keys[0].Desc != tc.wantDesc {
				t.Errorf("Desc = %v, want %v", ob.Keys[0].Desc, tc.wantDesc)
			}
		})
	}
}

// TestLogLineLimitPushdown_NoOpWhenLimitZeroOrMetricQuery confirms two
// more no-op cases beyond the unsafe-pipeline gate:
//
//   - LogLineLimit<=0 (every non-Opts entry point, and every caller that
//     hasn't parsed a request `limit`) never pushes.
//   - A metric query (RangeAggregationExpr et al.) never pushes even
//     when LogLineLimit is positive — [maybePushLogLineLimit] only fires
//     when the TOP-level parsed expr is itself a syntax.LogSelectorExpr,
//     which a metric query's top-level expr never is (see
//     logql.IsMetricQuery). This is what keeps a metric query's inner
//     selector (reached recursively while lowering the range
//     aggregation) from ever being wrapped.
func TestLogLineLimitPushdown_NoOpWhenLimitZeroOrMetricQuery(t *testing.T) {
	s := schema.DefaultOTelLogs()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	t.Run("LogLineLimit<=0 no-ops", func(t *testing.T) {
		expr, err := logql.ParseExprPermissive(`{app="x"}`)
		if err != nil {
			t.Fatalf("ParseExprPermissive: %v", err)
		}
		base, err := logql.LowerAt(context.Background(), expr, s, start, end)
		if err != nil {
			t.Fatalf("LowerAt: %v", err)
		}
		zero, err := logql.LowerAtRangeOpts(context.Background(), expr, s, start, end, 0, logql.LowerOpts{LogLineLimit: 0, LogLineBackward: true})
		if err != nil {
			t.Fatalf("LowerAtRangeOpts: %v", err)
		}
		if !zero.Equal(base) {
			t.Errorf("LogLineLimit=0 changed the plan; want byte-identical to Lower without opts")
		}
	})

	t.Run("metric query is never wrapped", func(t *testing.T) {
		expr, err := logql.ParseExprPermissive(`count_over_time({app="x"}[5m])`)
		if err != nil {
			t.Fatalf("ParseExprPermissive: %v", err)
		}
		base, err := logql.LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
		if err != nil {
			t.Fatalf("LowerAtRange: %v", err)
		}
		pushed, err := logql.LowerAtRangeOpts(context.Background(), expr, s, start, end, time.Minute, logql.LowerOpts{LogLineLimit: 50, LogLineBackward: true})
		if err != nil {
			t.Fatalf("LowerAtRangeOpts: %v", err)
		}
		if !pushed.Equal(base) {
			t.Errorf("a metric query's plan changed when LogLineLimit was set; want byte-identical — the limit must never reach a metric query's plan")
		}
		if _, ok := pushed.(*chplan.Limit); ok {
			t.Errorf("metric query plan's top node is *chplan.Limit; must never be wrapped")
		}
	})
}

package prom

import (
	"context"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/solver"
)

// The solver derives its RequestMeta from the PLAN, not the request: it asks
// solver.GridOf for the outer eval grid and treats a zero grid as proof the
// query is instant. So the grid a query_range plan reports must not depend on
// WHICH lowering strategy built it — two plans for the same expression over the
// same request window must report the same grid, or the classification records
// a range query as an instant one.
//
// This is the end-to-end pin for that invariant across the whole request
// path: parse -> lower (with the boot-wired strategy table) -> ProjectSamples
// -> optimize -> GridOf. The per-carrier unit coverage lives in
// internal/solver/grid_of_test.go and the completeness ratchet for the carrier
// set in internal/chplan/grid_carrier_completeness_test.go; this test is what
// proves the chain holds on a real lowered plan.

const nativeGridPinStep = 30 * time.Second

func nativeGridPinWindow() (time.Time, time.Time) {
	start := time.Date(2029, 7, 8, 9, 0, 0, 0, time.UTC)
	return start, start.Add(time.Hour)
}

// planForRangeQuery runs the full range-query plan build for q under the
// supplied lowering strategy table, exactly as the query_range handler does.
func planForRangeQuery(t *testing.T, q string, lowerers promql.RangeLowerers) chplan.Node {
	t.Helper()

	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	s := schema.DefaultOTelMetrics()
	start, end := nativeGridPinWindow()

	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, start, end, nativeGridPinStep,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower %q: %v", q, err)
	}

	// The engine wraps every PromQL plan with the Sample projection before it
	// optimizes and classifies, so the classified plan is rooted on a Project.
	l := &lang{Schema: s}
	plan = l.ProjectSamples(plan, engine.Meta{IsMetric: true})

	return optimizer.Default().Run(context.Background(), plan)
}

// countNativeNodes reports how many native ts-grid nodes plan carries — the
// guard against a case that silently proves nothing because the expression fell
// back to the fan-out lowering.
func countNativeNodes(plan chplan.Node) int {
	n := 0
	chplan.Walk(plan, func(node chplan.Node) bool {
		switch node.(type) {
		case *chplan.RangeWindowNative, *chplan.RangeWindowResample:
			n++
		}
		return true
	})
	return n
}

// TestRangeQueryGrid_IndependentOfLoweringStrategy pins the invariant: for the
// same expression over the same request window, the grid the plan reports is
// the request grid regardless of which lowering built it. A strategy whose
// carrier the grid reader cannot see reports a zero grid, which is
// indistinguishable from a genuine instant query.
func TestRangeQueryGrid_IndependentOfLoweringStrategy(t *testing.T) {
	t.Parallel()

	wantStart, wantEnd := nativeGridPinWindow()

	// Expressions chosen to cover the shapes whose ONLY grid carrier is a
	// native node: a by-less aggregation over a vector-vector join (two native
	// operands, no fan-out window anywhere in the plan), the same with a
	// grouping key, the bare two-operand form, and a single-sided control.
	for _, q := range []string{
		"sum(rate(requests_total[5m])) / sum(rate(attempts_total[5m]))",
		"sum by(job) (rate(requests_total[5m])) / sum by(job) (rate(attempts_total[5m]))",
		"rate(requests_total[5m]) / rate(attempts_total[5m])",
		"sum by(job) (rate(requests_total[5m]))",
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()

			native := planForRangeQuery(t, q, promql.RangeLowerers{
				Rate: promql.NativeRateLowerer{Fallback: promql.FanoutRateLowerer{}},
			})
			if got := countNativeNodes(native); got == 0 {
				t.Fatalf("plan carries no native ts-grid node — this case would pass on the fan-out "+
					"lowering alone and prove nothing (native nodes: %d)", got)
			}

			gotStart, gotEnd, gotStep := solver.GridOf(native)
			if gotStep != nativeGridPinStep {
				t.Errorf("native lowering reports step %s, want %s — a zero step is read as "+
					"'instant query' and the plan is never cost-analyzed at all", gotStep, nativeGridPinStep)
			}
			if !gotStart.Equal(wantStart) {
				t.Errorf("native lowering reports start %s, want %s", gotStart, wantStart)
			}
			if !gotEnd.Equal(wantEnd) {
				t.Errorf("native lowering reports end %s, want %s", gotEnd, wantEnd)
			}

			// The fan-out lowering of the same expression is the control: both
			// strategies must agree, since only the lowering differs.
			fanoutStart, fanoutEnd, fanoutStep := solver.GridOf(planForRangeQuery(t, q, promql.RangeLowerers{}))
			if fanoutStep != gotStep || !fanoutStart.Equal(gotStart) || !fanoutEnd.Equal(gotEnd) {
				t.Errorf("grid differs by lowering strategy: native=(%s,%s,%s) fanout=(%s,%s,%s)",
					gotStart, gotEnd, gotStep, fanoutStart, fanoutEnd, fanoutStep)
			}
		})
	}
}

// TestRangeQueryGrid_InstantQueryStillReportsNoGrid pins the converse: an
// instant query must keep reporting a zero grid, so the Planner's instant guard
// still fires. Widening the grid reader must not reclassify instant queries as
// range queries.
func TestRangeQueryGrid_InstantQueryStillReportsNoGrid(t *testing.T) {
	t.Parallel()

	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr("sum(rate(requests_total[5m])) / sum(rate(attempts_total[5m]))")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s := schema.DefaultOTelMetrics()
	_, evalAt := nativeGridPinWindow()

	plan, err := promql.LowerAt(context.Background(), expr, s, evalAt, evalAt)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	l := &lang{Schema: s}
	optimized := optimizer.Default().Run(context.Background(), l.ProjectSamples(plan, engine.Meta{IsMetric: true}))

	if _, _, step := solver.GridOf(optimized); step != 0 {
		t.Errorf("instant query reports step %s, want 0 — the Planner's instant guard would stop firing", step)
	}
}

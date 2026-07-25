package loki_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/solver"
)

// PR #1272 (commit ddd4a2f5) fixed a bug where RangeWindowNative,
// RangeWindowResample, and AbsentOverTime were missing from the old
// grid-classification kind-switch: a plan whose ONLY grid carrier was one
// of those node kinds silently reported a zero grid, indistinguishable
// from a genuine instant query. internal/api/prom/native_range_grid_test.go
// pins the fix end-to-end for PromQL. LogQL's absent_over_time lowers to
// the very same chplan.AbsentOverTime node (see
// internal/logql/range_aggregation.go's lowerAbsentOverTime), so it is
// exposed to the identical bug shape — this test is the LogQL analog,
// proving the invariant holds across the real LogQL lowering pipeline
// too, not just PromQL's.
const absentOverTimeGridPinStep = time.Minute

func absentOverTimeGridPinWindow() (time.Time, time.Time) {
	start := time.Date(2029, 7, 8, 9, 0, 0, 0, time.UTC)
	return start, start.Add(5 * time.Minute)
}

// TestAbsentOverTimeRangeQuery_GridIsRequestGrid pins the invariant for
// LogQL: an absent_over_time(...) range query — whose optimized plan's
// only grid carrier is *chplan.AbsentOverTime — must report the
// request's real (start, end, step) through solver.GridOf, not a zero
// grid. A zero grid here would be read as "instant query" and the
// range-query cost analysis / routing would never fire.
func TestAbsentOverTimeRangeQuery_GridIsRequestGrid(t *testing.T) {
	t.Parallel()

	const q = `absent_over_time({job="x"}[5m])`
	start, end := absentOverTimeGridPinWindow()
	s := schema.DefaultOTelLogs()

	l := &logql.Lang{Schema: s, Start: start, End: end, Step: absentOverTimeGridPinStep}
	plan, meta, err := l.Parse(context.Background(), q)
	if err != nil {
		t.Fatalf("parse+lower %q: %v", q, err)
	}
	plan = l.ProjectSamples(plan, meta)
	plan = optimizer.Default().Run(context.Background(), plan)

	// Guard: confirm the optimized plan's only grid carrier is
	// *chplan.AbsentOverTime, so this test actually exercises the bug
	// shape rather than silently passing because the query fell back to
	// some other grid-carrying node.
	carriers := 0
	chplan.Walk(plan, func(node chplan.Node) bool {
		if _, ok := node.(chplan.GridCarrier); ok {
			carriers++
			if _, ok := node.(*chplan.AbsentOverTime); !ok {
				t.Fatalf("unexpected grid carrier %T — expected only *chplan.AbsentOverTime", node)
			}
		}
		return true
	})
	if carriers == 0 {
		t.Fatal("plan carries no grid carrier node — this case would prove nothing about the fix")
	}

	gotStart, gotEnd, gotStep := solver.GridOf(plan)
	if gotStep != absentOverTimeGridPinStep {
		t.Errorf("absent_over_time range query reports step %s, want %s — a zero step is read as "+
			"'instant query' and the plan is never cost-analyzed at all", gotStep, absentOverTimeGridPinStep)
	}
	if !gotStart.Equal(start) {
		t.Errorf("absent_over_time range query reports start %s, want %s", gotStart, start)
	}
	if !gotEnd.Equal(end) {
		t.Errorf("absent_over_time range query reports end %s, want %s", gotEnd, end)
	}
}

// TestAbsentOverTimeInstantQuery_StillReportsNoGrid pins the converse:
// an instant absent_over_time query must keep reporting a zero grid, so
// the instant/range classification isn't accidentally widened to treat
// every absent_over_time query as a range query.
func TestAbsentOverTimeInstantQuery_StillReportsNoGrid(t *testing.T) {
	t.Parallel()

	const q = `absent_over_time({job="x"}[5m])`
	_, evalAt := absentOverTimeGridPinWindow()
	s := schema.DefaultOTelLogs()

	l := &logql.Lang{Schema: s, Start: evalAt, End: evalAt}
	plan, meta, err := l.Parse(context.Background(), q)
	if err != nil {
		t.Fatalf("parse+lower %q: %v", q, err)
	}
	plan = l.ProjectSamples(plan, meta)
	plan = optimizer.Default().Run(context.Background(), plan)

	if _, _, step := solver.GridOf(plan); step != 0 {
		t.Errorf("instant absent_over_time query reports step %s, want 0 — the instant classification "+
			"would stop firing", step)
	}
}

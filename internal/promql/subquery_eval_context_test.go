package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestCarriesEvalContext pins which inner expressions a subquery lowers
// at its own grid: those holding a node that fixes or moves its own
// evaluation instant.
func TestCarriesEvalContext(t *testing.T) {
	t.Parallel()
	cases := []struct {
		inner string
		want  bool
	}{
		{`m`, false},
		{`rate(m[5m])`, false},
		{`sum(m) + 1`, false},
		{`m @ 1767225540`, true},
		{`m @ start()`, true},
		{`m @ end()`, true},
		{`m offset 1h`, true},
		{`m offset -1h`, true},
		{`max_over_time(m[2m] offset 1h)`, true},
		{`max_over_time(m[5m] @ 1767225480)`, true},
		{`count_over_time(vector(1)[2m:1m])`, true},
		{`sum(rate(m[5m])) / sum(m offset 5m)`, true},
	}
	for _, tc := range cases {
		expr, err := parser.NewParser(parser.Options{}).ParseExpr(tc.inner)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.inner, err)
		}
		if got := carriesEvalContext(expr); got != tc.want {
			t.Errorf("carriesEvalContext(%q) = %v, want %v", tc.inner, got, tc.want)
		}
	}
}

// TestLowerSubquery_ContextInnerLowersOnGrid pins the routing: a subquery
// whose inner expression carries evaluation context is lowered at its own
// grid, marked by a [chplan.Project] with SubqueryGrid, in instant and
// range mode; one without context keeps the folded lowering.
func TestLowerSubquery_ContextInnerLowersOnGrid(t *testing.T) {
	t.Parallel()
	end := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	start := end.Add(-5 * time.Minute)
	cases := []struct {
		query string
		grid  bool
	}{
		{`max_over_time(rate(m[5m])[5m:1m])`, false},
		{`max_over_time(m[5m:1m])`, false},
		{`max_over_time((m @ 1767225540)[5m:1m] @ 1767225600)`, true},
		{`max_over_time((m offset 1h)[5m:1m])`, true},
		{`count_over_time((m @ end())[5m:1m] @ 1767000000)`, true},
		{`max_over_time(count_over_time(vector(1)[2m:45s])[5m:1m])`, true},
		{`max_over_time(max_over_time(timestamp(vector(1))[2m:20s])[5m:1m] @ 1767225630)`, true},
		{`max_over_time(max_over_time(timestamp(vector(1))[5m:1m])[3m:1m] offset -1h)`, true},
	}
	s := schema.DefaultOTelMetrics()
	for _, tc := range cases {
		expr, err := parser.NewParser(parser.Options{}).ParseExpr(tc.query)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.query, err)
		}
		instant, err := LowerAt(context.Background(), expr, s, end, end)
		if err != nil {
			t.Fatalf("instant lower %q: %v", tc.query, err)
		}
		ranged, err := LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
		if err != nil {
			t.Fatalf("range lower %q: %v", tc.query, err)
		}
		for mode, plan := range map[string]chplan.Node{"instant": instant, "range": ranged} {
			if got := holdsSubqueryGrid(plan); got != tc.grid {
				t.Errorf("%s %q: lowered at its own grid = %v, want %v", mode, tc.query, got, tc.grid)
			}
		}
	}
}

// holdsSubqueryGrid reports whether plan holds a subquery lowered at its
// own grid.
func holdsSubqueryGrid(plan chplan.Node) bool {
	found := false
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if p, ok := n.(*chplan.Project); ok && p.SubqueryGrid {
			found = true
		}
		return !found
	})
	return found
}

// TestLowerCtxOnGrid_KeepsRequestBounds pins that `@ start()` and
// `@ end()` name the request's instants however many grids replace the
// lowering's start and end.
func TestLowerCtxOnGrid_KeepsRequestBounds(t *testing.T) {
	t.Parallel()
	reqStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reqEnd := reqStart.Add(time.Hour)
	ctx := lowerCtx{start: reqStart, end: reqEnd}
	outer := ctx.onGrid(reqStart.Add(-10*time.Minute), reqEnd, time.Minute)
	inner := outer.onGrid(reqStart.Add(-20*time.Minute), reqEnd.Add(-time.Minute), 20*time.Second)
	for name, c := range map[string]lowerCtx{"request": ctx, "outer grid": outer, "inner grid": inner} {
		if !c.requestStart().Equal(reqStart) || !c.requestEnd().Equal(reqEnd) {
			t.Errorf("%s: request bounds = [%s, %s], want [%s, %s]", name, c.requestStart(), c.requestEnd(), reqStart, reqEnd)
		}
	}
	if !inner.start.Equal(reqStart.Add(-20*time.Minute)) || inner.step != 20*time.Second {
		t.Errorf("inner grid: start, step = %s, %s, want the grid's own", inner.start, inner.step)
	}
}

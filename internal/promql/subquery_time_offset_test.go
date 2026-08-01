package promql

import (
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestLowerSubqueryOverBinary_ShiftsSyntheticGridForOffset(t *testing.T) {
	t.Parallel()

	const query = `(vector(time()) - vector(1))[5m:1m] offset 2m`
	start := time.Date(2026, time.January, 1, 0, 10, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	subqueryRange := 5 * time.Minute
	offset := 2 * time.Minute

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	sub, ok := expr.(*parser.SubqueryExpr)
	if !ok {
		t.Fatalf("parsed %T, want *parser.SubqueryExpr", expr)
	}
	plan, err := lowerSubquery(sub, schema.DefaultOTelMetrics(), lowerCtx{
		start:    start,
		end:      end,
		step:     time.Minute,
		lowerers: RangeLowerers{}.withDefaults(),
	})
	if err != nil {
		t.Fatalf("lowerSubquery(%q): %v", query, err)
	}
	grid := firstStepGrid(plan)
	if grid == nil {
		t.Fatal("lowered subquery has no synthetic StepGrid")
	}
	wantStart := start.Add(-subqueryRange - offset)
	if !grid.Start.Equal(wantStart) {
		t.Errorf("StepGrid.Start = %s, want %s", grid.Start, wantStart)
	}
	if !grid.End.Equal(end.Add(-offset)) {
		t.Errorf("StepGrid.End = %s, want %s", grid.End, end.Add(-offset))
	}
}

func TestLowerSubqueryOverBinary_LowersAggregateOnInnerStepGrid(t *testing.T) {
	t.Parallel()

	const query = `(time() - max(demo_batch_last_success_timestamp_seconds) < 1000)[5m:10s]`
	start := time.Date(2026, time.January, 1, 0, 10, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	sub, ok := expr.(*parser.SubqueryExpr)
	if !ok {
		t.Fatalf("parsed %T, want *parser.SubqueryExpr", expr)
	}
	plan, err := lowerSubquery(sub, schema.DefaultOTelMetrics(), lowerCtx{
		start:    start,
		end:      end,
		step:     10 * time.Second,
		lowerers: RangeLowerers{}.withDefaults(),
	})
	if err != nil {
		t.Fatalf("lowerSubquery(%q): %v", query, err)
	}
	if firstRangeLWR(plan) == nil {
		t.Fatal("lowered subquery aggregate did not use range-mode staleness on the inner step grid")
	}
}

func firstStepGrid(plan chplan.Node) *chplan.StepGrid {
	if grid, ok := plan.(*chplan.StepGrid); ok {
		return grid
	}
	for _, child := range plan.Children() {
		if grid := firstStepGrid(child); grid != nil {
			return grid
		}
	}
	return nil
}

func firstRangeLWR(plan chplan.Node) *chplan.RangeLWR {
	if lwr, ok := plan.(*chplan.RangeLWR); ok {
		return lwr
	}
	for _, child := range plan.Children() {
		if lwr := firstRangeLWR(child); lwr != nil {
			return lwr
		}
	}
	return nil
}

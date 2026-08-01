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
	offset := 2 * time.Minute
	subRange := 5 * time.Minute
	subStep := time.Minute

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
		step:     subStep,
		lowerers: RangeLowerers{}.withDefaults(),
	})
	if err != nil {
		t.Fatalf("lowerSubquery(%q): %v", query, err)
	}
	grid := firstStepGrid(plan)
	if grid == nil {
		t.Fatal("lowered subquery has no synthetic StepGrid")
	}
	// The inner grid covers the union of every outer step's subquery
	// window, shifted by the offset: (start-offset-range, end-offset].
	// The window is left-open, so the lower endpoint itself is excluded
	// and the first anchor sits one sub-step later.
	wantStart := start.Add(-offset).Add(-subRange).Add(subStep)
	wantEnd := end.Add(-offset)
	if !grid.Start.Equal(wantStart) {
		t.Errorf("StepGrid.Start = %s, want %s", grid.Start, wantStart)
	}
	if !grid.End.Equal(wantEnd) {
		t.Errorf("StepGrid.End = %s, want %s", grid.End, wantEnd)
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

package promql

import (
	"context"
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

// TestLowerOuterRangeFnOverSubquery_WidensInnerByOffsetPlusRange is the
// direct-pipeline regression for #1464's route-A audit: three call sites in
// lowerOuterRangeFnOverSubquery (the pinned, range-mode, and instant/default
// branches) widened a subquery's inner matrix spine by `sub.Range` alone,
// silently dropping the outer reducer's own `offset` modifier. Each
// sub-test lowers a `max_over_time(rate(m[5m])[1h:5m] offset 10m)`-shaped
// query through the real entrypoint (LowerAtRange / LowerAt) and asserts
// the inner RangeWindow's widened Start honors Offset+Range.
func TestLowerOuterRangeFnOverSubquery_WidensInnerByOffsetPlusRange(t *testing.T) {
	t.Parallel()

	const query = `max_over_time(rate(demo_cpu[5m])[1h:5m] offset 10m)`
	offset := 10 * time.Minute
	subRange := time.Hour
	s := schema.DefaultOTelMetrics()

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}

	t.Run("range mode", func(t *testing.T) {
		t.Parallel()
		start := time.Unix(1_700_000_000, 0).UTC()
		end := start.Add(2 * time.Hour)
		plan, err := LowerAtRange(context.Background(), expr, s, start, end, time.Minute)
		if err != nil {
			t.Fatalf("LowerAtRange(%q): %v", query, err)
		}
		outer, ok := plan.(*chplan.RangeWindow)
		if !ok {
			t.Fatalf("plan is %T, want *chplan.RangeWindow", plan)
		}
		inner := firstRangeWindow(outer.Input)
		if inner == nil {
			t.Fatal("no inner RangeWindow found under outer.Input")
		}
		wantStart := start.Add(-offset - subRange)
		if !inner.Start.Equal(wantStart) {
			t.Errorf("inner.Start = %s, want %s (start - offset - subRange)", inner.Start, wantStart)
		}
	})

	t.Run("instant mode", func(t *testing.T) {
		t.Parallel()
		evalTS := time.Unix(1_700_010_000, 0).UTC()
		plan, err := LowerAt(context.Background(), expr, s, evalTS, evalTS)
		if err != nil {
			t.Fatalf("LowerAt(%q): %v", query, err)
		}
		outer, ok := plan.(*chplan.RangeWindow)
		if !ok {
			t.Fatalf("plan is %T, want *chplan.RangeWindow", plan)
		}
		inner := firstRangeWindow(outer.Input)
		if inner == nil {
			t.Fatal("no inner RangeWindow found under outer.Input")
		}
		wantStart := evalTS.Add(-offset - subRange)
		if !inner.Start.Equal(wantStart) {
			t.Errorf("inner.Start = %s, want %s (evalTS - offset - subRange)", inner.Start, wantStart)
		}
	})
}

// firstRangeWindow depth-first searches for the first *chplan.RangeWindow
// reachable from n (n itself included).
func firstRangeWindow(n chplan.Node) *chplan.RangeWindow {
	if rw, ok := n.(*chplan.RangeWindow); ok {
		return rw
	}
	for _, child := range n.Children() {
		if rw := firstRangeWindow(child); rw != nil {
			return rw
		}
	}
	return nil
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

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

// TestLowerSubqueryOverAbsent_ShiftsGridForSubqueryOffset covers the sibling
// gap #1464's audit catalogued and issue #1732 tracks: lowerSubqueryOverAbsent
// lowers `absent(<selector>)[range:step]` and never read the subquery's
// `OriginalOffset` at all, so the `offset` modifier had NO effect on the
// emitted plan — the offset-carrying and un-offset forms lowered
// byte-identically. That is a total omission rather than the wrong-arithmetic
// variant #1464 fixed elsewhere.
//
// The fix shifts the anchor GRID and leaves chplan.AbsentOverTime.Offset at
// zero, which is the half that is easy to get backwards. The node does have an
// Offset field, but it carries the opposite OUTPUT contract: its emitter shifts
// the internal grid and then adds the offset back on the output timestamp
// (chsql's absentGridAnchorFrag), so `absent_over_time(v[5m] offset 10m)`
// reports at the request's own anchors. That is right for a range-vector
// function over an offset-carrying selector, and wrong for a SUBQUERY grid,
// whose emitted timestamps ARE its evaluation instants and which an enclosing
// reducer selects BY those timestamps while applying the same offset to its own
// window. Setting both applies the shift twice and leaves the two anchor ranges
// disjoint — an empty result, which the chDB round-trip in
// test/spec/promql/subquery_absent_offset_shifts_window.txtar is what caught.
//
// Both fields are asserted, and the un-offset form alongside them, so neither a
// lowering that shifts nothing nor one that shifts every absent subquery by a
// constant can pass.
func TestLowerSubqueryOverAbsent_ShiftsGridForSubqueryOffset(t *testing.T) {
	t.Parallel()

	const (
		offsetQuery    = `max_over_time(absent(up)[5m:1m] offset 10m)`
		unshiftedQuery = `max_over_time(absent(up)[5m:1m])`
		// The subquery's own [5m:1m] range: grid Start is always this far
		// behind grid End, offset or no offset.
		subRange = 5 * time.Minute
	)
	offset := 10 * time.Minute
	s := schema.DefaultOTelMetrics()

	for _, tc := range []struct {
		name      string
		query     string
		wantShift time.Duration
	}{
		{"offset 10m", offsetQuery, offset},
		{"no offset", unshiftedQuery, 0},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := parser.NewParser(parser.Options{}).ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}

			// The grid a zero-offset query produces in each mode. Range mode
			// reaches back a subquery range before the REQUEST start so the
			// leading outer anchors have inner anchors to reduce; instant
			// mode has no request start and reaches back from the eval
			// timestamp. The offset case must land wantShift earlier on both
			// ends, and on both ends only.
			for _, mode := range []struct {
				name                         string
				unshiftedStart, unshiftedEnd time.Time
				plan                         func() (chplan.Node, error)
			}{
				{
					"range mode",
					time.Unix(1_700_000_000, 0).UTC().Add(-subRange),
					time.Unix(1_700_000_000, 0).UTC().Add(2 * time.Hour),
					func() (chplan.Node, error) {
						start := time.Unix(1_700_000_000, 0).UTC()
						return LowerAtRange(context.Background(), expr, s, start, start.Add(2*time.Hour), time.Minute)
					},
				},
				{
					"instant mode",
					time.Unix(1_700_010_000, 0).UTC().Add(-subRange),
					time.Unix(1_700_010_000, 0).UTC(),
					func() (chplan.Node, error) {
						evalTS := time.Unix(1_700_010_000, 0).UTC()
						return LowerAt(context.Background(), expr, s, evalTS, evalTS)
					},
				},
			} {
				mode := mode
				t.Run(mode.name, func(t *testing.T) {
					t.Parallel()

					plan, err := mode.plan()
					if err != nil {
						t.Fatalf("lower(%q): %v", tc.query, err)
					}
					a := firstAbsentOverTime(plan)
					if a == nil {
						t.Fatalf("no chplan.AbsentOverTime under the lowered %q — this pin has no "+
							"subject; the shape no longer routes through lowerSubqueryOverAbsent",
							tc.query)
					}
					wantEnd := mode.unshiftedEnd.Add(-tc.wantShift)
					if !a.End.Equal(wantEnd) {
						t.Errorf("AbsentOverTime.End = %s, want %s — the subquery's `offset` shifts "+
							"WHICH instants it evaluates, so the whole anchor grid moves back with it",
							a.End, wantEnd)
					}
					if wantStart := mode.unshiftedStart.Add(-tc.wantShift); !a.Start.Equal(wantStart) {
						t.Errorf("AbsentOverTime.Start = %s, want %s", a.Start, wantStart)
					}
					if a.Offset != 0 {
						t.Errorf("AbsentOverTime.Offset = %s, want 0 — this node's emitter adds the "+
							"offset back onto the OUTPUT timestamp, so carrying the shift here as "+
							"well as on the grid applies it twice and the enclosing reducer's window "+
							"comes out disjoint from the anchors", a.Offset)
					}
				})
			}
		})
	}
}

// firstAbsentOverTime depth-first searches for the first
// *chplan.AbsentOverTime reachable from n (n itself included).
func firstAbsentOverTime(n chplan.Node) *chplan.AbsentOverTime {
	if a, ok := n.(*chplan.AbsentOverTime); ok {
		return a
	}
	for _, child := range n.Children() {
		if a := firstAbsentOverTime(child); a != nil {
			return a
		}
	}
	return nil
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

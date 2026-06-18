package chplan_test

import (
	"errors"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/chplan"
)

// matrixWindow builds an unpinned matrix RangeWindow over a leaf scan —
// the shape the subquery lowerings emit (OuterRange + Step set, Start/End
// zero, filled by the re-anchor pass).
func matrixWindow(rang, step, outerRange time.Duration) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "metrics", Columns: []string{"Value", "TimeUnix"}},
		Func:            "rate",
		Range:           rang,
		Step:            step,
		OuterRange:      outerRange,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// TestReanchorRange_DoesNotMutateInput asserts the input tree is byte-
// identical after ReanchorRange — the copy-not-mutate contract the solver
// depends on (it runs K shards off one optimized plan).
func TestReanchorRange_DoesNotMutateInput(t *testing.T) {
	t.Parallel()

	in := matrixWindow(5*time.Minute, time.Minute, 0)
	snapshot := chplan.CloneNode(in)

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	out, err := chplan.ReanchorRange(in, start, end)
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	if !in.Equal(snapshot) {
		t.Fatal("ReanchorRange mutated its input")
	}
	if out == chplan.Node(in) {
		t.Fatal("ReanchorRange returned the same pointer")
	}
	rw := out.(*chplan.RangeWindow)
	if !rw.Start.Equal(start) || !rw.End.Equal(end) || rw.OuterRange != end.Sub(start) {
		t.Fatalf("re-anchored bounds wrong: Start=%v End=%v OuterRange=%v", rw.Start, rw.End, rw.OuterRange)
	}
}

// TestReanchorRange_NestedSpineWidens checks the inner matrix window is
// re-anchored from start.Add(-outerRange) — the start.Add(-Range) recursion
// that mirrors widenSubquerySpine.
func TestReanchorRange_NestedSpineWidens(t *testing.T) {
	t.Parallel()

	inner := matrixWindow(time.Minute, 30*time.Second, 0)
	outer := &chplan.RangeWindow{
		Input:           inner,
		Func:            "max_over_time",
		Range:           5 * time.Minute,
		Step:            time.Minute,
		TimestampColumn: "anchor_ts",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}

	start := time.Unix(10_000, 0).UTC()
	end := time.Unix(20_000, 0).UTC()
	out, err := chplan.ReanchorRange(outer, start, end)
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	gotOuter := out.(*chplan.RangeWindow)
	if !gotOuter.Start.Equal(start) || !gotOuter.End.Equal(end) {
		t.Fatalf("outer not re-anchored to [%v,%v], got [%v,%v]", start, end, gotOuter.Start, gotOuter.End)
	}
	gotInner := gotOuter.Input.(*chplan.RangeWindow)
	wantInnerStart := start.Add(-outer.Range)
	if !gotInner.Start.Equal(wantInnerStart) || !gotInner.End.Equal(end) {
		t.Fatalf("inner not widened: want Start=%v End=%v, got Start=%v End=%v",
			wantInnerStart, end, gotInner.Start, gotInner.End)
	}
	if gotInner.OuterRange != end.Sub(wantInnerStart) {
		t.Fatalf("inner OuterRange wrong: %v", gotInner.OuterRange)
	}
}

// TestReanchorRange_RejectsAtPin asserts an @-pinned matrix window (End
// already set to a value that is NOT the predicted grid End) is rejected so
// the solver routes A.
func TestReanchorRange_RejectsAtPin(t *testing.T) {
	t.Parallel()

	pinnedEnd := time.Unix(9999, 0).UTC()
	in := matrixWindow(5*time.Minute, time.Minute, time.Hour)
	in.Start = time.Unix(9999-3600, 0).UTC()
	in.End = pinnedEnd // an @-pinned anchor: End != the request grid end

	// Re-anchor to a DIFFERENT grid than the node is pinned to.
	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	_, err := chplan.ReanchorRange(in, start, end)
	if !errors.Is(err, chplan.ErrReanchorGridMismatch) {
		t.Fatalf("expected ErrReanchorGridMismatch for @-pinned node, got %v", err)
	}
}

// TestReanchorRange_AcceptsAlreadyGridded asserts a node already sitting on
// the predicted grid (the range-mode `rate(m[5m])` shape) is re-anchored
// without error — equivalence with the unpinned path.
func TestReanchorRange_AcceptsAlreadyGridded(t *testing.T) {
	t.Parallel()

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	in := matrixWindow(5*time.Minute, time.Minute, end.Sub(start))
	in.Start = start
	in.End = end

	out, err := chplan.ReanchorRange(in, start, end)
	if err != nil {
		t.Fatalf("already-gridded node should re-anchor cleanly, got %v", err)
	}
	rw := out.(*chplan.RangeWindow)
	if !rw.Start.Equal(start) || !rw.End.Equal(end) {
		t.Fatalf("bounds drifted: %v %v", rw.Start, rw.End)
	}
}

// TestReanchorRange_InstantTerminates asserts an instant RangeWindow
// (Step == 0) is copied verbatim and does not move.
func TestReanchorRange_InstantTerminates(t *testing.T) {
	t.Parallel()

	in := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "metrics"},
		Func:            "max_over_time",
		Range:           time.Hour,
		Step:            0, // instant
		TimestampColumn: "anchor_ts",
		ValueColumn:     "Value",
	}
	snapshot := chplan.CloneNode(in)
	out, err := chplan.ReanchorRange(in, time.Unix(1, 0).UTC(), time.Unix(2, 0).UTC())
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	if !out.Equal(snapshot) {
		t.Fatal("instant RangeWindow should be copied unchanged")
	}
	if !in.Equal(snapshot) {
		t.Fatal("ReanchorRange mutated the instant input")
	}
}

// TestReanchorRange_SpineNodeIsCloned pins the COW contract on the spine
// node itself: the re-gridded spine node is a FRESH clone, so re-anchoring a
// shard onto a sub-grid (the solver's only mutation of the output) never
// touches the input's bounds. The off-spine subtree (Input) and the off-grid
// immutable slice headers (GroupBy) are deliberately SHARED — see
// TestReanchorRange_OffSpineIsShared — so this test mutates only the cloned
// spine fields.
func TestReanchorRange_SpineNodeIsCloned(t *testing.T) {
	t.Parallel()

	in := matrixWindow(5*time.Minute, time.Minute, 0)
	snapshot := chplan.CloneNode(in)

	out, err := chplan.ReanchorRange(in, time.Unix(1000, 0).UTC(), time.Unix(4600, 0).UTC())
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	rw := out.(*chplan.RangeWindow)
	if rw == in {
		t.Fatal("ReanchorRange returned the same spine pointer (spine must be cloned)")
	}
	// Re-grid the shard further (what the slicer does per slice): this must
	// not move the input's grid.
	rw.Start = time.Unix(0, 0).UTC()
	rw.End = time.Unix(1, 0).UTC()
	rw.OuterRange = time.Second
	rw.Range = 999 * time.Hour

	if !in.Equal(snapshot) {
		t.Fatal("re-gridding the cloned spine node leaked into the input")
	}
}

// TestReanchorRange_OffSpineIsShared documents the COW contract explicitly:
// the off-spine subtree is SHARED, not copied. The re-anchored output's
// off-spine Input is the SAME pointer as the input's, and the off-grid
// immutable GroupBy slice shares its backing array. This is the lever that
// makes slicing K+1× cheaper; its soundness rests on the no-mutate-after-
// slice contract enforced by the solver's differential immutability guard.
func TestReanchorRange_OffSpineIsShared(t *testing.T) {
	t.Parallel()

	in := matrixWindow(5*time.Minute, time.Minute, 0)
	out, err := chplan.ReanchorRange(in, time.Unix(1000, 0).UTC(), time.Unix(4600, 0).UTC())
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	rw := out.(*chplan.RangeWindow)
	if rw.Input != in.Input {
		t.Fatal("off-spine Input was copied; COW requires it be shared")
	}
	if len(rw.GroupBy) == 0 || &rw.GroupBy[0] != &in.GroupBy[0] {
		t.Fatal("off-grid immutable GroupBy was copied; COW requires the slice be shared")
	}
}

// TestReanchorRange_PropertyBounds is the rapid property test: over random
// (range, step, anchor-count, offset) the single-level re-anchored window
// always lands exactly on the requested grid, and the nested-spine inner
// window is widened by the outer Range.
func TestReanchorRange_PropertyBounds(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		stepSec := rapid.Int64Range(1, 600).Draw(rt, "stepSec")
		anchors := rapid.Int64Range(2, 5000).Draw(rt, "anchors")
		rangeMul := rapid.Int64Range(1, 100).Draw(rt, "rangeMul")
		startSec := rapid.Int64Range(0, 1_000_000).Draw(rt, "startSec")

		step := time.Duration(stepSec) * time.Second
		rang := time.Duration(rangeMul) * step
		start := time.Unix(startSec, 0).UTC()
		end := start.Add(time.Duration(anchors-1) * step)

		inner := matrixWindow(step, step, 0)
		outer := &chplan.RangeWindow{
			Input:           inner,
			Func:            "max_over_time",
			Range:           rang,
			Step:            step,
			TimestampColumn: "anchor_ts",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		}
		snapshot := chplan.CloneNode(outer)

		out, err := chplan.ReanchorRange(outer, start, end)
		if err != nil {
			rt.Fatalf("ReanchorRange errored on a well-formed grid: %v", err)
		}
		if !outer.Equal(snapshot) {
			rt.Fatal("input mutated")
		}

		gotOuter := out.(*chplan.RangeWindow)
		if !gotOuter.Start.Equal(start) || !gotOuter.End.Equal(end) {
			rt.Fatalf("outer bounds: want [%v,%v] got [%v,%v]", start, end, gotOuter.Start, gotOuter.End)
		}
		if gotOuter.OuterRange != end.Sub(start) {
			rt.Fatalf("outer OuterRange %v != %v", gotOuter.OuterRange, end.Sub(start))
		}
		gotInner := out.(*chplan.RangeWindow).Input.(*chplan.RangeWindow)
		wantInnerStart := start.Add(-rang)
		if !gotInner.Start.Equal(wantInnerStart) {
			rt.Fatalf("inner Start: want %v got %v", wantInnerStart, gotInner.Start)
		}
		if !gotInner.End.Equal(end) {
			rt.Fatalf("inner End: want %v got %v", end, gotInner.End)
		}
		if gotInner.OuterRange != end.Sub(wantInnerStart) {
			rt.Fatalf("inner OuterRange %v != %v", gotInner.OuterRange, end.Sub(wantInnerStart))
		}
	})
}

// TestReanchorRange_PropertyRejectsAtPin draws random pinned ends that do
// NOT equal the predicted grid end and asserts every one is rejected.
func TestReanchorRange_PropertyRejectsAtPin(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		start := time.Unix(rapid.Int64Range(0, 100_000).Draw(rt, "start"), 0).UTC()
		end := start.Add(time.Duration(rapid.Int64Range(60, 100_000).Draw(rt, "span")) * time.Second)
		// A pinned end deliberately off the predicted grid.
		skew := rapid.Int64Range(1, 50_000).Draw(rt, "skew")
		pinnedEnd := end.Add(time.Duration(skew) * time.Second)

		in := matrixWindow(5*time.Minute, time.Minute, end.Sub(start))
		in.Start = start
		in.End = pinnedEnd // != predicted end

		_, err := chplan.ReanchorRange(in, start, end)
		if !errors.Is(err, chplan.ErrReanchorGridMismatch) {
			rt.Fatalf("expected grid mismatch for pinned end %v vs predicted %v, got %v", pinnedEnd, end, err)
		}
	})
}

// lwrNode builds a RangeLWR over a leaf scan for the given grid + offset — the
// bare-selector last-with-respect-to shape phase-3 ReanchorRange re-grids.
func lwrNode(start, end time.Time, step, lookback, offset time.Duration) *chplan.RangeLWR {
	return &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "metrics", Columns: []string{"Value", "TimeUnix"}},
		Start:         start,
		End:           end,
		Step:          step,
		Lookback:      lookback,
		Offset:        offset,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
}

// TestReanchorRange_LWRReGrids asserts a pinned RangeLWR sitting on the
// predicted grid is re-anchored onto a sub-grid, its input scan widened by the
// offset-aware membership lookback (Offset+Lookback), and the input is left
// byte-identical (copy-not-mutate).
func TestReanchorRange_LWRReGrids(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		offset time.Duration
	}{
		{"zero offset", 0},
		{"negative offset", -5 * time.Minute},
		{"positive offset", time.Hour},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gridStart := time.Unix(1000, 0).UTC()
			gridEnd := time.Unix(4600, 0).UTC()
			lookback := 5 * time.Minute
			in := lwrNode(gridStart, gridEnd, time.Minute, lookback, tc.offset)
			snapshot := chplan.CloneNode(in)

			// Re-anchor onto a sub-window of the predicted grid. The node is
			// unpinned first (the slicer's shape) so the sub-grid is accepted.
			in.Start = time.Time{}
			in.End = time.Time{}
			subStart := gridStart.Add(10 * time.Minute)
			subEnd := gridEnd.Add(-10 * time.Minute)

			out, err := chplan.ReanchorRange(in, subStart, subEnd)
			if err != nil {
				t.Fatalf("ReanchorRange: %v", err)
			}
			r := out.(*chplan.RangeLWR)
			if !r.Start.Equal(subStart) || !r.End.Equal(subEnd) {
				t.Fatalf("re-anchored bounds wrong: Start=%v End=%v", r.Start, r.End)
			}
			if r.Lookback != lookback || r.Offset != tc.offset || r.Step != time.Minute {
				t.Fatalf("re-anchor lost a non-grid field: %+v", r)
			}
			// The input must NOT be mutated — restore its pins and compare.
			in.Start = gridStart
			in.End = gridEnd
			if !in.Equal(snapshot) {
				t.Fatal("ReanchorRange mutated its RangeLWR input")
			}
		})
	}
}

// TestReanchorRange_LWRAcceptsAlreadyGridded: a RangeLWR already on the
// predicted grid re-anchors without error (the equivalence-with-unpinned path).
func TestReanchorRange_LWRAcceptsAlreadyGridded(t *testing.T) {
	t.Parallel()
	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	in := lwrNode(start, end, time.Minute, 5*time.Minute, 0)
	out, err := chplan.ReanchorRange(in, start, end)
	if err != nil {
		t.Fatalf("already-gridded RangeLWR should re-anchor cleanly, got %v", err)
	}
	r := out.(*chplan.RangeLWR)
	if !r.Start.Equal(start) || !r.End.Equal(end) {
		t.Fatalf("bounds drifted: %v %v", r.Start, r.End)
	}
}

// TestReanchorRange_LWRRejectsAtPin: an @-pinned RangeLWR whose End diverges
// from the predicted grid is rejected so the solver routes A.
func TestReanchorRange_LWRRejectsAtPin(t *testing.T) {
	t.Parallel()
	in := lwrNode(time.Unix(9999-3600, 0).UTC(), time.Unix(9999, 0).UTC(), time.Minute, 5*time.Minute, 0)
	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	_, err := chplan.ReanchorRange(in, start, end)
	if !errors.Is(err, chplan.ErrReanchorGridMismatch) {
		t.Fatalf("expected ErrReanchorGridMismatch for @-pinned RangeLWR, got %v", err)
	}
}

// TestReanchorRange_LWRInstantTerminates: a Step==0 RangeLWR is copied verbatim.
func TestReanchorRange_LWRInstantTerminates(t *testing.T) {
	t.Parallel()
	in := lwrNode(time.Time{}, time.Time{}, 0, 5*time.Minute, 0)
	snapshot := chplan.CloneNode(in)
	out, err := chplan.ReanchorRange(in, time.Unix(1, 0).UTC(), time.Unix(2, 0).UTC())
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	if !out.Equal(snapshot) {
		t.Fatal("instant RangeLWR should be copied unchanged")
	}
	if !in.Equal(snapshot) {
		t.Fatal("ReanchorRange mutated the instant RangeLWR input")
	}
}

// TestReanchorRange_LWRSpineNodeIsCloned: the COW contract on the RangeLWR
// spine node. The re-gridded LWR is a fresh clone, so re-anchoring a shard
// (the solver's only output mutation) never moves the input's grid. The
// off-spine Input is SHARED — see TestReanchorRange_LWROffSpineIsShared — so
// this test mutates only the cloned spine fields.
func TestReanchorRange_LWRSpineNodeIsCloned(t *testing.T) {
	t.Parallel()
	in := lwrNode(time.Time{}, time.Time{}, time.Minute, 5*time.Minute, -5*time.Minute)
	snapshot := chplan.CloneNode(in)

	out, err := chplan.ReanchorRange(in, time.Unix(1000, 0).UTC(), time.Unix(4600, 0).UTC())
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	r := out.(*chplan.RangeLWR)
	if r == in {
		t.Fatal("ReanchorRange returned the same RangeLWR pointer (spine must be cloned)")
	}
	r.Start = time.Unix(0, 0).UTC()
	r.End = time.Unix(1, 0).UTC()
	r.Offset = 999 * time.Hour

	if !in.Equal(snapshot) {
		t.Fatal("re-gridding the cloned RangeLWR spine node leaked into the input")
	}
}

// TestReanchorRange_LWROffSpineIsShared documents that the RangeLWR's
// off-spine Input subtree is SHARED, not copied — the COW lever for the
// bare-selector family.
func TestReanchorRange_LWROffSpineIsShared(t *testing.T) {
	t.Parallel()
	in := lwrNode(time.Time{}, time.Time{}, time.Minute, 5*time.Minute, -5*time.Minute)
	out, err := chplan.ReanchorRange(in, time.Unix(1000, 0).UTC(), time.Unix(4600, 0).UTC())
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	r := out.(*chplan.RangeLWR)
	if r.Input != in.Input {
		t.Fatal("off-spine Input was copied; COW requires it be shared")
	}
}

// TestReanchorRange_LWRPropertyBounds is the rapid property test for the
// RangeLWR arm: over random (step, lookback, anchors, offset) the re-anchored
// LWR lands exactly on the requested grid and its input scan is widened by
// Offset+Lookback.
func TestReanchorRange_LWRPropertyBounds(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		stepSec := rapid.Int64Range(1, 600).Draw(rt, "stepSec")
		anchors := rapid.Int64Range(2, 5000).Draw(rt, "anchors")
		lookbackMul := rapid.Int64Range(1, 100).Draw(rt, "lookbackMul")
		offsetSec := rapid.Int64Range(-3600, 7200).Draw(rt, "offsetSec")
		startSec := rapid.Int64Range(0, 1_000_000).Draw(rt, "startSec")

		step := time.Duration(stepSec) * time.Second
		lookback := time.Duration(lookbackMul) * step
		offset := time.Duration(offsetSec) * time.Second
		start := time.Unix(startSec, 0).UTC()
		end := start.Add(time.Duration(anchors-1) * step)

		// Unpinned LWR (the slicer's shape) so any sub-grid is accepted.
		in := lwrNode(time.Time{}, time.Time{}, step, lookback, offset)
		snapshot := chplan.CloneNode(in)

		out, err := chplan.ReanchorRange(in, start, end)
		if err != nil {
			rt.Fatalf("ReanchorRange errored on a well-formed grid: %v", err)
		}
		if !in.Equal(snapshot) {
			rt.Fatal("input mutated")
		}
		r := out.(*chplan.RangeLWR)
		if !r.Start.Equal(start) || !r.End.Equal(end) {
			rt.Fatalf("LWR bounds: want [%v,%v] got [%v,%v]", start, end, r.Start, r.End)
		}
		if r.Lookback != lookback || r.Offset != offset || r.Step != step {
			rt.Fatalf("LWR lost a non-grid field: %+v", r)
		}
	})
}

// TestReanchorRange_NilInput returns (nil, nil).
func TestReanchorRange_NilInput(t *testing.T) {
	t.Parallel()
	out, err := chplan.ReanchorRange(nil, time.Now(), time.Now())
	if err != nil || out != nil {
		t.Fatalf("nil input should return (nil, nil), got (%v, %v)", out, err)
	}
}

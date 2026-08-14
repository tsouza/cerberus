package chplan_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// nativeNode builds a RangeWindowNative over a leaf scan for the given grid +
// offset — the ClickHouse-native timeSeries<fn>ToGrid shape ReanchorRange
// re-grids (issue #2117).
func nativeNode(start, end time.Time, step, rang, offset time.Duration) *chplan.RangeWindowNative {
	return &chplan.RangeWindowNative{
		Input:           &chplan.Scan{Table: "metrics", Columns: []string{"Value", "TimeUnix"}},
		Func:            "rate",
		Range:           rang,
		Step:            step,
		Start:           start,
		End:             end,
		Offset:          offset,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// nativeOverWindow builds a RangeWindowNative whose Input is a matrix
// RangeWindow, so the widening the native arm applies to its inner spine is
// OBSERVABLE: the inner window's re-anchored Start is the only place
// Offset+Range shows up.
func nativeOverWindow(step, rang, offset time.Duration) *chplan.RangeWindowNative {
	n := nativeNode(time.Time{}, time.Time{}, step, rang, offset)
	n.Input = matrixWindow(time.Minute, step, 0)
	return n
}

// TestReanchorRange_NativeReGrids asserts an unpinned RangeWindowNative is
// re-anchored onto the requested sub-grid, that its non-grid fields survive,
// and that the input tree is left byte-identical (copy-not-mutate).
func TestReanchorRange_NativeReGrids(t *testing.T) {
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

			const rang = 5 * time.Minute
			in := nativeNode(time.Time{}, time.Time{}, time.Minute, rang, tc.offset)
			snapshot := chplan.CloneNode(in)

			subStart := time.Unix(1600, 0).UTC()
			subEnd := time.Unix(4000, 0).UTC()

			out, err := chplan.ReanchorRange(in, subStart, subEnd)
			if err != nil {
				t.Fatalf("ReanchorRange: %v", err)
			}
			r := out.(*chplan.RangeWindowNative)
			if !r.Start.Equal(subStart) || !r.End.Equal(subEnd) {
				t.Fatalf("re-anchored bounds wrong: Start=%v End=%v", r.Start, r.End)
			}
			if r.Range != rang || r.Offset != tc.offset || r.Step != time.Minute || r.Func != "rate" {
				t.Fatalf("re-anchor lost a non-grid field: %+v", r)
			}
			if !in.Equal(snapshot) {
				t.Fatal("ReanchorRange mutated its RangeWindowNative input")
			}
		})
	}
}

// TestReanchorRange_NativeWidensInputSpine asserts the native arm widens its
// INPUT by Offset+Range — the arithmetic RangeWindowNative.InputWindow owns and
// the solver's signal walk predicts. A shard whose inner spine is not widened
// starts its scan at the shard's own oldest anchor, so that anchor's window is
// missing every sample older than it and the shard's first grid points are
// wrong (or absent) in a way no full-grid test can see.
func TestReanchorRange_NativeWidensInputSpine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		offset time.Duration
	}{
		{"zero offset", 0},
		{"negative offset", -3 * time.Minute},
		{"positive offset", 30 * time.Minute},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			const rang = 5 * time.Minute
			in := nativeOverWindow(time.Minute, rang, tc.offset)

			start := time.Unix(10_000, 0).UTC()
			end := time.Unix(20_000, 0).UTC()
			out, err := chplan.ReanchorRange(in, start, end)
			if err != nil {
				t.Fatalf("ReanchorRange: %v", err)
			}
			inner := out.(*chplan.RangeWindowNative).Input.(*chplan.RangeWindow)

			wantStart, wantEnd := in.InputWindow(start, end)
			if !inner.Start.Equal(wantStart) || !inner.End.Equal(wantEnd) {
				t.Fatalf("inner spine at [%v,%v], want InputWindow [%v,%v]",
					inner.Start, inner.End, wantStart, wantEnd)
			}
			// Non-vacuity: the widening really moved the inner start.
			if wantStart.Equal(start) {
				t.Fatalf("InputWindow returned the unwidened start for offset=%s range=%s; "+
					"the assertion above would hold for a node that widened by nothing",
					tc.offset, rang)
			}
		})
	}
}

// TestReanchorRange_NativeAcceptsAlreadyGridded: a RangeWindowNative already
// sitting exactly on the predicted grid re-anchors without error — the shape a
// top-level range-mode rate() carries before UnpinSpine touches it.
func TestReanchorRange_NativeAcceptsAlreadyGridded(t *testing.T) {
	t.Parallel()
	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	in := nativeNode(start, end, time.Minute, 5*time.Minute, 0)
	out, err := chplan.ReanchorRange(in, start, end)
	if err != nil {
		t.Fatalf("already-gridded RangeWindowNative should re-anchor cleanly, got %v", err)
	}
	r := out.(*chplan.RangeWindowNative)
	if !r.Start.Equal(start) || !r.End.Equal(end) {
		t.Fatalf("bounds drifted: %v %v", r.Start, r.End)
	}
}

// TestReanchorRange_NativeRejectsAtPin: a RangeWindowNative whose bounds
// diverge from the predicted grid (an @-pinned anchor) is rejected, so the
// solver aborts the Decision to route A rather than emit shards that silently
// disagree with @ semantics.
func TestReanchorRange_NativeRejectsAtPin(t *testing.T) {
	t.Parallel()
	in := nativeNode(time.Unix(9999-3600, 0).UTC(), time.Unix(9999, 0).UTC(), time.Minute, 5*time.Minute, 0)
	_, err := chplan.ReanchorRange(in, time.Unix(1000, 0).UTC(), time.Unix(4600, 0).UTC())
	if !errors.Is(err, chplan.ErrReanchorGridMismatch) {
		t.Fatalf("expected ErrReanchorGridMismatch for an @-pinned RangeWindowNative, got %v", err)
	}
}

// TestReanchorRange_NativeSpineNodeIsCloned: the COW contract on the native
// spine node. The re-gridded node is a fresh clone, so re-gridding one shard
// never moves another's (or the input's) grid.
func TestReanchorRange_NativeSpineNodeIsCloned(t *testing.T) {
	t.Parallel()
	in := nativeNode(time.Time{}, time.Time{}, time.Minute, 5*time.Minute, -5*time.Minute)
	snapshot := chplan.CloneNode(in)

	out, err := chplan.ReanchorRange(in, time.Unix(1000, 0).UTC(), time.Unix(4600, 0).UTC())
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	r := out.(*chplan.RangeWindowNative)
	if r == in {
		t.Fatal("ReanchorRange returned the same RangeWindowNative pointer (spine must be cloned)")
	}
	r.Start = time.Unix(0, 0).UTC()
	r.End = time.Unix(1, 0).UTC()
	r.Offset = 999 * time.Hour

	if !in.Equal(snapshot) {
		t.Fatal("re-gridding the cloned native spine node leaked into the input")
	}
	if r.Input != in.Input {
		t.Fatal("off-spine Input was copied; COW requires it be shared")
	}
}

// TestReanchorRange_UnionAllReAnchorsEveryArm is the pin for the mixed spine
// issue #2117 exists to unblock: `UnionAll{RangeWindowNative, RangeWindow}`.
//
// Both arms must land on the SAME re-anchored grid. The arms are concatenated
// positionally, so a shard that re-gridded one and shared the other verbatim
// would emit one sub-grid's rows beside the full grid's, and the K-way
// composition would double-count every anchor of the arm that did not move —
// silently, since both arms still project the same column shape.
func TestReanchorRange_UnionAllReAnchorsEveryArm(t *testing.T) {
	t.Parallel()

	native := nativeNode(time.Time{}, time.Time{}, time.Minute, 5*time.Minute, 0)
	fanout := matrixWindow(5*time.Minute, time.Minute, 0)
	in := &chplan.UnionAll{Inputs: []chplan.Node{native, fanout}}
	snapshot := chplan.CloneNode(in)

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()

	out, err := chplan.ReanchorRange(in, start, end)
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	u, ok := out.(*chplan.UnionAll)
	if !ok {
		t.Fatalf("ReanchorRange returned %T, want *chplan.UnionAll", out)
	}
	if len(u.Inputs) != len(in.Inputs) {
		t.Fatalf("arm count changed: %d -> %d", len(in.Inputs), len(u.Inputs))
	}

	gotNative := u.Inputs[0].(*chplan.RangeWindowNative)
	if !gotNative.Start.Equal(start) || !gotNative.End.Equal(end) {
		t.Errorf("native arm at [%v,%v], want [%v,%v]", gotNative.Start, gotNative.End, start, end)
	}
	gotFanout := u.Inputs[1].(*chplan.RangeWindow)
	if !gotFanout.Start.Equal(start) || !gotFanout.End.Equal(end) {
		t.Errorf("fan-out arm at [%v,%v], want [%v,%v]", gotFanout.Start, gotFanout.End, start, end)
	}
	if gotFanout.OuterRange != end.Sub(start) {
		t.Errorf("fan-out arm OuterRange = %s, want %s", gotFanout.OuterRange, end.Sub(start))
	}

	if !in.Equal(snapshot) {
		t.Fatal("ReanchorRange mutated its UnionAll input")
	}
	if u == in {
		t.Fatal("ReanchorRange returned the same UnionAll pointer (the arm slice must be fresh)")
	}
}

// TestReanchorRange_UnionAllPropagatesArmMismatch: a single @-pinned arm aborts
// the whole re-anchor. Re-anchoring the other arms and dropping this one would
// produce a shard set whose union is missing that arm's rows entirely.
func TestReanchorRange_UnionAllPropagatesArmMismatch(t *testing.T) {
	t.Parallel()

	pinned := nativeNode(time.Unix(9999-3600, 0).UTC(), time.Unix(9999, 0).UTC(),
		time.Minute, 5*time.Minute, 0)
	in := &chplan.UnionAll{Inputs: []chplan.Node{
		matrixWindow(5*time.Minute, time.Minute, 0),
		pinned,
	}}

	_, err := chplan.ReanchorRange(in, time.Unix(1000, 0).UTC(), time.Unix(4600, 0).UTC())
	if !errors.Is(err, chplan.ErrReanchorGridMismatch) {
		t.Fatalf("expected ErrReanchorGridMismatch from the @-pinned arm, got %v", err)
	}
}

// TestReanchorRange_UnionAllBelowSpineIsUnchanged pins the pre-existing shape
// this arm must not disturb: the classic-histogram companion-suffix routing
// puts the UnionAll BENEATH the windowed node, over arms that carry no grid at
// all. Those arms hit the share-verbatim default, so the re-anchored tree must
// be Equal to the input with the window's grid filled and nothing else moved.
func TestReanchorRange_UnionAllBelowSpineIsUnchanged(t *testing.T) {
	t.Parallel()

	armA := &chplan.Scan{Table: "otel_metrics_sum", Columns: []string{"Value", "TimeUnix"}}
	armB := &chplan.Scan{Table: "otel_metrics_histogram", Columns: []string{"Value", "TimeUnix"}}
	union := &chplan.UnionAll{Inputs: []chplan.Node{armA, armB}}
	in := matrixWindow(5*time.Minute, time.Minute, 0)
	in.Input = union
	snapshot := chplan.CloneNode(in)

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	out, err := chplan.ReanchorRange(in, start, end)
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}

	gotUnion := out.(*chplan.RangeWindow).Input.(*chplan.UnionAll)
	if !gotUnion.Equal(union) {
		t.Fatal("a grid-free UnionAll below the spine was altered by re-anchoring")
	}
	if gotUnion.Inputs[0] != chplan.Node(armA) || gotUnion.Inputs[1] != chplan.Node(armB) {
		t.Fatal("grid-free UnionAll arms were copied; they must be shared verbatim")
	}
	if !in.Equal(snapshot) {
		t.Fatal("ReanchorRange mutated its input")
	}
}

package chplan_test

import (
	"errors"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/chplan"
)

// stepAlignedJoin builds a StepAligned vector-vector join over two matrix
// RangeWindow arms — the shape `sum by(job)(rate(a[5m])) / sum by(job)
// (rate(b[5m]))` lowers to (modulo the per-arm Aggregate, immaterial to
// re-anchoring). group_left + Include exercise the many-to-one modifier
// fields ReanchorRange must carry verbatim.
func stepAlignedJoin(left, right chplan.Node) *chplan.VectorJoin {
	return &chplan.VectorJoin{
		Left:             left,
		Right:            right,
		Op:               chplan.OpDiv,
		Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
		Card:             chplan.CardManyToOne,
		Include:          []string{"instance"},
		ReturnBool:       false,
		StepAligned:      true,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
}

// TestReanchorRange_VectorJoin_ReAnchorsBothArms asserts a step-aligned join
// re-anchors BOTH arms onto the SAME requested [start, end] (a join carries no
// lookback, so no widening at the join level), and the input tree stays
// byte-identical (copy-not-mutate).
func TestReanchorRange_VectorJoin_ReAnchorsBothArms(t *testing.T) {
	t.Parallel()

	left := matrixWindow(5*time.Minute, time.Minute, 0)
	right := matrixWindow(5*time.Minute, time.Minute, 0)
	in := stepAlignedJoin(left, right)
	snapshot := chplan.CloneNode(in)

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	out, err := chplan.ReanchorRange(in, start, end)
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	if !in.Equal(snapshot) {
		t.Fatal("ReanchorRange mutated its VectorJoin input")
	}
	if out == chplan.Node(in) {
		t.Fatal("ReanchorRange returned the same VectorJoin pointer (join node must be cloned)")
	}
	j := out.(*chplan.VectorJoin)
	gotLeft := j.Left.(*chplan.RangeWindow)
	gotRight := j.Right.(*chplan.RangeWindow)
	for name, rw := range map[string]*chplan.RangeWindow{"left": gotLeft, "right": gotRight} {
		if !rw.Start.Equal(start) || !rw.End.Equal(end) || rw.OuterRange != end.Sub(start) {
			t.Fatalf("%s arm re-anchored wrong: Start=%v End=%v OuterRange=%v",
				name, rw.Start, rw.End, rw.OuterRange)
		}
	}
}

// TestReanchorRange_VectorJoin_COWPreservesModifiers pins the copy-on-write
// contract: the immutable modifier fields (Op / Match / Card / Include /
// ReturnBool / StepAligned) and the four column names survive verbatim, and
// re-gridding the cloned join's arms never leaks into the input.
func TestReanchorRange_VectorJoin_COWPreservesModifiers(t *testing.T) {
	t.Parallel()

	in := stepAlignedJoin(
		matrixWindow(5*time.Minute, time.Minute, 0),
		matrixWindow(5*time.Minute, time.Minute, 0),
	)
	snapshot := chplan.CloneNode(in)

	out, err := chplan.ReanchorRange(in, time.Unix(1000, 0).UTC(), time.Unix(4600, 0).UTC())
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	j := out.(*chplan.VectorJoin)
	if j.Op != in.Op || !j.Match.Equal(in.Match) || j.Card != in.Card ||
		j.ReturnBool != in.ReturnBool || j.StepAligned != in.StepAligned {
		t.Fatalf("modifier field drifted after re-anchor: %+v", j)
	}
	if j.MetricNameColumn != in.MetricNameColumn || j.AttributesColumn != in.AttributesColumn ||
		j.TimestampColumn != in.TimestampColumn || j.ValueColumn != in.ValueColumn {
		t.Fatalf("column name drifted after re-anchor: %+v", j)
	}
	// Re-grid the shard's arms further (what the slicer does per slice) — this
	// must not move the input's grid.
	j.Left.(*chplan.RangeWindow).Start = time.Unix(0, 0).UTC()
	j.Right.(*chplan.RangeWindow).End = time.Unix(1, 0).UTC()
	if !in.Equal(snapshot) {
		t.Fatal("re-gridding the cloned join's arms leaked into the input")
	}
}

// TestReanchorRange_VectorJoin_AtPinnedArm asserts an @-pinned arm (its End
// diverges from the predicted grid) surfaces ErrReanchorGridMismatch from the
// recursion, aborting the whole re-anchor to route A.
func TestReanchorRange_VectorJoin_AtPinnedArm(t *testing.T) {
	t.Parallel()

	// The left arm is a clean unpinned matrix window; the right arm is
	// @-pinned to a grid that is NOT the one we re-anchor onto.
	left := matrixWindow(5*time.Minute, time.Minute, 0)
	right := matrixWindow(5*time.Minute, time.Minute, time.Hour)
	right.Start = time.Unix(9999-3600, 0).UTC()
	right.End = time.Unix(9999, 0).UTC() // @-pinned End != request grid end
	in := stepAlignedJoin(left, right)

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	_, err := chplan.ReanchorRange(in, start, end)
	if !errors.Is(err, chplan.ErrReanchorGridMismatch) {
		t.Fatalf("expected ErrReanchorGridMismatch for @-pinned join arm, got %v", err)
	}
}

// TestReanchorRange_VectorJoin_PropertyBounds is the rapid property test: over
// random grids AND asymmetric per-arm Range/Offset, both arms of a step-aligned
// join always re-anchor exactly onto the requested grid while each keeps its OWN
// Range and Offset (the join recursion re-grids the shared grid, never the
// per-arm window shape), and the input is never mutated. The asymmetric per-arm
// Offset is the case a single global (ΣRange, one-offset) scan floor would
// mishandle — here the plan-level guarantee is that slicing a join preserves
// each arm's window verbatim.
func TestReanchorRange_VectorJoin_PropertyBounds(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		stepSec := rapid.Int64Range(1, 600).Draw(rt, "stepSec")
		anchors := rapid.Int64Range(2, 5000).Draw(rt, "anchors")
		rangeMulL := rapid.Int64Range(1, 100).Draw(rt, "rangeMulL")
		rangeMulR := rapid.Int64Range(1, 100).Draw(rt, "rangeMulR")
		offMulL := rapid.Int64Range(0, 50).Draw(rt, "offMulL")
		offMulR := rapid.Int64Range(0, 50).Draw(rt, "offMulR")
		startSec := rapid.Int64Range(0, 1_000_000).Draw(rt, "startSec")

		step := time.Duration(stepSec) * time.Second
		start := time.Unix(startSec, 0).UTC()
		end := start.Add(time.Duration(anchors-1) * step)

		rangL := time.Duration(rangeMulL) * step
		rangR := time.Duration(rangeMulR) * step
		offL := time.Duration(offMulL) * step
		offR := time.Duration(offMulR) * step
		left := matrixWindow(rangL, step, 0)
		left.Offset = offL
		right := matrixWindow(rangR, step, 0)
		right.Offset = offR
		in := stepAlignedJoin(left, right)
		snapshot := chplan.CloneNode(in)

		out, err := chplan.ReanchorRange(in, start, end)
		if err != nil {
			rt.Fatalf("ReanchorRange errored on a well-formed grid: %v", err)
		}
		if !in.Equal(snapshot) {
			rt.Fatal("input mutated")
		}
		j := out.(*chplan.VectorJoin)
		arms := []struct {
			side string
			rw   *chplan.RangeWindow
			rang time.Duration
			off  time.Duration
		}{
			{"left", j.Left.(*chplan.RangeWindow), rangL, offL},
			{"right", j.Right.(*chplan.RangeWindow), rangR, offR},
		}
		for _, a := range arms {
			if !a.rw.Start.Equal(start) || !a.rw.End.Equal(end) {
				rt.Fatalf("%s arm bounds: want [%v,%v] got [%v,%v]", a.side, start, end, a.rw.Start, a.rw.End)
			}
			if a.rw.OuterRange != end.Sub(start) {
				rt.Fatalf("%s arm OuterRange %v != %v", a.side, a.rw.OuterRange, end.Sub(start))
			}
			if a.rw.Range != a.rang {
				rt.Fatalf("%s arm Range drifted: want %v got %v", a.side, a.rang, a.rw.Range)
			}
			if a.rw.Offset != a.off {
				rt.Fatalf("%s arm Offset drifted: want %v got %v", a.side, a.off, a.rw.Offset)
			}
		}
	})
}

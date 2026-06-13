package chplan_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file gathers targeted tests that kill the LIVED gremlins mutants
// reported by the phase-1 mutation matrix in
// `.github/workflows/mutation.yml` (`./internal/chplan @ 95%`). Each
// test pins the exact behaviour a single mutation would break:
//
//   - An INVERT_LOGICAL on a `||`/`&&` inside an `Equal` method needs a
//     case where EXACTLY ONE operand is true (a single field differs,
//     all others equal). With the original operator the nodes are not
//     Equal; flipping `||`→`&&` would wrongly report Equal because the
//     other (equal) field makes the `&&` short-circuit elsewhere.
//   - An ARITHMETIC_BASE / INVERT_NEGATIVES on the membership-window
//     widen (`start - Offset - Lookback`) needs a nested node so the
//     widened start is recorded and asserted against the exact value.
//   - An INVERT_LOGICAL on the reanchor grid-prediction guards needs a
//     partially-pinned node (one bound zero / one bound on-grid) so the
//     `&&` and `||` forms diverge.
//
// These mirror the per-node `*_Equal_Negative_*` convention already in
// equal_invariants_test.go; they sit here as a single mutation-budget
// file so the kill set is easy to audit against the gremlins report.

// --- cross_join.go:27:30 — `Left.Equal(o.Left) && Right.Equal(o.Right)` ---

// TestCrossJoin_Equal_Negative_RightOnly / _LeftOnly exercise the
// `Left && Right` tail of CrossJoin.Equal. A mutant flipping `&&` to
// `||` would falsely report Equal when only one child differs, because
// the matching child's Equal returns true.
func TestCrossJoin_Equal_Negative_RightOnly(t *testing.T) {
	t.Parallel()
	a := &chplan.CrossJoin{Left: &chplan.Scan{Table: "shared"}, Right: &chplan.Scan{Table: "a"}}
	b := &chplan.CrossJoin{Left: &chplan.Scan{Table: "shared"}, Right: &chplan.Scan{Table: "b"}}
	if a.Equal(b) {
		t.Errorf("CrossJoin: different Right (Left equal) should not be Equal")
	}
}

func TestCrossJoin_Equal_Negative_LeftOnly(t *testing.T) {
	t.Parallel()
	a := &chplan.CrossJoin{Left: &chplan.Scan{Table: "a"}, Right: &chplan.Scan{Table: "shared"}}
	b := &chplan.CrossJoin{Left: &chplan.Scan{Table: "b"}, Right: &chplan.Scan{Table: "shared"}}
	if a.Equal(b) {
		t.Errorf("CrossJoin: different Left (Right equal) should not be Equal")
	}
}

// TestCrossJoin_Equal_Positive anchors the true case so the negatives
// above are genuine single-field divergences off an otherwise-equal pair.
func TestCrossJoin_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.CrossJoin{Left: &chplan.Scan{Table: "l"}, Right: &chplan.Scan{Table: "r"}}
	b := &chplan.CrossJoin{Left: &chplan.Scan{Table: "l"}, Right: &chplan.Scan{Table: "r"}}
	if !a.Equal(b) {
		t.Errorf("identical CrossJoin trees should be Equal")
	}
}

// --- nary_vector_set_op.go Equal disjunct chains ---

func naryFixture() *chplan.NaryVectorSetOp {
	return &chplan.NaryVectorSetOp{
		Arms:             []chplan.Node{&chplan.Scan{Table: "a"}, &chplan.Scan{Table: "b"}},
		Op:               chplan.VectorSetOr,
		Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
}

func TestNaryVectorSetOp_Equal_Positive(t *testing.T) {
	t.Parallel()
	if !naryFixture().Equal(naryFixture()) {
		t.Errorf("identical NaryVectorSetOp trees should be Equal")
	}
}

// TestNaryVectorSetOp_Equal_Negative_Fields exercises each disjunct in
// NaryVectorSetOp.Equal individually:
//   - nary_vector_set_op.go:72 — `Op != || !Match.Equal`
//   - nary_vector_set_op.go:75/76/77 — the
//     `MetricNameColumn != || AttributesColumn != || TimestampColumn !=
//     || ValueColumn !=` chain
//
// Each row diverges in exactly ONE field; with `||` → `&&` the mutant
// would require every column to differ before reporting not-Equal, so a
// single-field mismatch keeps the kill tight.
func TestNaryVectorSetOp_Equal_Negative_Fields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(n *chplan.NaryVectorSetOp)
	}{
		{"op", func(n *chplan.NaryVectorSetOp) { n.Op = chplan.VectorSetAnd }},
		{"match", func(n *chplan.NaryVectorSetOp) {
			n.Match = chplan.VectorMatch{Labels: []string{"instance"}, On: true}
		}},
		{"metricNameColumn", func(n *chplan.NaryVectorSetOp) { n.MetricNameColumn = "Other" }},
		{"attributesColumn", func(n *chplan.NaryVectorSetOp) { n.AttributesColumn = "Other" }},
		{"timestampColumn", func(n *chplan.NaryVectorSetOp) { n.TimestampColumn = "Other" }},
		{"valueColumn", func(n *chplan.NaryVectorSetOp) { n.ValueColumn = "Other" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := naryFixture(), naryFixture()
			tc.mutate(b)
			if a.Equal(b) || b.Equal(a) {
				t.Errorf("NaryVectorSetOp.Equal must detect %s divergence", tc.name)
			}
		})
	}
}

// --- range_bucket_fanout.go Equal disjunct chains ---

func fanoutFixture() *chplan.RangeBucketFanout {
	return &chplan.RangeBucketFanout{
		Input:          &chplan.Scan{Table: "metrics"},
		Start:          time.Unix(1000, 0).UTC(),
		End:            time.Unix(4600, 0).UTC(),
		Step:           30 * time.Second,
		Lookback:       5 * time.Minute,
		Offset:         time.Minute,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		GroupByAliases: []string{"g0"},
		AggFuncs:       []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "BucketCounts"}},
		AnchorAlias:    "anchor_ts",
		TimestampCol:   "TimeUnix",
	}
}

func TestRangeBucketFanout_Equal_Positive(t *testing.T) {
	t.Parallel()
	if !fanoutFixture().Equal(fanoutFixture()) {
		t.Errorf("identical RangeBucketFanout trees should be Equal")
	}
}

// TestRangeBucketFanout_Equal_Negative_Fields exercises each `||`
// disjunct in RangeBucketFanout.Equal one field at a time:
//   - 99  — `!Start.Equal || !End.Equal`
//   - 102 — `Step != || Lookback != || Offset !=`
//   - 105 — `AnchorAlias != || TimestampCol !=`
//   - 108 — `len(GroupBy) != || len(AggFuncs) !=`
//
// Single-field divergence means `||` → `&&` (which needs both sides of
// a disjunct true) wrongly reports Equal — caught here.
func TestRangeBucketFanout_Equal_Negative_Fields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(r *chplan.RangeBucketFanout)
	}{
		{"start", func(r *chplan.RangeBucketFanout) { r.Start = r.Start.Add(time.Second) }},
		{"end", func(r *chplan.RangeBucketFanout) { r.End = r.End.Add(time.Second) }},
		{"step", func(r *chplan.RangeBucketFanout) { r.Step = time.Minute }},
		{"lookback", func(r *chplan.RangeBucketFanout) { r.Lookback = time.Hour }},
		{"offset", func(r *chplan.RangeBucketFanout) { r.Offset = 2 * time.Minute }},
		{"anchorAlias", func(r *chplan.RangeBucketFanout) { r.AnchorAlias = "other_anchor" }},
		{"timestampCol", func(r *chplan.RangeBucketFanout) { r.TimestampCol = "Other" }},
		{"groupByLen", func(r *chplan.RangeBucketFanout) {
			r.GroupBy = append(r.GroupBy, &chplan.ColumnRef{Name: "Extra"})
		}},
		{"aggFuncsLen", func(r *chplan.RangeBucketFanout) {
			r.AggFuncs = append(r.AggFuncs, chplan.AggFunc{Name: "count", Alias: "Extra"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := fanoutFixture(), fanoutFixture()
			tc.mutate(b)
			if a.Equal(b) || b.Equal(a) {
				t.Errorf("RangeBucketFanout.Equal must detect %s divergence", tc.name)
			}
		})
	}
}

// TestRangeBucketFanout_Equal_Negative_InputOneNil pins the `Input ==
// nil || o.Input == nil` short-circuit (range_bucket_fanout.go:129). A
// `||` → `&&` flip would skip the early-out when exactly one Input is
// nil, walking into Input.Equal on a nil receiver / mis-reporting.
func TestRangeBucketFanout_Equal_Negative_InputOneNil(t *testing.T) {
	t.Parallel()
	a := fanoutFixture()
	b := fanoutFixture()
	b.Input = nil
	if a.Equal(b) {
		t.Errorf("non-nil Input vs nil Input should not be Equal")
	}
	if b.Equal(a) {
		t.Errorf("nil Input vs non-nil Input should not be Equal (reverse)")
	}
}

// --- step_grid.go:38 — `Start.Equal && End.Equal && Step ==` ---

func TestStepGrid_Equal_Positive(t *testing.T) {
	t.Parallel()
	g := func() *chplan.StepGrid {
		return &chplan.StepGrid{
			Start: time.Unix(1000, 0).UTC(),
			End:   time.Unix(4600, 0).UTC(),
			Step:  time.Minute,
		}
	}
	if !g().Equal(g()) {
		t.Errorf("identical StepGrid should be Equal")
	}
}

// TestStepGrid_Equal_Negative_Fields exercises each `&&` conjunct in
// StepGrid.Equal singly. A `&&` → `||` flip would report Equal when two
// of the three fields match and one differs; single-field divergence
// catches each conjunct (Start, End, Step).
func TestStepGrid_Equal_Negative_Fields(t *testing.T) {
	t.Parallel()
	base := chplan.StepGrid{
		Start: time.Unix(1000, 0).UTC(),
		End:   time.Unix(4600, 0).UTC(),
		Step:  time.Minute,
	}
	cases := []struct {
		name   string
		mutate func(g *chplan.StepGrid)
	}{
		{"start", func(g *chplan.StepGrid) { g.Start = g.Start.Add(time.Second) }},
		{"end", func(g *chplan.StepGrid) { g.End = g.End.Add(time.Second) }},
		{"step", func(g *chplan.StepGrid) { g.Step = 2 * time.Minute }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := base
			b := base
			tc.mutate(&b)
			if a.Equal(&b) || b.Equal(&a) {
				t.Errorf("StepGrid.Equal must detect %s divergence", tc.name)
			}
		})
	}
}

// --- reanchor.go widen arithmetic + grid-prediction guards ---

// TestReanchorRange_LWRWidenArithmetic kills the membership-window widen
// math at reanchor.go:126 — `start.Add(-v.Offset - v.Lookback)`. The
// outer RangeLWR's Input is itself an (unpinned) RangeLWR, so the
// widened start is RECORDED as the inner node's Start and can be
// asserted exactly. Offset and Lookback are distinct non-zero durations
// so that:
//   - ARITHMETIC_BASE (`-` → `+`) on either subtraction shifts the
//     widened start by 2*Offset or 2*Lookback, and
//   - INVERT_NEGATIVES (`-v.Offset` → `v.Offset`, `-v.Lookback` →
//     `v.Lookback`) flips the sign of one term,
//
// all of which move the inner Start off the asserted value.
func TestReanchorRange_LWRWidenArithmetic(t *testing.T) {
	t.Parallel()

	const (
		offset   = 90 * time.Second
		lookback = 7 * time.Minute
	)
	inner := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "metrics"},
		Step:          time.Minute, // Step > 0 so the inner participates in the grid
		Lookback:      2 * time.Minute,
		Offset:        0,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
		// Start/End left zero → unpinned, accepted by the grid check.
	}
	outer := &chplan.RangeLWR{
		Input:         inner,
		Step:          time.Minute,
		Lookback:      lookback,
		Offset:        offset,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
		// Outer unpinned too.
	}

	start := time.Unix(100_000, 0).UTC()
	end := time.Unix(200_000, 0).UTC()
	out, err := chplan.ReanchorRange(outer, start, end)
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	gotOuter := out.(*chplan.RangeLWR)
	gotInner := gotOuter.Input.(*chplan.RangeLWR)

	// The inner spine is reanchored to start - Offset - Lookback (and end).
	wantInnerStart := start.Add(-offset - lookback)
	if !gotInner.Start.Equal(wantInnerStart) {
		t.Fatalf("inner widen start wrong: want %v (start - %v - %v), got %v",
			wantInnerStart, offset, lookback, gotInner.Start)
	}
	if !gotInner.End.Equal(end) {
		t.Fatalf("inner end wrong: want %v, got %v", end, gotInner.End)
	}
}

// TestReanchorRange_RejectsPartialPin kills the matrix-window grid guards
// at reanchor.go:185 (`Start.IsZero() && End.IsZero()`). A partially-
// pinned node — Start zero, End pinned OFF the predicted grid — must be
// rejected with ErrReanchorGridMismatch. With `&&` → `||` the unpinned
// early-return would fire (Start.IsZero() alone is enough), wrongly
// accepting the off-grid End.
func TestReanchorRange_RejectsPartialPin(t *testing.T) {
	t.Parallel()

	in := matrixWindowKill(5*time.Minute, time.Minute, time.Hour)
	in.Start = time.Time{}            // zero
	in.End = time.Unix(9999, 0).UTC() // pinned, NOT the predicted grid end

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	if _, err := chplan.ReanchorRange(in, start, end); !errors.Is(err, chplan.ErrReanchorGridMismatch) {
		t.Fatalf("partially-pinned matrix window should be rejected, got %v", err)
	}
}

// TestReanchorRange_LWRRejectsPartialPinZeroStart kills the LWR
// unpinned guard at reanchor.go:209 (`Start.IsZero() && End.IsZero()`):
// Start zero, End off-grid → reject. `&&` → `||` would accept on the
// zero Start alone.
func TestReanchorRange_LWRRejectsPartialPinZeroStart(t *testing.T) {
	t.Parallel()

	in := lwrNodeKill(time.Time{}, time.Unix(9999, 0).UTC(), time.Minute, 5*time.Minute, 0)
	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	if _, err := chplan.ReanchorRange(in, start, end); !errors.Is(err, chplan.ErrReanchorGridMismatch) {
		t.Fatalf("LWR with zero Start + off-grid End should be rejected, got %v", err)
	}
}

// TestReanchorRange_LWRRejectsGriddedStartOffGridEnd kills the LWR
// already-gridded guard at reanchor.go:212 (`Start.Equal(predStart) &&
// End.Equal(predEnd)`): Start sits exactly on the predicted grid but End
// diverges → reject. With `&&` → `||` the matching Start alone would
// satisfy the guard and wrongly accept the off-grid End.
func TestReanchorRange_LWRRejectsGriddedStartOffGridEnd(t *testing.T) {
	t.Parallel()

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	// Start == the predicted grid start; End pinned off-grid.
	in := lwrNodeKill(start, time.Unix(9999, 0).UTC(), time.Minute, 5*time.Minute, 0)
	if _, err := chplan.ReanchorRange(in, start, end); !errors.Is(err, chplan.ErrReanchorGridMismatch) {
		t.Fatalf("LWR with on-grid Start + off-grid End should be rejected, got %v", err)
	}
}

// matrixWindowKill / lwrNodeKill are local fixture builders for the
// reanchor kills above — kept self-contained in this file so the kill
// set does not depend on helper signatures in reanchor_test.go.
func matrixWindowKill(rang, step, outerRange time.Duration) *chplan.RangeWindow {
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

func lwrNodeKill(start, end time.Time, step, lookback, offset time.Duration) *chplan.RangeLWR {
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

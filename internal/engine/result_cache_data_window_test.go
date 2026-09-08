package engine

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestEligibleForResultCache_JudgesTheDataWindowNotTheRequestGrid pins the
// property CLAUDE.md invariant 12 rests the result-cache carve-out on: the
// cache may only be stamped on a query whose every evaluated window ends
// strictly before now - ingestLag, so no later ingest can change the
// answer.
//
// The gate used to read EvalGrid's End, which is the REQUEST grid. A
// selector's `offset` moves the rows a carrier actually reads, and PromQL
// accepts a NEGATIVE offset that shifts evaluation FORWARD: over a request
// grid ending at 11:50, `offset -1h` reads up to 12:50 — fifty minutes into
// the future of the reference now. Every such plan was judged closed and
// cached.
//
// The instant-shape cases are the second half of the same hole: an
// `@`-pinned window rides a Step == 0 carrier beside the request's own
// closed StepGrid, and the gate skipped every Step <= 0 carrier outright,
// so the pin was never examined.
func TestEligibleForResultCache_JudgesTheDataWindowNotTheRequestGrid(t *testing.T) {
	closedStart := fixedNow.Add(-2 * time.Hour)
	closedEnd := fixedNow.Add(-10 * time.Minute) // 11:50, before the 11:55 threshold.

	cases := []struct {
		name string
		plan chplan.Node
		want bool
	}{
		{
			name: "closed_grid_no_offset",
			plan: &chplan.RangeWindow{
				Start: closedStart, End: closedEnd, Step: time.Minute, Range: 5 * time.Minute,
			},
			want: true,
		},
		{
			name: "positive_offset_moves_the_window_further_back",
			plan: &chplan.RangeWindow{
				Start: closedStart, End: closedEnd, Step: time.Minute, Range: 5 * time.Minute,
				Offset: time.Hour,
			},
			want: true,
		},
		{
			name: "negative_offset_pushes_the_data_edge_past_now",
			plan: &chplan.RangeWindow{
				Start: closedStart, End: closedEnd, Step: time.Minute, Range: 5 * time.Minute,
				Offset: -time.Hour,
			},
			want: false,
		},
		{
			name: "negative_offset_just_past_the_threshold",
			// End 11:50 with offset -6m reads to 11:56, one minute past
			// the 11:55 horizon. The boundary, not a comfortable margin.
			plan: &chplan.RangeWindow{
				Start: closedStart, End: closedEnd, Step: time.Minute, Range: 5 * time.Minute,
				Offset: -6 * time.Minute,
			},
			want: false,
		},
		{
			name: "range_lwr_negative_offset",
			plan: &chplan.RangeLWR{
				Start: closedStart, End: closedEnd, Step: time.Minute,
				Offset: -time.Hour,
			},
			want: false,
		},
		{
			name: "absent_over_time_negative_offset",
			plan: &chplan.AbsentOverTime{
				Start: closedStart, End: closedEnd, Step: time.Minute, Range: 5 * time.Minute,
				Offset: -time.Hour,
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eligibleForResultCache(tc.plan, fixedNow, testIngestLag); got != tc.want {
				t.Errorf(
					"eligibleForResultCache = %v, want %v — the carrier's data window ends at %v, threshold is %v",
					got, tc.want,
					tc.plan.(chplan.GridCarrier).DataWindowEnd(),
					fixedNow.Add(-testIngestLag),
				)
			}
		})
	}
}

// TestEligibleForResultCache_InstantShapeCarrierIsJudgedToo pins that a
// Step == 0 carrier beside a closed range grid is examined rather than
// skipped. This is the shape an `@` pin lowers to: the request's own
// StepGrid is closed, and the pinned window hanging off it is not.
func TestEligibleForResultCache_InstantShapeCarrierIsJudgedToo(t *testing.T) {
	closedGrid := &chplan.StepGrid{
		Start: fixedNow.Add(-2 * time.Hour),
		End:   fixedNow.Add(-10 * time.Minute),
		Step:  time.Minute,
	}

	cases := []struct {
		name   string
		pinned *chplan.RangeWindow
		want   bool
	}{
		{
			name: "pin_inside_the_ingest_lag",
			// 11:59 — one minute before now, well inside the horizon.
			pinned: &chplan.RangeWindow{
				End: fixedNow.Add(-time.Minute), Range: 5 * time.Minute,
			},
			want: false,
		},
		{
			name: "pin_in_a_closed_window",
			pinned: &chplan.RangeWindow{
				End: fixedNow.Add(-30 * time.Minute), Range: 5 * time.Minute,
			},
			want: true,
		},
		{
			name: "pin_resolved_at_emit_time",
			// Zero End is the codebase-wide "resolve at emit time"
			// sentinel: the window is not fixed at all.
			pinned: &chplan.RangeWindow{Range: 5 * time.Minute},
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := &chplan.CrossJoin{Left: closedGrid, Right: tc.pinned}
			if got := eligibleForResultCache(plan, fixedNow, testIngestLag); got != tc.want {
				t.Errorf(
					"eligibleForResultCache = %v, want %v — closed StepGrid beside a pinned window ending %v",
					got, tc.want, tc.pinned.DataWindowEnd(),
				)
			}
		})
	}
}

// TestDataWindowEnd_SubtractsTheOffsetForEveryCarrierKind is the
// completeness half: every GridCarrier must report End - Offset, so a kind
// added later cannot inherit a wrong default. The compile-time list in
// grid_carrier.go forces each kind to implement the method; this asserts
// each one implements it correctly.
func TestDataWindowEnd_SubtractsTheOffsetForEveryCarrierKind(t *testing.T) {
	end := fixedNow.Add(-time.Hour)
	off := 30 * time.Minute
	want := end.Add(-off)

	carriers := map[string]chplan.GridCarrier{
		"RangeWindow":              &chplan.RangeWindow{End: end, Offset: off},
		"RangeWindowGridNative":    &chplan.RangeWindowGridNative{End: end, Offset: off},
		"RangeWindowStaleResample": &chplan.RangeWindowStaleResample{End: end, Offset: off},
		"RangeLWR":                 &chplan.RangeLWR{End: end, Offset: off},
		"RangeBucketFanout":        &chplan.RangeBucketFanout{End: end, Offset: off},
		"RangeBucketGridNative":    &chplan.RangeBucketGridNative{End: end, Offset: off},
		"AbsentOverTime":           &chplan.AbsentOverTime{End: end, Offset: off},
	}
	for name, c := range carriers {
		if got := c.DataWindowEnd(); !got.Equal(want) {
			t.Errorf("%s.DataWindowEnd() = %v, want %v (End - Offset)", name, got, want)
		}
	}

	// StepGrid is the request's own anchor grid and carries no offset.
	sg := &chplan.StepGrid{End: end, Step: time.Minute}
	if got := sg.DataWindowEnd(); !got.Equal(end) {
		t.Errorf("StepGrid.DataWindowEnd() = %v, want %v", got, end)
	}

	// The zero-End sentinel survives for every kind.
	for name, c := range map[string]chplan.GridCarrier{
		"RangeWindow":    &chplan.RangeWindow{Offset: off},
		"RangeLWR":       &chplan.RangeLWR{Offset: off},
		"AbsentOverTime": &chplan.AbsentOverTime{Offset: off},
		"StepGrid":       &chplan.StepGrid{Step: time.Minute},
	} {
		if got := c.DataWindowEnd(); !got.IsZero() {
			t.Errorf("%s.DataWindowEnd() with zero End = %v, want the zero time", name, got)
		}
	}
}

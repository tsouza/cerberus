package solver

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/chplan"
)

// sliceWindow builds a pinned matrix RangeWindow over a leaf scan for the
// given grid + offset — the shape the slicer re-anchors.
func sliceWindow(start, end time.Time, step, rang, offset time.Duration) chplan.Node {
	return &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "metrics", Columns: []string{"Value", "TimeUnix", "Attributes"}},
		Func:            "rate",
		Range:           rang,
		Step:            step,
		OuterRange:      end.Sub(start),
		Offset:          offset,
		Start:           start,
		End:             end,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// sliceLWR builds a pinned RangeLWR over the given grid + offset — the
// bare-selector last-with-respect-to shape the phase-3 slicer re-anchors.
func sliceLWR(start, end time.Time, step, lookback, offset time.Duration) chplan.Node {
	return &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "metrics", Columns: []string{"Value", "TimeUnix", "Attributes"}},
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

// originalAnchors enumerates the full anchor grid: End - i*Step, i in [0,N).
func originalAnchors(start, end time.Time, step time.Duration) []time.Time {
	n := int(end.Sub(start)/step) + 1
	out := make([]time.Time, n)
	for i := 0; i < n; i++ {
		out[i] = end.Add(-time.Duration(i) * step)
	}
	return out
}

// sliceAnchors enumerates the anchors a slice owns from its [Start,End] grid.
func sliceAnchors(s Slice, step time.Duration) []time.Time {
	n := int(s.End.Sub(s.Start)/step) + 1
	out := make([]time.Time, n)
	for i := 0; i < n; i++ {
		out[i] = s.End.Add(-time.Duration(i) * step)
	}
	return out
}

func TestSlice_AnchorUnionAndDisjoint(t *testing.T) {
	t.Parallel()
	p := NewPlanner(autoCfg())

	rapid.Check(t, func(t *rapid.T) {
		step := time.Duration(rapid.IntRange(1, 120).Draw(t, "stepSec")) * time.Second
		nAnchors := rapid.IntRange(4, 600).Draw(t, "nAnchors")
		k := rapid.IntRange(2, 8).Draw(t, "k")
		rangeMul := rapid.IntRange(1, 50).Draw(t, "rangeMul")
		offsetSec := rapid.IntRange(-3600, 7200).Draw(t, "offsetSec")

		start := time.Unix(1_700_000_000, 0).UTC()
		end := start.Add(time.Duration(nAnchors-1) * step)
		rang := time.Duration(rangeMul) * step
		offset := time.Duration(offsetSec) * time.Second

		meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}
		plan := sliceWindow(start, end, step, rang, offset)

		slices, err := p.slice(plan, meta, k)
		if err != nil {
			t.Fatalf("slice: %v", err)
		}
		if len(slices) < 2 {
			t.Fatalf("expected >= 2 slices, got %d", len(slices))
		}

		// Union == original, pairwise disjoint.
		seen := map[int64]int{}
		for _, s := range slices {
			// Singleton-tail rule: no slice may carry < 2 anchors.
			cnt := int(s.End.Sub(s.Start)/step) + 1
			if cnt < 2 {
				t.Fatalf("slice %d has count %d < 2 (singleton-tail not merged)", s.Index, cnt)
			}
			// End_j must be on the original grid (End - multiple*Step).
			diff := end.Sub(s.End)
			if diff < 0 || diff%step != 0 {
				t.Fatalf("slice %d End=%v not on original grid (diff=%v step=%v)", s.Index, s.End, diff, step)
			}
			for _, a := range sliceAnchors(s, step) {
				seen[a.UnixNano()]++
			}
		}

		orig := originalAnchors(start, end, step)
		if len(seen) != len(orig) {
			t.Fatalf("union size %d != original %d", len(seen), len(orig))
		}
		for _, a := range orig {
			c, ok := seen[a.UnixNano()]
			if !ok {
				t.Fatalf("original anchor %v missing from slice union", a)
			}
			if c != 1 {
				t.Fatalf("anchor %v appears in %d slices (not disjoint)", a, c)
			}
		}

		// Slices are oldest-first: index 0 starts earliest, last ends at End.
		if !slices[0].Start.Equal(start) {
			t.Fatalf("oldest slice Start=%v, want grid Start=%v", slices[0].Start, start)
		}
		if !slices[len(slices)-1].End.Equal(end) {
			t.Fatalf("newest slice End=%v, want grid End=%v", slices[len(slices)-1].End, end)
		}
	})
}

func TestSlice_ScanFromSignAware(t *testing.T) {
	t.Parallel()
	p := NewPlanner(autoCfg())
	start := time.Unix(1_700_000_000, 0).UTC()
	step := 15 * time.Second
	end := start.Add(time.Hour)
	rang := 5 * time.Minute // D = 5m for a single-window spine

	for _, tc := range []struct {
		name   string
		offset time.Duration
	}{
		{"positive offset", time.Hour},
		{"zero offset", 0},
		{"negative offset", -10 * time.Minute},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}
			plan := sliceWindow(start, end, step, rang, tc.offset)
			slices, err := p.slice(plan, meta, 8)
			if err != nil {
				t.Fatalf("slice: %v", err)
			}
			for _, s := range slices {
				// ScanFrom = Start_j - D - Offset (sign-aware).
				want := s.Start.Add(-rang).Add(-tc.offset)
				if !s.ScanFrom.Equal(want) {
					t.Fatalf("%s: slice %d ScanFrom=%v, want %v", tc.name, s.Index, s.ScanFrom, want)
				}
			}
		})
	}
}

func TestSlice_KClampHonored(t *testing.T) {
	t.Parallel()
	p := NewPlanner(autoCfg())
	start := time.Unix(1_700_000_000, 0).UTC()
	step := time.Minute
	// N = 5 anchors; ask for K = 8 → clamped to <= N, with singleton-tail
	// merge ensuring no count<2 slice.
	end := start.Add(4 * time.Minute)
	meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}
	plan := sliceWindow(start, end, step, time.Minute, 0)
	slices, err := p.slice(plan, meta, 8)
	if err != nil {
		t.Fatalf("slice: %v", err)
	}
	if len(slices) > 5 {
		t.Fatalf("got %d slices, must not exceed N=5", len(slices))
	}
	total := 0
	for _, s := range slices {
		cnt := int(s.End.Sub(s.Start)/step) + 1
		if cnt < 2 {
			t.Fatalf("slice %d count %d < 2", s.Index, cnt)
		}
		total += cnt
	}
	if total != 5 {
		t.Fatalf("anchor total across slices = %d, want N=5", total)
	}
}

// TestSlice_SpineImmutability: re-gridding a returned Slice.Plan's SPINE node
// (the only thing the solver mutates per slice) must not change the input
// plan. The off-spine subtree is SHARED with the input by design (COW) — that
// is asserted separately in TestSlice_OffSpineIsShared — so this test mutates
// only the cloned spine fields, which the COW contract keeps isolated.
func TestSlice_SpineImmutability(t *testing.T) {
	t.Parallel()
	p := NewPlanner(autoCfg())
	start := time.Unix(1_700_000_000, 0).UTC()
	step := 15 * time.Second
	end := start.Add(time.Hour)
	meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}

	plan := sliceWindow(start, end, step, 5*time.Minute, 0)
	snapshot := chplan.CloneNode(plan)

	slices, err := p.slice(plan, meta, 8)
	if err != nil {
		t.Fatalf("slice: %v", err)
	}

	// Re-grid the cloned spine node of the first slice (the per-slice
	// rewrite). This must not move the input plan's grid.
	rw := slices[0].Plan.(*chplan.RangeWindow)
	if rw == plan {
		t.Fatal("Slice.Plan shares the spine pointer; the spine must be cloned")
	}
	rw.Range = 999 * time.Hour
	rw.Start = time.Unix(0, 0).UTC()
	rw.End = time.Unix(1, 0).UTC()
	rw.OuterRange = time.Second
	rw.GroupBy = nil

	if !plan.Equal(snapshot) {
		t.Fatal("re-gridding a returned Slice.Plan's spine node mutated the input plan")
	}
}

// TestSlice_OffSpineIsShared documents the COW lever: every shard's off-spine
// subtree is the SAME pointer as the input's (and the same across shards), so
// slicing does K+1 fewer full-subtree copies. Soundness rests on the
// no-mutate-after-slice contract — see TestSlice_NoSharedMutation.
func TestSlice_OffSpineIsShared(t *testing.T) {
	t.Parallel()
	p := NewPlanner(autoCfg())
	start := time.Unix(1_700_000_000, 0).UTC()
	step := 15 * time.Second
	end := start.Add(time.Hour)
	meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}

	plan := sliceWindow(start, end, step, 5*time.Minute, 0)
	origScan := plan.(*chplan.RangeWindow).Input.(*chplan.Scan)

	slices, err := p.slice(plan, meta, 8)
	if err != nil {
		t.Fatalf("slice: %v", err)
	}

	var firstOffSpine *chplan.Scan
	for i, s := range slices {
		off, ok := s.Plan.(*chplan.RangeWindow).Input.(*chplan.Scan)
		if !ok {
			t.Fatalf("slice %d off-spine input is %T, want *chplan.Scan", i, s.Plan.(*chplan.RangeWindow).Input)
		}
		if off != origScan {
			t.Fatalf("slice %d off-spine Scan was copied; COW requires it be shared with the input", i)
		}
		if i == 0 {
			firstOffSpine = off
		} else if off != firstOffSpine {
			t.Fatalf("slice %d off-spine Scan differs from slice 0; shards must share one off-spine", i)
		}
	}
}

// TestSliceLWR_AnchorUnionAndDisjoint is the phase-3 RangeLWR sibling of
// TestSlice_AnchorUnionAndDisjoint: over random (step, N, K, offset) the
// RangeLWR slice union equals the original anchor set EXACTLY, pairwise
// disjoint, and the slices are oldest-first.
func TestSliceLWR_AnchorUnionAndDisjoint(t *testing.T) {
	t.Parallel()
	p := NewPlanner(autoCfg())

	rapid.Check(t, func(t *rapid.T) {
		step := time.Duration(rapid.IntRange(1, 120).Draw(t, "stepSec")) * time.Second
		nAnchors := rapid.IntRange(4, 600).Draw(t, "nAnchors")
		k := rapid.IntRange(2, 8).Draw(t, "k")
		lookbackMul := rapid.IntRange(1, 50).Draw(t, "lookbackMul")
		offsetSec := rapid.IntRange(-3600, 7200).Draw(t, "offsetSec")

		start := time.Unix(1_700_000_000, 0).UTC()
		end := start.Add(time.Duration(nAnchors-1) * step)
		lookback := time.Duration(lookbackMul) * step
		offset := time.Duration(offsetSec) * time.Second

		meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}
		plan := sliceLWR(start, end, step, lookback, offset)

		slices, err := p.slice(plan, meta, k)
		if err != nil {
			t.Fatalf("slice: %v", err)
		}
		if len(slices) < 2 {
			t.Fatalf("expected >= 2 slices, got %d", len(slices))
		}

		seen := map[int64]int{}
		for _, s := range slices {
			cnt := int(s.End.Sub(s.Start)/step) + 1
			if cnt < 2 {
				t.Fatalf("slice %d has count %d < 2 (singleton-tail not merged)", s.Index, cnt)
			}
			diff := end.Sub(s.End)
			if diff < 0 || diff%step != 0 {
				t.Fatalf("slice %d End=%v not on original grid (diff=%v step=%v)", s.Index, s.End, diff, step)
			}
			// Every re-anchored shard plan must be a RangeLWR with filled bounds
			// exactly matching the slice grid (ReanchorRange re-gridded it).
			r, ok := s.Plan.(*chplan.RangeLWR)
			if !ok {
				t.Fatalf("slice %d plan is %T, want *chplan.RangeLWR", s.Index, s.Plan)
			}
			if !r.Start.Equal(s.Start) || !r.End.Equal(s.End) {
				t.Fatalf("slice %d RangeLWR bounds [%v,%v] != slice grid [%v,%v]",
					s.Index, r.Start, r.End, s.Start, s.End)
			}
			if r.Lookback != lookback || r.Offset != offset || r.Step != step {
				t.Fatalf("slice %d RangeLWR lost a non-grid field: %+v", s.Index, r)
			}
			for _, a := range sliceAnchors(s, step) {
				seen[a.UnixNano()]++
			}
		}

		orig := originalAnchors(start, end, step)
		if len(seen) != len(orig) {
			t.Fatalf("union size %d != original %d", len(seen), len(orig))
		}
		for _, a := range orig {
			c, ok := seen[a.UnixNano()]
			if !ok {
				t.Fatalf("original anchor %v missing from slice union", a)
			}
			if c != 1 {
				t.Fatalf("anchor %v appears in %d slices (not disjoint)", a, c)
			}
		}

		if !slices[0].Start.Equal(start) {
			t.Fatalf("oldest slice Start=%v, want grid Start=%v", slices[0].Start, start)
		}
		if !slices[len(slices)-1].End.Equal(end) {
			t.Fatalf("newest slice End=%v, want grid End=%v", slices[len(slices)-1].End, end)
		}
	})
}

// TestSliceLWR_ScanFromSignAware: the RangeLWR scan floor is
// Start_j - (Offset + Lookback), and the solver-owned ScanFrom is
// Start_j - D - Offset where D = Lookback for a single-LWR spine — both
// offset-sign-aware.
func TestSliceLWR_ScanFromSignAware(t *testing.T) {
	t.Parallel()
	p := NewPlanner(autoCfg())
	start := time.Unix(1_700_000_000, 0).UTC()
	step := 15 * time.Second
	end := start.Add(time.Hour)
	lookback := 5 * time.Minute // D = Lookback for a single-LWR spine

	for _, tc := range []struct {
		name   string
		offset time.Duration
	}{
		{"positive offset", time.Hour},
		{"zero offset", 0},
		{"negative offset", -10 * time.Minute},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}
			plan := sliceLWR(start, end, step, lookback, tc.offset)
			slices, err := p.slice(plan, meta, 8)
			if err != nil {
				t.Fatalf("slice: %v", err)
			}
			for _, s := range slices {
				want := s.Start.Add(-lookback).Add(-tc.offset)
				if !s.ScanFrom.Equal(want) {
					t.Fatalf("%s: slice %d ScanFrom=%v, want %v", tc.name, s.Index, s.ScanFrom, want)
				}
			}
		})
	}
}

// TestSliceLWR_SpineImmutability: re-gridding a returned RangeLWR Slice.Plan's
// SPINE node must not change the input plan — the COW contract through the LWR
// arm. The off-spine Input is SHARED with the input by design (asserted in
// TestSliceLWR_OffSpineIsShared); this test mutates only the cloned spine.
func TestSliceLWR_SpineImmutability(t *testing.T) {
	t.Parallel()
	p := NewPlanner(autoCfg())
	start := time.Unix(1_700_000_000, 0).UTC()
	step := 15 * time.Second
	end := start.Add(time.Hour)
	meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}

	plan := sliceLWR(start, end, step, 5*time.Minute, -5*time.Minute)
	snapshot := chplan.CloneNode(plan)

	slices, err := p.slice(plan, meta, 8)
	if err != nil {
		t.Fatalf("slice: %v", err)
	}

	r := slices[0].Plan.(*chplan.RangeLWR)
	if r == plan {
		t.Fatal("Slice.Plan shares the spine pointer; the spine must be cloned")
	}
	r.Lookback = 999 * time.Hour
	r.Start = time.Unix(0, 0).UTC()
	r.End = time.Unix(1, 0).UTC()
	r.Offset = 123 * time.Hour

	if !plan.Equal(snapshot) {
		t.Fatal("re-gridding a returned RangeLWR Slice.Plan's spine node mutated the input plan")
	}
}

// nestedSubqueryWindowPlan builds a single-spine plan whose OFF-spine subtree
// carries its own matrix RangeWindow that unpinSpine must zero. The spine root
// is a pinned matrix RangeWindow; on the spine sits a TopK whose computed-K
// subtree (KExpr) is a SECOND matrix RangeWindow over a leaf scan. TopK.KExpr
// is a Node child (not the spine Input), so unpinSpine reaches it through the
// off-spine descent — the GUARDRAIL B path: it must DESCEND and clone the path
// to that inner window rather than blanket-share the KExpr subtree (sharing +
// zeroing in place would corrupt the caller's plan).
func nestedSubqueryWindowPlan(start, end time.Time, step, rang time.Duration) chplan.Node {
	innerWindow := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "metrics_k", Columns: []string{"Value", "TimeUnix"}},
		Func:            "count_over_time",
		Range:           2 * rang,
		Step:            step,
		OuterRange:      end.Sub(start),
		Start:           start,
		End:             end,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	topk := &chplan.TopK{
		Input: &chplan.Scan{Table: "metrics", Columns: []string{"Value", "TimeUnix", "Attributes"}},
		KExpr: innerWindow,
		By:    []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		Desc:  true,
	}
	return &chplan.RangeWindow{
		Input:           topk,
		Func:            "rate",
		Range:           rang,
		Step:            step,
		OuterRange:      end.Sub(start),
		Start:           start,
		End:             end,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// TestSlice_NestedSubqueryDescendsAndClones is GUARDRAIL B: an off-spine
// subtree that itself carries a windowed node unpinSpine must zero is handled
// by DESCENDING and cloning the path to that inner window, never by blanket-
// sharing-and-mutating. It asserts:
//
//  1. slicing does NOT mutate the input plan (the inner off-spine window's
//     pinned bounds survive — sharing-then-zeroing would have cleared them);
//  2. every shard's inner off-spine window is UNPINNED (the descend reached
//     and zeroed it, so it isn't left carrying the original full-grid bounds
//     in every shard); and
//  3. the spine is correctly re-gridded per shard.
func TestSlice_NestedSubqueryDescendsAndClones(t *testing.T) {
	t.Parallel()
	p := NewPlanner(autoCfg())
	start := time.Unix(1_700_000_000, 0).UTC()
	step := 15 * time.Second
	end := start.Add(time.Hour)
	meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}

	plan := nestedSubqueryWindowPlan(start, end, step, 5*time.Minute)
	snapshot := chplan.CloneNode(plan)

	slices, err := p.slice(plan, meta, 8)
	if err != nil {
		t.Fatalf("slice: %v", err)
	}

	// (1) The input plan is byte-identical: the off-spine KExpr window's
	// pinned [Start,End] are intact (descend-and-clone never mutated it).
	if !plan.Equal(snapshot) {
		t.Fatal("slicing mutated the input plan — GUARDRAIL B blanket-shared and zeroed an off-spine window in place")
	}

	for i, s := range slices {
		spine := s.Plan.(*chplan.RangeWindow)
		// (3) Spine re-gridded onto this slice.
		if !spine.Start.Equal(s.Start) || !spine.End.Equal(s.End) {
			t.Fatalf("slice %d spine not re-gridded: Start=%v End=%v want [%v,%v]",
				i, spine.Start, spine.End, s.Start, s.End)
		}
		// (2) The inner off-spine KExpr window was descended-into and zeroed:
		// in the unpinned base every windowed node is zeroed; ReanchorRange
		// then re-grids only the SPINE, leaving the off-spine KExpr window at
		// its zeroed (unpinned) bounds in every shard.
		tk, ok := spine.Input.(*chplan.TopK)
		if !ok {
			t.Fatalf("slice %d spine.Input is %T, want *chplan.TopK", i, spine.Input)
		}
		inner, ok := tk.KExpr.(*chplan.RangeWindow)
		if !ok {
			t.Fatalf("slice %d KExpr is %T, want *chplan.RangeWindow", i, tk.KExpr)
		}
		if !inner.Start.IsZero() || !inner.End.IsZero() || inner.OuterRange != 0 {
			t.Fatalf("slice %d inner off-spine window not zeroed by descend: Start=%v End=%v OuterRange=%v",
				i, inner.Start, inner.End, inner.OuterRange)
		}
	}
}

// TestSliceLWR_OffSpineIsShared: every RangeLWR shard shares the input's
// off-spine Input subtree (and shares it across shards) — the COW lever for
// the bare-selector family.
func TestSliceLWR_OffSpineIsShared(t *testing.T) {
	t.Parallel()
	p := NewPlanner(autoCfg())
	start := time.Unix(1_700_000_000, 0).UTC()
	step := 15 * time.Second
	end := start.Add(time.Hour)
	meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}

	plan := sliceLWR(start, end, step, 5*time.Minute, -5*time.Minute)
	origScan := plan.(*chplan.RangeLWR).Input.(*chplan.Scan)

	slices, err := p.slice(plan, meta, 8)
	if err != nil {
		t.Fatalf("slice: %v", err)
	}
	for i, s := range slices {
		if s.Plan.(*chplan.RangeLWR).Input.(*chplan.Scan) != origScan {
			t.Fatalf("slice %d off-spine Scan was copied; COW requires it be shared", i)
		}
	}
}

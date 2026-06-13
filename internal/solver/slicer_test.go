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
	p := &Planner{Cfg: autoCfg()}

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
	p := &Planner{Cfg: autoCfg()}
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
	p := &Planner{Cfg: autoCfg()}
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

// TestSlice_DeepCopyIsolation: mutating a returned Slice.Plan must not change
// the input plan.
func TestSlice_DeepCopyIsolation(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
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

	// Mutate the first slice's plan deeply.
	rw := slices[0].Plan.(*chplan.RangeWindow)
	rw.Range = 999 * time.Hour
	rw.Start = time.Unix(0, 0).UTC()
	rw.GroupBy = nil
	innerScan := rw.Input.(*chplan.Scan)
	innerScan.Table = "MUTATED"

	if !plan.Equal(snapshot) {
		t.Fatal("mutating a returned Slice.Plan mutated the input plan (not a deep copy)")
	}
}

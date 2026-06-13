package solver

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/chplan"
)

// config_sweep_test.go is the config-SPACE property sweep over the Planner.
//
// The table-driven planner_test.go pins the Planner against the DefaultConfig
// thresholds; this file asserts the structural invariants hold across the
// WHOLE valid config space — every combination of Mode / MinFanout /
// MinAnchorPairs / MaxK / MinAnchorsPerSlice the operator could dial in — and
// across random grids + random eligible/ineligible plans. rapid generates the
// config + grid + plan; the invariants below must hold for every draw.
//
// The generators emit ONLY configs that pass Config.Validate (an invalid
// config is a fail-fast misconfiguration the engine adapter rejects before the
// Planner ever sees it, so it is out of scope here).

// drawValidConfig generates a random Config and keeps only those passing
// Validate (the same fail-fast gate the engine adapter applies). Ranges
// straddle the DefaultConfig values in both directions so the monotonicity and
// mode invariants are exercised across the realistic operator-tuning surface.
func drawValidConfig(t *rapid.T) Config {
	c := Config{
		Mode:               rapid.SampledFrom([]string{ModeAuto, ModeSingle, ModeSharded}).Draw(t, "mode"),
		MinFanout:          rapid.IntRange(1, 64).Draw(t, "minFanout"),
		MinAnchorPairs:     rapid.IntRange(1, 20000).Draw(t, "minAnchorPairs"),
		MaxK:               rapid.IntRange(2, 32).Draw(t, "maxK"),
		MinAnchorsPerSlice: rapid.IntRange(2, 64).Draw(t, "minAnchorsPerSlice"),
		Parallel:           rapid.IntRange(1, 8).Draw(t, "parallel"),
		Timeout:            time.Duration(rapid.IntRange(1, 600).Draw(t, "timeoutSec")) * time.Second,
		MaxOutputRows:      int64(rapid.IntRange(1, 10_000_000).Draw(t, "maxOutputRows")),
		MemoryApportion:    rapid.Bool().Draw(t, "memoryApportion"),
	}
	// The ranges above are bounded to the valid space by construction, so every
	// draw must validate. A failure here means a range edit widened past the
	// Validate floor — surface it loudly rather than silently narrowing the
	// generator, so the sweep keeps covering the full valid config space.
	if err := c.Validate(); err != nil {
		t.Fatalf("drawValidConfig produced an invalid config %+v: %v", c, err)
	}
	return c
}

// drawGrid generates a random eval grid (start/end/step) wide enough to carry
// at least a handful of anchors, so a routable plan has room to slice.
func drawGrid(t *rapid.T) (start, end time.Time, step time.Duration) {
	stepSec := rapid.IntRange(1, 120).Draw(t, "stepSec")
	step = time.Duration(stepSec) * time.Second
	nAnchors := rapid.IntRange(2, 800).Draw(t, "nAnchors")
	start = time.Unix(1_700_000_000, 0).UTC()
	end = start.Add(time.Duration(nAnchors-1) * step)
	return start, end, step
}

// drawEligibleWindow builds a pinned matrix RangeWindow on the given grid — the
// routable *RangeWindow matrix family. Range is a random Step-multiple so the
// fan-out F varies across the config space.
func drawEligibleWindow(t *rapid.T, start, end time.Time, step time.Duration) chplan.Node {
	rangeMul := rapid.IntRange(1, 60).Draw(t, "rangeMul")
	offsetSec := rapid.IntRange(-3600, 7200).Draw(t, "offsetSec")
	return sliceWindow(start, end, step,
		time.Duration(rangeMul)*step, time.Duration(offsetSec)*time.Second)
}

// TestSweep_PlanNeverPanics: Plan() is total over the valid config space — no
// draw of {config, grid, plan} may panic. A panic in classification would take
// down the request path, so totality is the floor invariant.
func TestSweep_PlanNeverPanics(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		cfg := drawValidConfig(t)
		start, end, step := drawGrid(t)
		meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}

		// Cover both an eligible matrix window and a grab-bag of ineligible
		// shapes so the panic-freedom claim spans the whole classifier.
		plans := []chplan.Node{
			drawEligibleWindow(t, start, end, step),
			instantWindow(start, end),                                               // Step==0 → instant
			&chplan.Limit{Input: drawEligibleWindow(t, start, end, step), Count: 5}, // unmarked node
			now64Window(start, end, step),                                           // now64 in a projection
		}
		p := &Planner{Cfg: cfg}
		for _, plan := range plans {
			// The contract is "does not panic"; the decision itself is asserted
			// by the targeted invariants below.
			_, _ = p.Plan(plan, meta)
		}
	})
}

// TestSweep_SingleNeverRoutes: Mode=single classifies but NEVER routes,
// regardless of every other knob or the plan/grid. This is the ship-dark
// guarantee — flipping any threshold must not leak a route while the mode is
// single.
func TestSweep_SingleNeverRoutes(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		cfg := drawValidConfig(t)
		cfg.Mode = ModeSingle
		start, end, step := drawGrid(t)
		meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}
		plan := drawEligibleWindow(t, start, end, step) // the most routable shape

		p := &Planner{Cfg: cfg}
		_, routed := p.Plan(plan, meta)
		if routed {
			t.Fatalf("Mode=single routed (cfg=%+v) — ship-dark guarantee broken", cfg)
		}
	})
}

// TestSweep_RoutedDecisionIsWellFormed: any routed Decision, under any valid
// config, satisfies the slicer contract — K >= 2, the slice anchor-union equals
// the original grid exactly, and the slices are pairwise disjoint. This reuses
// the slicer invariant checks but drives them through the full Plan() path so
// the K the Planner clamps to is the K that gets validated.
func TestSweep_RoutedDecisionIsWellFormed(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		cfg := drawValidConfig(t)
		start, end, step := drawGrid(t)
		meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}
		plan := drawEligibleWindow(t, start, end, step)

		p := &Planner{Cfg: cfg}
		d, routed := p.Plan(plan, meta)
		if !routed {
			return // ineligible-for-threshold under this config; nothing to check.
		}

		if d.Reason != ReasonRouted {
			t.Fatalf("routed Decision has reason %q, want %q", d.Reason, ReasonRouted)
		}
		if d.K < 2 {
			t.Fatalf("routed Decision has K=%d, want >= 2 (cfg=%+v)", d.K, cfg)
		}
		if len(d.Slices) != d.K {
			t.Fatalf("Decision.K=%d but len(Slices)=%d", d.K, len(d.Slices))
		}

		// Anchor-union == original grid, pairwise disjoint (the slicer
		// geometry invariant, asserted on the produced Decision).
		seen := map[int64]int{}
		for _, s := range d.Slices {
			cnt := int(s.End.Sub(s.Start)/step) + 1
			if cnt < 2 {
				t.Fatalf("routed slice %d has count %d < 2 (singleton-tail not merged)", s.Index, cnt)
			}
			for _, a := range sliceAnchors(s, step) {
				seen[a.UnixNano()]++
			}
		}
		orig := originalAnchors(start, end, step)
		if len(seen) != len(orig) {
			t.Fatalf("slice anchor-union size %d != original grid %d (cfg=%+v)", len(seen), len(orig), cfg)
		}
		for _, a := range orig {
			c, ok := seen[a.UnixNano()]
			if !ok {
				t.Fatalf("original anchor %v missing from routed slice union", a)
			}
			if c != 1 {
				t.Fatalf("anchor %v appears in %d slices (not pairwise disjoint)", a, c)
			}
		}
		// Oldest-first composition order: slice 0 starts at the grid Start, the
		// last slice ends at the grid End.
		if !d.Slices[0].Start.Equal(start.UTC()) {
			t.Fatalf("oldest slice Start=%v, want grid Start=%v", d.Slices[0].Start, start.UTC())
		}
		if !d.Slices[len(d.Slices)-1].End.Equal(end.UTC()) {
			t.Fatalf("newest slice End=%v, want grid End=%v", d.Slices[len(d.Slices)-1].End, end.UTC())
		}
	})
}

// TestSweep_IneligibleAlwaysRoutesA: a structurally-ineligible plan (instant /
// now64 / unpinned / unmarked node / non-RangeWindow spine) routes A for EVERY
// valid config — fail-toward-A is config-INDEPENDENT. No threshold knob can
// turn an ineligible plan into a route; even Mode=sharded (thresholds at the
// floor) keeps it on route A.
func TestSweep_IneligibleAlwaysRoutesA(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		cfg := drawValidConfig(t)
		start, end, step := drawGrid(t)
		instantMeta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}

		// Each ineligible shape, paired with the meta it is ineligible under.
		type tc struct {
			name string
			plan chplan.Node
			meta RequestMeta
		}
		cases := []tc{
			// Step==0 request → instant, no anchor grid.
			{
				"instant-meta", drawEligibleWindow(t, start, end, step),
				RequestMeta{Lang: "promql", Start: start, End: end, Step: 0},
			},
			// now64 in a projection → two shards see different wall-clocks.
			{"now64", now64Window(start, end, step), instantMeta},
			// An unmarked (not slice-invariant) node anywhere in the tree.
			{"unmarked-node", &chplan.Limit{Input: drawEligibleWindow(t, start, end, step), Count: 5}, instantMeta},
			// A non-RangeWindow spine bound-carrier (StepGrid with its own grid).
			{"non-rangewindow-spine", stepGridSpine(start, end, step), instantMeta},
			// An instant-shape window (OuterRange==0) at the outermost spine.
			{"instant-window", instantWindow(start, end), instantMeta},
		}

		p := &Planner{Cfg: cfg}
		for _, c := range cases {
			_, routed := p.Plan(c.plan, c.meta)
			if routed {
				t.Fatalf("ineligible plan %q routed under cfg=%+v — fail-toward-A must be config-independent",
					c.name, cfg)
			}
		}
	})
}

// TestSweep_ThresholdMonotonicity: raising MinFanout or MinAnchorPairs can only
// SHRINK (or keep) the routed set — a higher bar never makes more queries
// route. The probe holds {grid, plan, all other knobs} fixed and asserts that
// if the LOW-threshold config does not route, the HIGH-threshold config (same
// shape, stricter bars) does not route either. This is a cheap, strong
// correctness check on the Mode=auto cost gate.
func TestSweep_ThresholdMonotonicity(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		base := drawValidConfig(t)
		base.Mode = ModeAuto // monotonicity is a property of the auto cost gate.

		start, end, step := drawGrid(t)
		meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}
		plan := drawEligibleWindow(t, start, end, step)

		// A strictly-higher (or equal) bar on both cost thresholds.
		bumpFanout := rapid.IntRange(0, 48).Draw(t, "bumpFanout")
		bumpPairs := rapid.IntRange(0, 16000).Draw(t, "bumpPairs")
		strict := base
		strict.MinFanout = base.MinFanout + bumpFanout
		strict.MinAnchorPairs = base.MinAnchorPairs + bumpPairs

		lowP := &Planner{Cfg: base}
		hiP := &Planner{Cfg: strict}

		_, lowRouted := lowP.Plan(plan, meta)
		_, hiRouted := hiP.Plan(plan, meta)

		// Monotone: a stricter bar may flip a route OFF, never ON. So if the
		// looser config did NOT route, the stricter one must not route either.
		if !lowRouted && hiRouted {
			t.Fatalf("raising thresholds turned a non-route into a route "+
				"(base Fmin=%d pairs=%d → strict Fmin=%d pairs=%d) — cost gate is not monotone",
				base.MinFanout, base.MinAnchorPairs, strict.MinFanout, strict.MinAnchorPairs)
		}
	})
}

// --- ineligible-plan builders (config-independent route-A shapes) ---

// instantWindow builds an instant-shape RangeWindow (Step==0, OuterRange==0):
// no anchor grid to slice. Pinned bounds so only the instant-shape signal,
// not an unpinned-bound signal, drives the route-A.
func instantWindow(start, end time.Time) chplan.Node {
	return &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            0,
		OuterRange:      0,
		Start:           start,
		End:             end,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
}

// now64Window builds an eligible-looking matrix window whose Project carries a
// now64() call — the wall-clock-divergence signal that forces route A.
func now64Window(start, end time.Time, step time.Duration) chplan.Node {
	rw := sliceWindow(start, end, step, 5*time.Minute, 0)
	return &chplan.Project{
		Input: rw,
		Projections: []chplan.Projection{
			{Alias: "v", Expr: &chplan.FuncCall{Name: "now64", Args: nil}},
		},
	}
}

// stepGridSpine builds a plan whose grid-carrying spine node is a StepGrid (its
// own Start/End/Step that ReanchorRange clones verbatim) — a non-RangeWindow
// spine that fails closed to route A in phase 1.
func stepGridSpine(start, end time.Time, step time.Duration) chplan.Node {
	return &chplan.StepGrid{
		Start: start,
		End:   end,
		Step:  step,
	}
}

package solver

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestDecision_CostGrid_PopulatedOnRoute pins that a true route-B decision
// carries the RAW classifier cost scalars (N/F/D/OuterRange/Step) the
// calibration corpus joins to observed cost — not just Strategy/K/Reason.
func TestDecision_CostGrid_PopulatedOnRoute(t *testing.T) {
	t.Parallel()

	p := NewPlanner(autoCfg())
	d, routed := p.Plan(oomWindow(), oomMeta())
	if !routed {
		t.Fatalf("OOM shape should route under auto; reason=%q", d.Reason)
	}

	// The OOM shape is rate(m[5m]) @ 15s over 1h: F = 5m/15s = 20,
	// N = 1h/15s + 1 = 241, D = 5m, OuterRange = 1h, Step = 15s.
	if d.NAnchors != 241 {
		t.Errorf("NAnchors = %d, want 241", d.NAnchors)
	}
	if d.Fanout != 20 {
		t.Errorf("Fanout = %d, want 20", d.Fanout)
	}
	if d.CumulativeD != 5*time.Minute {
		t.Errorf("CumulativeD = %s, want 5m", d.CumulativeD)
	}
	if d.OuterRange != time.Hour {
		t.Errorf("OuterRange = %s, want 1h", d.OuterRange)
	}
	if d.Step != gridStep {
		t.Errorf("Step = %s, want %s", d.Step, gridStep)
	}
}

// TestDecision_CostGrid_PopulatedOnNotRouted pins the dossier correction: a
// route-A (not-routed) decision MUST still record its N/F/D, because the
// overlap analysis compares route-A and route-B cost distributions at equal
// (N, F, D). A modest below-threshold shape under auto stays on route A but
// must carry the grid.
func TestDecision_CostGrid_PopulatedOnNotRouted(t *testing.T) {
	t.Parallel()

	// rate(m[30s]) @ 15s over 1h: F = 30s/15s = 2 < MinFanout, so auto keeps
	// it on route A — but N/F/D must still be recorded.
	rw := &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "rate",
		Range:           30 * time.Second,
		Step:            gridStep,
		OuterRange:      time.Hour,
		Start:           gridStart,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	plan := &chplan.Aggregate{
		Input:    rw,
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
	}

	p := NewPlanner(autoCfg())
	d, routed := p.Plan(plan, oomMeta())
	if routed {
		t.Fatalf("F=2 shape should NOT route under auto")
	}
	if d.Reason != ReasonBelowThreshold {
		t.Fatalf("reason = %q, want below-threshold", d.Reason)
	}
	// Strategy stays empty (route A) but the grid is recorded.
	if d.Strategy != "" {
		t.Errorf("Strategy = %q, want empty on route A", d.Strategy)
	}
	if d.NAnchors != 241 {
		t.Errorf("NAnchors = %d, want 241 even on route A", d.NAnchors)
	}
	if d.Fanout != 2 {
		t.Errorf("Fanout = %d, want 2", d.Fanout)
	}
	if d.CumulativeD != 30*time.Second {
		t.Errorf("CumulativeD = %s, want 30s", d.CumulativeD)
	}
	if d.OuterRange != time.Hour || d.Step != gridStep {
		t.Errorf("OuterRange/Step = %s/%s, want 1h/%s", d.OuterRange, d.Step, gridStep)
	}
}

// TestDecision_CostGrid_HighDNotFoldedIntoReason pins the bucketing rule: the
// high-D class records ReasonHighD distinctly AND carries its raw D, so the
// corpus never has to infer the class from the (folded) shadow-header reason.
func TestDecision_CostGrid_HighDNotFoldedIntoReason(t *testing.T) {
	t.Parallel()

	// A single long-lookback window whose D forces the K clamp ceiling below 2:
	// OuterRange 1h, Range 40m → floor(1h / 40m) = 1 < 2 → ReasonHighD.
	rw := &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "rate",
		Range:           40 * time.Minute,
		Step:            gridStep,
		OuterRange:      time.Hour,
		Start:           gridStart,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	plan := &chplan.Aggregate{
		Input:    rw,
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
	}

	p := NewPlanner(autoCfg())
	d, routed := p.Plan(plan, oomMeta())
	if routed {
		t.Fatalf("high-D shape should NOT route")
	}
	if d.Reason != ReasonHighD {
		t.Fatalf("reason = %q, want high-D (distinct, not folded)", d.Reason)
	}
	if d.CumulativeD != 40*time.Minute {
		t.Errorf("CumulativeD = %s, want 40m (raw D available regardless of reason)", d.CumulativeD)
	}
}

// TestPlan_OfflineReplay_ReproducesRoute is the counterfactual-replay
// demonstration: feed the recorded (N, F, D, OuterRange, Step) features back
// through Planner.Plan by reconstructing a minimal plan from them and assert it
// reproduces the recorded route. This proves the captured features suffice for
// offline threshold testing — the operator can replay the pure classifier from
// the corpus alone.
func TestPlan_OfflineReplay_ReproducesRoute(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		fanRn time.Duration // Range (so F = Range/Step)
		mode  string
	}{
		{name: "oom-shape routes (auto)", fanRn: 5 * time.Minute, mode: ModeAuto},
		{name: "low-fanout route A (auto)", fanRn: 30 * time.Second, mode: ModeAuto},
		{name: "eligible routes (sharded)", fanRn: time.Minute, mode: ModeSharded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.Mode = tc.mode
			p := NewPlanner(cfg)

			// Original classification, capturing the recorded features.
			plan := buildReplayPlan(tc.fanRn)
			d0, routed0 := p.Plan(plan, oomMeta())

			// Reconstruct a plan from ONLY the recorded scalar features and
			// replay it. Step/OuterRange/Range are recovered from the recorded
			// grid: Range = Fanout*Step, OuterRange and Step verbatim.
			step := d0.Step
			rebuilt := replayPlanFromFeatures(d0.NAnchors, d0.Fanout, d0.OuterRange, step)
			meta := RequestMeta{Lang: "promql", Start: gridStart, End: gridStart.Add(d0.OuterRange), Step: step}

			d1, routed1 := p.Plan(rebuilt, meta)

			if routed1 != routed0 {
				t.Fatalf("replay routed=%v != original routed=%v", routed1, routed0)
			}
			if d1.Reason != d0.Reason {
				t.Errorf("replay reason=%q != original reason=%q", d1.Reason, d0.Reason)
			}
			if d1.K != d0.K {
				t.Errorf("replay K=%d != original K=%d", d1.K, d0.K)
			}
			if d1.NAnchors != d0.NAnchors || d1.Fanout != d0.Fanout {
				t.Errorf("replay (N,F)=(%d,%d) != original (%d,%d)", d1.NAnchors, d1.Fanout, d0.NAnchors, d0.Fanout)
			}
		})
	}
}

// buildReplayPlan builds a sum(rate(m[range])) @ 15s over 1h plan for a given
// inner Range — the shape the original classification ran over.
func buildReplayPlan(rng time.Duration) chplan.Node {
	rw := &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "rate",
		Range:           rng,
		Step:            gridStep,
		OuterRange:      time.Hour,
		Start:           gridStart,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	return &chplan.Aggregate{
		Input:    rw,
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
	}
}

// replayPlanFromFeatures reconstructs the minimal plan the classifier needs
// from ONLY the recorded scalar features: Range is recovered as Fanout*Step,
// the grid bounds from OuterRange/Step. This is exactly the information a
// corpus row carries, demonstrating offline replay needs nothing more.
func replayPlanFromFeatures(_ int, fanout int64, outerRange, step time.Duration) chplan.Node {
	start := gridStart
	end := start.Add(outerRange)
	rw := &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "rate",
		Range:           time.Duration(fanout) * step,
		Step:            step,
		OuterRange:      outerRange,
		Start:           start,
		End:             end,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	return &chplan.Aggregate{
		Input:    rw,
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
	}
}

package solver

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// belowAutoThresholdWindow builds a shape Plan rejects under ModeAuto's
// predictive cost gate but that IS structurally sliceable: Range=1m gives
// F=4, below the default MinFanout=16, so Plan(..., ModeAuto) reports
// below-threshold even though every correctness gate passes.
func belowAutoThresholdWindow() chplan.Node {
	rw := &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "rate",
		Range:           time.Minute,
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
		GroupBy:  nil,
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
	}
}

// TestEligible_BypassesAutoThresholdsNotStructuralGates is the core
// behavioural contract Eligible adds: a plan Plan() rejects under ModeAuto
// PURELY because of the predictive fanout/anchor-pair threshold routes via
// Eligible regardless of Cfg.Mode, because Eligible answers a different
// question ("can this be sliced and answered identically") than Plan's
// ModeAuto branch ("is this worth slicing, predicted from cost alone").
func TestEligible_BypassesAutoThresholdsNotStructuralGates(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	plan := belowAutoThresholdWindow()
	meta := oomMeta()

	// Plan() under ModeAuto rejects it — confirms the fixture actually
	// exercises the threshold gate, not some other rejection reason.
	planDecision, planRouted := p.Plan(plan, meta)
	if planRouted {
		t.Fatalf("fixture must be below-threshold under ModeAuto; got routed=true (reason=%q) — fixture no longer exercises the intended gate", planDecision.Reason)
	}
	if planDecision.Reason != ReasonBelowThreshold {
		t.Fatalf("Plan() reason = %q, want %q — fixture rejected for the wrong reason", planDecision.Reason, ReasonBelowThreshold)
	}

	// Eligible(), same Planner (Cfg.Mode still ModeAuto), same plan+meta —
	// routes, because it never reads MinFanout/MinAnchorPairs at all.
	eligDecision, eligRouted := p.Eligible(plan, meta)
	if !eligRouted {
		t.Fatalf("Eligible() must route a below-threshold-but-structurally-eligible plan; got reason=%q", eligDecision.Reason)
	}
	if eligDecision.Reason != ReasonRouted {
		t.Fatalf("Eligible() reason = %q, want %q", eligDecision.Reason, ReasonRouted)
	}
	if eligDecision.K < 2 {
		t.Fatalf("Eligible() K = %d, want >= 2", eligDecision.K)
	}
	if len(eligDecision.Slices) != eligDecision.K {
		t.Fatalf("len(Slices) = %d, want K=%d", len(eligDecision.Slices), eligDecision.K)
	}
}

// TestEligible_StructuralGatesStillApply proves Eligible does not bypass
// correctness — reusing the rejection-table's now64 fixture, which is
// unrelated to any cost threshold and must fail closed regardless of which
// entry point is called.
func TestEligible_StructuralGatesStillApply(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	rw := oomWindow().(*chplan.Aggregate)
	// A now64() predicate anywhere in the plan fails closed — two shard
	// statements resolving it independently would disagree on wall-clock.
	plan := &chplan.Filter{
		Input: rw,
		Predicate: &chplan.Binary{
			Op:    chplan.OpLt,
			Left:  &chplan.ColumnRef{Name: "TimeUnix"},
			Right: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}},
		},
	}
	d, routed := p.Eligible(plan, oomMeta())
	if routed {
		t.Fatal("Eligible() must not route a plan containing now64 — a correctness gate, not a cost gate")
	}
	if d.Reason != ReasonNow64 {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonNow64)
	}
}

// TestEligible_RespectsModeSingle: an operator who has explicitly disabled
// the solver (Mode=single) has said route B never runs — Eligible, itself a
// form of route-B usage, honours that rather than routing around it.
func TestEligible_RespectsModeSingle(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig() // Mode == single
	p := &Planner{Cfg: cfg}
	d, routed := p.Eligible(oomWindow(), oomMeta())
	if routed {
		t.Fatal("Eligible() must never route under Mode=single")
	}
	if d == nil {
		t.Fatal("decision must be non-nil even when not routed")
	}
}

// TestEligible_MatchesShardedPlanForEligiblePlan: for a plan that IS
// eligible, Eligible's floor semantics (no threshold gate) must agree with
// what Plan() under ModeSharded (which also drops thresholds to the floor)
// would produce — the two are meant to compute the identical K/Slices via
// the same classify() + sliceAndDecide() path.
func TestEligible_MatchesShardedPlanForEligiblePlan(t *testing.T) {
	t.Parallel()
	plan := oomWindow()
	meta := oomMeta()

	autoP := &Planner{Cfg: autoCfg()}
	eligDecision, eligRouted := autoP.Eligible(plan, meta)

	shardedCfg := DefaultConfig()
	shardedCfg.Mode = ModeSharded
	shardedP := &Planner{Cfg: shardedCfg}
	shardedDecision, shardedRouted := shardedP.Plan(plan, meta)

	if eligRouted != shardedRouted {
		t.Fatalf("routed mismatch: Eligible()=%v, sharded Plan()=%v", eligRouted, shardedRouted)
	}
	if eligDecision.K != shardedDecision.K {
		t.Fatalf("K mismatch: Eligible()=%d, sharded Plan()=%d", eligDecision.K, shardedDecision.K)
	}
	if len(eligDecision.Slices) != len(shardedDecision.Slices) {
		t.Fatalf("slice count mismatch: Eligible()=%d, sharded Plan()=%d", len(eligDecision.Slices), len(shardedDecision.Slices))
	}
}

// TestEligible_InstantQueryNeverRoutes: Step<=0 (instant) fails closed
// through classify's very first gate, same as Plan.
func TestEligible_InstantQueryNeverRoutes(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	instantMeta := RequestMeta{Lang: "promql", Start: gridStart, End: gridEnd, Step: 0}
	d, routed := p.Eligible(oomWindow(), instantMeta)
	if routed {
		t.Fatal("Eligible() must never route an instant query")
	}
	if d.Reason != ReasonInstant {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonInstant)
	}
}

package solver

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// joinPlan builds a vector-vector ratio join over two `sum(rate(m[5m]))` arms
// (each the oomWindow shape — an Aggregate over a pinned matrix RangeWindow on
// the canonical 1h/15s grid). stepAligned toggles the range-vs-instant mode
// flag; every other field is held fixed so the ONLY thing distinguishing the
// routable case from the fail-closed case is StepAligned.
func joinPlan(stepAligned bool) *chplan.VectorJoin {
	return &chplan.VectorJoin{
		Left:             oomWindow(),
		Right:            oomWindow(),
		Op:               chplan.OpDiv,
		Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
		StepAligned:      stepAligned,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
}

// TestPlan_StepAlignedVectorJoinRoutes: a StepAligned vector-vector join whose
// arms are both slice-invariant routes B under Mode=sharded. This is the
// positive half of the discrimination proof — the same plan shape with
// StepAligned=false must NOT route (see below), so a passing pair proves the
// guard keys on StepAligned, not on the node kind alone.
func TestPlan_StepAlignedVectorJoinRoutes(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Mode = ModeSharded // floor thresholds: eligibility, not cost, decides.
	p := &Planner{Cfg: cfg}

	d, routed := p.Plan(joinPlan(true), oomMeta())
	if !routed {
		t.Fatalf("StepAligned vector-vector join must route B; got reason=%q", d.Reason)
	}
	if d.Reason != ReasonRouted {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonRouted)
	}
	if d.K < 2 {
		t.Fatalf("routed join must produce K >= 2 slices, got K=%d", d.K)
	}
}

// TestPlan_InstantVectorJoinRoutesA: the negative half — a StepAligned=false
// (instant-mode) vector-vector join must fail closed to route A with
// ReasonInstantJoin, even though VectorJoin is registered slice-invariant. Its
// emitter synthesizes the join-side timestamp with now64(9), a wall-clock that
// diverges across shards; slicing it would silently concatenate wrong results.
func TestPlan_InstantVectorJoinRoutesA(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Mode = ModeSharded // even with thresholds floored, it must NOT route.
	p := &Planner{Cfg: cfg}

	d, routed := p.Plan(joinPlan(false), oomMeta())
	if routed {
		t.Fatalf("instant-mode (!StepAligned) vector-vector join must NOT route (K=%d)", d.K)
	}
	if d.Reason != ReasonInstantJoin {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonInstantJoin)
	}
}

// instantScalarInteriorJoinPlan builds the leak shape the registry opened: a
// routable outer spine (the oomWindow sum(rate) grid) carrying a ScalarSubquery
// whose interior is an INSTANT (!StepAligned) vector-vector join over two
// Aggregate-rooted instant arms (sum(up_a) / sum(up_b) — NO windowed node
// inside, so checkScalarHeavy never fires, and the now64(9) join-side timestamp
// is minted in SQL, so the now64 scan never fires either). Only the
// sawInstantVectorJoin fail-close in walkScalarInterior stands between this plan
// and a route-B decision that would replicate a per-shard wall-clock scalar
// across time-slices.
func instantScalarInteriorJoinPlan() chplan.Node {
	instantArm := func() chplan.Node {
		return &chplan.Aggregate{
			Input:    leafScan(),
			AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
		}
	}
	join := &chplan.VectorJoin{
		Left:             instantArm(),
		Right:            instantArm(),
		Op:               chplan.OpDiv,
		Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
		StepAligned:      false, // instant-mode: emitter synthesizes now64(9)
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
	rw := &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            gridStep,
		OuterRange:      time.Hour,
		Start:           gridStart,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		ScalarExprs:     []chplan.Expr{&chplan.ScalarSubquery{Input: join}},
	}
	return &chplan.Aggregate{
		Input:    rw,
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
	}
}

// TestPlan_InstantVectorJoinInScalarInteriorRoutesA: an instant vector-vector
// join buried in a ScalarSubquery interior must fail closed to route A with
// ReasonInstantJoin, exactly like a top-level instant join. The main-path
// walkNode never recurses into ScalarSubquery.Input — walkScalarInterior does —
// so without a VectorJoin case there, this plan slips every gate (it is
// slice-invariant, now64-free at the chplan level, and not scalar-heavy) and
// routes B, silently concatenating a per-shard now64(9) scalar across slices.
// This is the regression guard for that leak.
func TestPlan_InstantVectorJoinInScalarInteriorRoutesA(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Mode = ModeSharded // floor thresholds: only the fail-close can stop it.
	p := &Planner{Cfg: cfg}

	d, routed := p.Plan(instantScalarInteriorJoinPlan(), oomMeta())
	if routed {
		t.Fatalf("instant join in a scalar interior must NOT route (K=%d)", d.K)
	}
	if d.Reason != ReasonInstantJoin {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonInstantJoin)
	}
}

// TestPlan_VectorJoinAtPinnedArmRoutesA: a StepAligned join with one @-pinned
// arm (its End diverges from the request grid) must route A. The grid-
// prediction guard fires on the divergent arm exactly as it does on a
// single-spine @-pinned window, so an @ inside a join arm cannot slip a
// wrong-grid shard plan past the solver.
func TestPlan_VectorJoinAtPinnedArmRoutesA(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Mode = ModeSharded
	p := &Planner{Cfg: cfg}

	j := joinPlan(true)
	// Pin the right arm's outermost RangeWindow to an End off the request grid.
	pinnedArm := j.Right.(*chplan.Aggregate).Input.(*chplan.RangeWindow)
	pinnedArm.End = gridEnd.Add(time.Hour) // != predicted grid end

	d, routed := p.Plan(j, oomMeta())
	if routed {
		t.Fatalf("join with an @-pinned arm must NOT route (K=%d)", d.K)
	}
	if d.Reason != ReasonGridMismatch {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonGridMismatch)
	}
}

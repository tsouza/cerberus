package solver

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// gridStart/gridEnd are the canonical 1h request window used across the
// eligibility table. End - Start = 1h; with Step = 15s that is N = 241 anchors.
var (
	gridStart = time.Unix(1_700_000_000, 0).UTC()
	gridEnd   = gridStart.Add(time.Hour)
	gridStep  = 15 * time.Second
)

func oomMeta() RequestMeta {
	return RequestMeta{Lang: "promql", Start: gridStart, End: gridEnd, Step: gridStep}
}

// leafScan is a plain slice-invariant Scan.
func leafScan() chplan.Node {
	return &chplan.Scan{Table: "metrics", Columns: []string{"Value", "TimeUnix", "Attributes"}}
}

// oomWindow builds the motivating shape: sum(rate(m[5m])) @ 15s over 1h.
// The outermost spine node is a pinned matrix RangeWindow (Range=5m, Step=15s,
// OuterRange=1h, Start/End on the predicted grid) under an Aggregate (the
// sum), which is slice-invariant per the registry.
func oomWindow() chplan.Node {
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
	}
	return &chplan.Aggregate{
		Input:    rw,
		GroupBy:  nil,
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
	}
}

func autoCfg() Config {
	c := DefaultConfig()
	c.Mode = ModeAuto
	return c
}

// TestPlan_OOMShapeRoutes pins the worked example from the doc: the OOM shape
// routes under Mode=auto with K=8 and Reason=routed.
func TestPlan_OOMShapeRoutes(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	d, routed := p.Plan(oomWindow(), oomMeta())
	if !routed {
		t.Fatalf("OOM shape must route; got reason=%q", d.Reason)
	}
	if d.Reason != ReasonRouted {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonRouted)
	}
	if d.K != 8 {
		t.Fatalf("K = %d, want 8", d.K)
	}
	if d.Strategy != StrategyShardedTimeslice {
		t.Fatalf("strategy = %q, want %q", d.Strategy, StrategyShardedTimeslice)
	}
	if len(d.Slices) != 8 {
		t.Fatalf("len(Slices) = %d, want 8", len(d.Slices))
	}
}

// TestPlan_SingleNeverRoutes: Mode=="single" classifies but never routes,
// even for the eligible OOM shape.
func TestPlan_SingleNeverRoutes(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig() // Mode == single
	p := &Planner{Cfg: cfg}
	d, routed := p.Plan(oomWindow(), oomMeta())
	if routed {
		t.Fatal("Mode=single must never route")
	}
	if d == nil {
		t.Fatal("decision must be non-nil even when not routed")
	}
}

// TestPlan_ShardedRoutesEligible: Mode=="sharded" drops thresholds to the
// floor so an eligible plan routes even below the auto cost thresholds.
func TestPlan_ShardedRoutesEligible(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Mode = ModeSharded
	// A modest eligible shape that would be below-threshold under auto:
	// Range=1m (F=4 < Fmin), N=241. Eligible, so sharded routes it.
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
	p := &Planner{Cfg: cfg}
	d, routed := p.Plan(rw, oomMeta())
	if !routed {
		t.Fatalf("sharded must route eligible plan; reason=%q", d.Reason)
	}
	if d.Reason != ReasonRouted {
		t.Fatalf("reason = %q, want routed", d.Reason)
	}
}

func TestPlan_RejectionTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		plan   func() chplan.Node
		meta   func() RequestMeta
		reason string
	}{
		{
			name: "now64 in filter predicate",
			plan: func() chplan.Node {
				rw := oomWindow().(*chplan.Aggregate)
				return &chplan.Filter{
					Input: rw,
					Predicate: &chplan.Binary{
						Op:    chplan.OpLt,
						Left:  &chplan.ColumnRef{Name: "TimeUnix"},
						Right: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}},
					},
				}
			},
			meta:   oomMeta,
			reason: ReasonNow64,
		},
		{
			name: "now64 in scalar subquery input",
			plan: func() chplan.Node {
				agg := oomWindow().(*chplan.Aggregate)
				// Filter whose predicate compares against a ScalarSubquery
				// whose interior projects now64.
				scalarInner := &chplan.Project{
					Input:       leafScan(),
					Projections: []chplan.Projection{{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: "v"}},
				}
				return &chplan.Filter{
					Input: agg,
					Predicate: &chplan.Binary{
						Op:    chplan.OpGt,
						Left:  &chplan.ColumnRef{Name: "Value"},
						Right: &chplan.ScalarSubquery{Input: scalarInner},
					},
				}
			},
			meta:   oomMeta,
			reason: ReasonNow64,
		},
		{
			name: "unpinned bounds on outer window",
			plan: func() chplan.Node {
				return &chplan.RangeWindow{
					Input:      leafScan(),
					Func:       "rate",
					Range:      5 * time.Minute,
					Step:       gridStep,
					OuterRange: time.Hour,
					// Start/End left zero — unpinned outer window.
					TimestampColumn: "TimeUnix",
					ValueColumn:     "Value",
					GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
				}
			},
			meta:   oomMeta,
			reason: ReasonInstant,
		},
		{
			name: "instant query (Step == 0)",
			plan: oomWindow,
			meta: func() RequestMeta {
				m := oomMeta()
				m.Step = 0
				return m
			},
			reason: ReasonInstant,
		},
		{
			name: "unmarked slice-invariant node",
			plan: func() chplan.Node {
				// OrderBy is NOT in the slice-invariant registry.
				return &chplan.OrderBy{
					Input: oomWindow(),
					Keys:  []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "Value"}}},
				}
			},
			meta:   oomMeta,
			reason: ReasonNotSliceable,
		},
		{
			name: "grid mismatch (@-pinned End)",
			plan: func() chplan.Node {
				rw := &chplan.RangeWindow{
					Input:           leafScan(),
					Func:            "rate",
					Range:           5 * time.Minute,
					Step:            gridStep,
					OuterRange:      time.Hour,
					Start:           gridStart,
					End:             gridEnd.Add(-7 * time.Minute), // diverges from predicted grid
					TimestampColumn: "TimeUnix",
					ValueColumn:     "Value",
					GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
				}
				return rw
			},
			meta:   oomMeta,
			reason: ReasonGridMismatch,
		},
		{
			name: "below-threshold (F < Fmin)",
			plan: func() chplan.Node {
				// Range=1m → F=4 < Fmin=16. N=241. Auto: below threshold.
				return &chplan.RangeWindow{
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
			},
			meta:   oomMeta,
			reason: ReasonBelowThreshold,
		},
		{
			name: "below-threshold (N*F < MinAnchorPairs)",
			plan: func() chplan.Node {
				// Range=5m (F=20 >= Fmin), but a short 5m window keeps N small
				// enough (N=21) that N*F = 420 < 4000, while D=5m stays small
				// vs OuterRange=5m so the high-D clamp does not fire first
				// (floor(5m/5m)=1 → high-D would, so widen OuterRange to 30m,
				// D=5m → floor(30m/5m)=6 ample; N=121, F=20 → N*F=2420 < 4000).
				start := gridStart
				end := start.Add(30 * time.Minute)
				return &chplan.RangeWindow{
					Input:           leafScan(),
					Func:            "rate",
					Range:           5 * time.Minute,
					Step:            gridStep,
					OuterRange:      30 * time.Minute, // N = 121
					Start:           start,
					End:             end,
					TimestampColumn: "TimeUnix",
					ValueColumn:     "Value",
					GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
				}
			},
			meta: func() RequestMeta {
				m := oomMeta()
				m.End = m.Start.Add(30 * time.Minute)
				return m
			},
			reason: ReasonBelowThreshold,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &Planner{Cfg: autoCfg()}
			d, routed := p.Plan(tc.plan(), tc.meta())
			if routed {
				t.Fatalf("%s: expected NOT routed, got routed (K=%d)", tc.name, d.K)
			}
			if d.Reason != tc.reason {
				t.Fatalf("%s: reason = %q, want %q", tc.name, d.Reason, tc.reason)
			}
		})
	}
}

// TestPlan_ScalarHeavyRejected: a ScalarSubquery whose interior carries its
// own windowed spine cannot be replicated K× in phase 1 → scalar-heavy.
func TestPlan_ScalarHeavyRejected(t *testing.T) {
	t.Parallel()
	agg := oomWindow().(*chplan.Aggregate)
	heavyInner := &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "sum_over_time",
		Range:           24 * time.Hour,
		Step:            time.Minute,
		OuterRange:      time.Hour,
		Start:           gridStart,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	plan := &chplan.Filter{
		Input: agg,
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "Value"},
			Right: &chplan.ScalarSubquery{Input: heavyInner},
		},
	}
	p := &Planner{Cfg: autoCfg()}
	d, routed := p.Plan(plan, oomMeta())
	if routed {
		t.Fatal("scalar-heavy plan must not route")
	}
	if d.Reason != ReasonScalarHeavy {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonScalarHeavy)
	}
}

// TestPlan_IncommensurateNestedSpine: a nested spine whose inner resolution
// leaves no valid slice quantum window → incommensurate.
func TestPlan_IncommensurateNestedSpine(t *testing.T) {
	t.Parallel()
	// Outer grid N small enough that N/2 < MinAnchorsPerSlice, so there is
	// no room for a valid quantum window once a nested spine exists.
	step := time.Minute
	start := gridStart
	end := start.Add(20 * time.Minute) // N = 21
	inner := &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "rate",
		Range:           time.Minute,
		Step:            7 * time.Second, // co-prime inner resolution
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	outer := &chplan.RangeWindow{
		Input:           inner,
		Func:            "max_over_time",
		Range:           5 * time.Minute,
		Step:            step,
		OuterRange:      20 * time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "anchor_ts",
		ValueColumn:     "Value",
	}
	p := &Planner{Cfg: autoCfg()}
	d, routed := p.Plan(outer, RequestMeta{Lang: "promql", Start: start, End: end, Step: step})
	if routed {
		t.Fatal("incommensurate nested spine must not route")
	}
	if d.Reason != ReasonIncommensurate {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonIncommensurate)
	}
}

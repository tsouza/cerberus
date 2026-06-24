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
	p := NewPlanner(autoCfg())
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

// lwrSpine builds a bare-selector last-with-respect-to plan over the canonical
// grid — the deriv / idelta / irate / instant-LWR / negative-offset family the
// phase-3 widening admits. With Lookback=5m / Step=15s the membership fan-out
// F = Lookback/Step = 20, N = 241, so it clears the auto cost thresholds.
func lwrSpine(offset time.Duration) chplan.Node {
	return &chplan.RangeLWR{
		Input:         leafScan(),
		Start:         gridStart,
		End:           gridEnd,
		Step:          gridStep,
		Lookback:      5 * time.Minute,
		Offset:        offset,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
}

// TestPlan_RangeLWRSpineRoutes is the phase-3 advancement: a bare-selector
// RangeLWR spine that route A left un-sliceable now ROUTES B with K >= 2 and
// correctly-anchored slices. Both the zero-offset and the negative-offset
// (offset -5m) shapes route — the offset shifts only the membership window, not
// the grid, so the anchor decomposition is unchanged.
func TestPlan_RangeLWRSpineRoutes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		offset time.Duration
	}{
		{"zero offset", 0},
		{"negative offset", -5 * time.Minute},
		{"positive offset", time.Hour},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := NewPlanner(autoCfg())
			d, routed := p.Plan(lwrSpine(tc.offset), oomMeta())
			if !routed {
				t.Fatalf("RangeLWR spine must route; got reason=%q", d.Reason)
			}
			if d.Reason != ReasonRouted {
				t.Fatalf("reason = %q, want %q", d.Reason, ReasonRouted)
			}
			if d.K < 2 {
				t.Fatalf("K = %d, want >= 2", d.K)
			}
			if d.Strategy != StrategyShardedTimeslice {
				t.Fatalf("strategy = %q, want %q", d.Strategy, StrategyShardedTimeslice)
			}
			if len(d.Slices) != d.K {
				t.Fatalf("len(Slices) = %d, want K=%d", len(d.Slices), d.K)
			}
			// The produced slices must re-grid onto RangeLWR nodes whose bounds
			// are filled (non-zero) and whose union covers the original grid:
			// oldest slice starts at the grid Start, newest ends at grid End.
			oldest := d.Slices[0].Plan.(*chplan.RangeLWR)
			newest := d.Slices[len(d.Slices)-1].Plan.(*chplan.RangeLWR)
			if !oldest.Start.Equal(gridStart) {
				t.Fatalf("oldest slice Start=%v, want grid Start=%v", oldest.Start, gridStart)
			}
			if !newest.End.Equal(gridEnd) {
				t.Fatalf("newest slice End=%v, want grid End=%v", newest.End, gridEnd)
			}
			for _, sl := range d.Slices {
				r := sl.Plan.(*chplan.RangeLWR)
				if r.Start.IsZero() || r.End.IsZero() {
					t.Fatalf("slice %d left RangeLWR bounds unpinned: Start=%v End=%v",
						sl.Index, r.Start, r.End)
				}
				if r.Step != gridStep || r.Lookback != 5*time.Minute || r.Offset != tc.offset {
					t.Fatalf("slice %d RangeLWR lost a non-grid field: %+v", sl.Index, r)
				}
			}
		})
	}
}

// TestPlan_SingleNeverRoutes: Mode=="single" classifies but never routes,
// even for the eligible OOM shape.
func TestPlan_SingleNeverRoutes(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig() // Mode == single
	p := NewPlanner(cfg)
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
	p := NewPlanner(cfg)
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
			p := NewPlanner(autoCfg())
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

// TestPlan_Now64InAggregateArgsRejected pins DEFECT 1: a now64 hidden in the
// OUTERMOST Aggregate's AggFuncs[].Args (off the windowed spine) must be swept
// by walkNode — otherwise the OOM shape routes despite a now64, and two shards
// would resolve different wall-clocks.
func TestPlan_Now64InAggregateArgsRejected(t *testing.T) {
	t.Parallel()
	agg := oomWindow().(*chplan.Aggregate)
	// Inject now64 into the outer sum's argument: sum(rate(m[5m]) * now64()).
	agg.AggFuncs = []chplan.AggFunc{{
		Name: "sum",
		Args: []chplan.Expr{&chplan.Binary{
			Op:    chplan.OpMul,
			Left:  &chplan.ColumnRef{Name: "Value"},
			Right: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}},
		}},
	}}
	p := NewPlanner(autoCfg())
	d, routed := p.Plan(agg, oomMeta())
	if routed {
		t.Fatal("now64 in outer Aggregate args must not route")
	}
	if d.Reason != ReasonNow64 {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonNow64)
	}
}

// TestPlan_Now64InAggregateGroupByRejected: a now64 in the outer Aggregate's
// GroupBy keys is the sibling gap walkNode must also sweep.
func TestPlan_Now64InAggregateGroupByRejected(t *testing.T) {
	t.Parallel()
	agg := oomWindow().(*chplan.Aggregate)
	agg.GroupBy = []chplan.Expr{&chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}}
	p := NewPlanner(autoCfg())
	d, routed := p.Plan(agg, oomMeta())
	if routed {
		t.Fatal("now64 in outer Aggregate GroupBy must not route")
	}
	if d.Reason != ReasonNow64 {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonNow64)
	}
}

// TestPlan_Now64InScalarInteriorAggregateRejected pins the DEFECT 1 sibling in
// walkScalarInterior: scalar(sum(... now64 ...)) — a now64 inside an Aggregate
// nested in a ScalarSubquery interior must be caught.
func TestPlan_Now64InScalarInteriorAggregateRejected(t *testing.T) {
	t.Parallel()
	agg := oomWindow().(*chplan.Aggregate)
	// ScalarSubquery whose interior is an Aggregate with now64 in its args.
	scalarInner := &chplan.Aggregate{
		Input: leafScan(),
		AggFuncs: []chplan.AggFunc{{
			Name:  "sum",
			Args:  []chplan.Expr{&chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}},
			Alias: "v",
		}},
	}
	plan := &chplan.Filter{
		Input: agg,
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "Value"},
			Right: &chplan.ScalarSubquery{Input: scalarInner},
		},
	}
	p := NewPlanner(autoCfg())
	d, routed := p.Plan(plan, oomMeta())
	if routed {
		t.Fatal("now64 in scalar-interior Aggregate must not route")
	}
	if d.Reason != ReasonNow64 {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonNow64)
	}
}

// TestPlan_NonRangeWindowSpineRejected pins the residual routable-spine
// restriction after phase 3: the routable bound-carriers are *RangeWindow
// (phase 1) and *RangeLWR (phase 3). A RangeBucketFanout / StepGrid spine still
// carries a grid ReanchorRange leaves un-re-anchored (CloneNode'd verbatim), so
// each such plan must fail closed to route A with Reason=not-sliceable.
func TestPlan_NonRangeWindowSpineRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		plan func() chplan.Node
	}{
		{
			name: "RangeBucketFanout spine",
			plan: func() chplan.Node {
				return &chplan.RangeBucketFanout{
					Input:        leafScan(),
					Start:        gridStart,
					End:          gridEnd,
					Step:         gridStep,
					Lookback:     5 * time.Minute,
					AnchorAlias:  "anchor_ts",
					TimestampCol: "TimeUnix",
					AggFuncs: []chplan.AggFunc{{
						Name:  "sumForEach",
						Args:  []chplan.Expr{&chplan.ColumnRef{Name: "BucketCounts"}},
						Alias: "BucketCounts",
					}},
				}
			},
		},
		{
			name: "StepGrid spine",
			plan: func() chplan.Node {
				return &chplan.StepGrid{Start: gridStart, End: gridEnd, Step: gridStep}
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := NewPlanner(autoCfg())
			d, routed := p.Plan(tc.plan(), oomMeta())
			if routed {
				t.Fatalf("%s must not route (K=%d)", tc.name, d.K)
			}
			if d.Reason != ReasonNotSliceable {
				t.Fatalf("%s: reason = %q, want %q", tc.name, d.Reason, ReasonNotSliceable)
			}
		})
	}
}

// TestPlan_SingleProducedSliceNotRouted pins DEFECT 3: a tiny-N eligible plan
// whose singleton-tail merge collapses to ONE produced slice must report NOT
// routed (route A) — never a K=1 routed Decision (the doc's "route iff K>=2").
func TestPlan_SingleProducedSliceNotRouted(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Mode = ModeSharded // floor thresholds so eligibility, not cost, decides
	// N = 3 anchors (Step=1m over 2m). m = ceil(3/2) = 2 → spans {2,1};
	// the singleton tail (count 1) merges into its neighbor → ONE slice.
	step := time.Minute
	start := gridStart
	end := start.Add(2 * time.Minute)
	plan := &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "rate",
		Range:           time.Minute,
		Step:            step,
		OuterRange:      2 * time.Minute,
		Start:           start,
		End:             end,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	meta := RequestMeta{Lang: "promql", Start: start, End: end, Step: step}
	p := NewPlanner(cfg)
	d, routed := p.Plan(plan, meta)
	if routed {
		t.Fatalf("a plan collapsing to one slice must not route (K=%d)", d.K)
	}
	if d.Reason != ReasonBelowThreshold {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonBelowThreshold)
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
	p := NewPlanner(autoCfg())
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
	p := NewPlanner(autoCfg())
	d, routed := p.Plan(outer, RequestMeta{Lang: "promql", Start: start, End: end, Step: step})
	if routed {
		t.Fatal("incommensurate nested spine must not route")
	}
	if d.Reason != ReasonIncommensurate {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonIncommensurate)
	}
}

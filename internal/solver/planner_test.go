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
		AggFuncs: []chplan.AggFunc{{Fn: chplan.FnSum, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
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
			p := &Planner{Cfg: autoCfg()}
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
						Right: &chplan.FuncCall{Fn: chplan.FnNow64, Args: []chplan.Expr{&chplan.LitInt{V: 9}}},
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
					Projections: []chplan.Projection{{Expr: &chplan.FuncCall{Fn: chplan.FnNow64, Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: "v"}},
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

// TestPlan_Now64InAggregateArgsRejected pins DEFECT 1: a now64 hidden in the
// OUTERMOST Aggregate's AggFuncs[].Args (off the windowed spine) must be swept
// by walkNode — otherwise the OOM shape routes despite a now64, and two shards
// would resolve different wall-clocks.
func TestPlan_Now64InAggregateArgsRejected(t *testing.T) {
	t.Parallel()
	agg := oomWindow().(*chplan.Aggregate)
	// Inject now64 into the outer sum's argument: sum(rate(m[5m]) * now64()).
	agg.AggFuncs = []chplan.AggFunc{{
		Fn: chplan.FnSum,
		Args: []chplan.Expr{&chplan.Binary{
			Op:    chplan.OpMul,
			Left:  &chplan.ColumnRef{Name: "Value"},
			Right: &chplan.FuncCall{Fn: chplan.FnNow64, Args: []chplan.Expr{&chplan.LitInt{V: 9}}},
		}},
	}}
	p := &Planner{Cfg: autoCfg()}
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
	agg.GroupBy = []chplan.Expr{&chplan.FuncCall{Fn: chplan.FnNow64, Args: []chplan.Expr{&chplan.LitInt{V: 9}}}}
	p := &Planner{Cfg: autoCfg()}
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
			Fn:    chplan.FnSum,
			Args:  []chplan.Expr{&chplan.FuncCall{Fn: chplan.FnNow64, Args: []chplan.Expr{&chplan.LitInt{V: 9}}}},
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
	p := &Planner{Cfg: autoCfg()}
	d, routed := p.Plan(plan, oomMeta())
	if routed {
		t.Fatal("now64 in scalar-interior Aggregate must not route")
	}
	if d.Reason != ReasonNow64 {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonNow64)
	}
}

// rangeBucketFanoutSpine builds the array-aggregate fan-out behind the
// classic-histogram families over the standard 1h/15s grid, mirroring
// lwrSpine's shape (Lookback 5m, no offset).
// rangeBucketFanoutSpine is the CLASSIC bucket-ladder fan-out — the shape
// behind the APM-style panel. PeakIndependentOfGrid mirrors what
// histogram_quantile_range.go's classic lowering sets: route A was measured to
// fit (2.84 GB), so slicing it is waste.
func rangeBucketFanoutSpine() *chplan.RangeBucketFanout {
	return &chplan.RangeBucketFanout{
		Input:                 leafScan(),
		PeakIndependentOfGrid: true,
		Start:                 gridStart,
		End:                   gridEnd,
		Step:                  gridStep,
		Lookback:              5 * time.Minute,
		AnchorAlias:           "anchor_ts",
		TimestampCol:          "TimeUnix",
		AggFuncs: []chplan.AggFunc{{
			Fn:    chplan.FnSumForEach,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: "BucketCounts"}},
			Alias: "BucketCounts",
		}},
	}
}

// TestPlan_RangeBucketFanoutSpineDeclinesIndivisibleGrid pins the cost-model
// correction: a bare RangeBucketFanout spine is STRUCTURALLY sliceable (PR
// #2387 made it so, and TestEligible_SlicesIndivisibleAnchorGrid below still
// proves it), but ModeAuto declines to route it PREDICTIVELY because its peak
// intermediate is Theta(rows x Lookback/Step) — constant in the grid width — so
// slicing replicates the per-(series, anchor) fold instead of partitioning it.
//
// Same shape and grid as the real motivating query (1h/15s = 241 anchors,
// 5m lookback). Measured on ClickHouse 26.6 at K=12: route B cost
// 23x the ClickHouse work (185,101 ms vs 8,070 ms) and read 36x the rows, to
// recover 8.7% of a perfectly-divisible peak.
func TestPlan_RangeBucketFanoutSpineDeclinesIndivisibleGrid(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	d, routed := p.Plan(rangeBucketFanoutSpine(), oomMeta())
	if routed {
		t.Fatalf("RangeBucketFanout spine must NOT route predictively; got K=%d", d.K)
	}
	if d.Reason != ReasonAnchorGridIndivisible {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonAnchorGridIndivisible)
	}
}

// TestEligible_SlicesIndivisibleAnchorGrid is the #2387 SAFETY PIN. The cost
// gate above is a ModeAuto PREDICTION; Eligible() is the seam the
// failure-driven route memo calls after a real route-A resource failure, and it
// must still slice this shape — otherwise the classic-histogram OOM #2387 fixed
// would come back with no way out. Model sets the prior; measurement overrides.
//
// It also carries #2387's SLICING invariants. ModeAuto no longer reaches the
// slicer for this shape, but the memo seam does, so the assertions move here
// rather than disappearing: the slices span the whole grid
// (gridStart/gridEnd/gridStep = 1h/15s = 240 anchors), none is left with
// unpinned bounds, and none loses a non-grid field.
func TestEligible_SlicesIndivisibleAnchorGrid(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	d, ok := p.Eligible(rangeBucketFanoutSpine(), oomMeta())
	if !ok {
		t.Fatal("Eligible must still slice a RangeBucketFanout spine; the route memo depends on it")
	}
	if d == nil || d.K < 2 {
		t.Fatalf("Eligible K = %v, want >= 2", d)
	}
	if d.Strategy != StrategyShardedTimeslice {
		t.Fatalf("strategy = %q, want %q", d.Strategy, StrategyShardedTimeslice)
	}
	if len(d.Slices) != d.K {
		t.Fatalf("len(Slices) = %d, want K=%d", len(d.Slices), d.K)
	}
	oldest := d.Slices[0].Plan.(*chplan.RangeBucketFanout)
	newest := d.Slices[len(d.Slices)-1].Plan.(*chplan.RangeBucketFanout)
	if !oldest.Start.Equal(gridStart) {
		t.Fatalf("oldest slice Start=%v, want grid Start=%v", oldest.Start, gridStart)
	}
	if !newest.End.Equal(gridEnd) {
		t.Fatalf("newest slice End=%v, want grid End=%v", newest.End, gridEnd)
	}
	for _, sl := range d.Slices {
		r := sl.Plan.(*chplan.RangeBucketFanout)
		if r.Start.IsZero() || r.End.IsZero() {
			t.Fatalf("slice %d left RangeBucketFanout bounds unpinned: Start=%v End=%v",
				sl.Index, r.Start, r.End)
		}
		if r.Step != gridStep || r.Lookback != 5*time.Minute {
			t.Fatalf("slice %d RangeBucketFanout lost a non-grid field: %+v", sl.Index, r)
		}
	}
}

// TestPlan_ShardedModeIgnoresIndivisibleAnchorGrid: the gate is ModeAuto's cost
// PROXY, not a correctness refusal, so an operator who forced route B still
// gets it — and the force-route parity lanes stay unaffected.
func TestPlan_ShardedModeIgnoresIndivisibleAnchorGrid(t *testing.T) {
	t.Parallel()
	cfg := autoCfg()
	cfg.Mode = ModeSharded
	p := &Planner{Cfg: cfg}
	d, routed := p.Plan(rangeBucketFanoutSpine(), oomMeta())
	if !routed {
		t.Fatalf("ModeSharded must still route; got reason=%q", d.Reason)
	}
}

// TestPlan_HistogramQuantileOverRangeBucketFanoutDeclinesIndivisibleGrid is the
// full motivating spine — histogram_quantile over the classic-bucket fan-out —
// declining to route predictively for the same reason the bare spine does. This
// is the exact shape of the APM panel whose p50/p75/p99 columns timed out under
// load: sharded it issued 12 ClickHouse queries totalling 185,101 ms;
// unsliced it answers in 8,070 ms.
func TestPlan_HistogramQuantileOverRangeBucketFanoutDeclinesIndivisibleGrid(t *testing.T) {
	t.Parallel()
	plan := &chplan.HistogramQuantile{
		Input:                rangeBucketFanoutSpine(),
		Phi:                  0.99,
		BucketCountsColumn:   "BucketCounts",
		ExplicitBoundsColumn: "ExplicitBounds",
		MetricNameColumn:     "MetricName",
		AttributesColumn:     "Attributes",
		TimestampColumn:      "TimeUnix",
	}
	p := &Planner{Cfg: autoCfg()}
	d, routed := p.Plan(plan, oomMeta())
	if routed {
		t.Fatalf("HistogramQuantile over RangeBucketFanout must NOT route predictively; got K=%d", d.K)
	}
	if d.Reason != ReasonAnchorGridIndivisible {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonAnchorGridIndivisible)
	}
	// The memo seam must still be able to slice it — same safety pin as
	// TestEligible_SlicesIndivisibleAnchorGrid, at the full spine — and it must
	// slice it CORRECTLY. This is where #2387's silent-K-duplication hazard
	// lives: HistogramQuantile is registered slice-invariant, so if
	// chplan.ReanchorRange ever loses its *HistogramQuantile pass-through arm
	// its default case returns the node UNRECURSED, handing every shard the
	// ORIGINAL full-grid RangeBucketFanout — K duplicated copies of the same
	// rows, not an error. Distinct inner Starts are what a regression breaks.
	ed, ok := p.Eligible(plan, oomMeta())
	if !ok {
		t.Fatal("Eligible must still slice the HistogramQuantile spine")
	}
	seen := map[time.Time]bool{}
	for _, sl := range ed.Slices {
		hq, ok := sl.Plan.(*chplan.HistogramQuantile)
		if !ok {
			t.Fatalf("slice %d plan is %T, want *chplan.HistogramQuantile", sl.Index, sl.Plan)
		}
		rbf, ok := hq.Input.(*chplan.RangeBucketFanout)
		if !ok {
			t.Fatalf("slice %d HistogramQuantile.Input is %T, want *chplan.RangeBucketFanout", sl.Index, hq.Input)
		}
		if rbf.Start.IsZero() || rbf.End.IsZero() {
			t.Fatalf("slice %d left inner RangeBucketFanout bounds unpinned: Start=%v End=%v",
				sl.Index, rbf.Start, rbf.End)
		}
		if seen[rbf.Start] {
			t.Fatalf("slice %d inner RangeBucketFanout.Start=%v duplicates an earlier slice — "+
				"every shard re-gridded to the SAME bounds instead of its own (the silent K-duplication hazard)",
				sl.Index, rbf.Start)
		}
		seen[rbf.Start] = true
	}
	oldest := ed.Slices[0].Plan.(*chplan.HistogramQuantile).Input.(*chplan.RangeBucketFanout)
	newest := ed.Slices[len(ed.Slices)-1].Plan.(*chplan.HistogramQuantile).Input.(*chplan.RangeBucketFanout)
	if !oldest.Start.Equal(gridStart) {
		t.Fatalf("oldest slice inner Start=%v, want grid Start=%v", oldest.Start, gridStart)
	}
	if !newest.End.Equal(gridEnd) {
		t.Fatalf("newest slice inner End=%v, want grid End=%v", newest.End, gridEnd)
	}
}

// TestPlan_Now64InHistogramQuantilePhiExprRejected pins the walkNode arm
// HistogramQuantile needs alongside its slice-invariance registration: a
// computed phi (`histogram_quantile(scalar(x), b)`) lowers PhiExpr to a
// ScalarSubquery (see chplan.HistogramQuantile.PhiExpr's doc), and
// walkScalarInterior's own doc already names "a histogram_quantile's phi" as
// a historically-missed slot that let a now64 escape every gate and route B.
// Registering HistogramQuantile as slice-invariant (the classic-histogram OOM
// fix) reopens exactly that hazard at this package's own top-level walk
// unless walkNode also sweeps GroupBy/PhiExpr the way the Aggregate arm
// sweeps its own — this test is what a regression in that sweep would break.
//
// The now64 rejection is a CORRECTNESS gate inside classify(), so it fires
// before ModeAuto's anchor-grid cost gate is ever consulted; the reason must
// stay ReasonNow64 and not drift to ReasonAnchorGridIndivisible, which would
// mean the sweep had stopped running and the cost gate was masking it.
func TestPlan_Now64InHistogramQuantilePhiExprRejected(t *testing.T) {
	t.Parallel()
	scalarInner := &chplan.Aggregate{
		Input: leafScan(),
		AggFuncs: []chplan.AggFunc{{
			Fn:    chplan.FnSum,
			Args:  []chplan.Expr{&chplan.FuncCall{Fn: chplan.FnNow64, Args: []chplan.Expr{&chplan.LitInt{V: 9}}}},
			Alias: "v",
		}},
	}
	plan := &chplan.HistogramQuantile{
		Input:                rangeBucketFanoutSpine(),
		PhiExpr:              &chplan.ScalarSubquery{Input: scalarInner},
		BucketCountsColumn:   "BucketCounts",
		ExplicitBoundsColumn: "ExplicitBounds",
		MetricNameColumn:     "MetricName",
		AttributesColumn:     "Attributes",
		TimestampColumn:      "TimeUnix",
	}
	p := &Planner{Cfg: autoCfg()}
	d, routed := p.Plan(plan, oomMeta())
	if routed {
		t.Fatal("now64 in HistogramQuantile.PhiExpr's scalar interior must not route")
	}
	if d.Reason != ReasonNow64 {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonNow64)
	}
	// Eligible() bypasses the COST proxy, never a correctness gate: a now64
	// interior must stay ineligible at the memo seam too.
	if _, ok := p.Eligible(plan, oomMeta()); ok {
		t.Fatal("Eligible must not slice a plan with now64 in HistogramQuantile.PhiExpr")
	}
}

// TestPlan_NonRangeWindowSpineRejected pins the residual routable-spine
// restriction: the routable bound-carriers are *RangeWindow (phase 1),
// *RangeLWR (phase 3), and *RangeBucketFanout (the classic-histogram
// OOM fix — see TestEligible_SlicesIndivisibleAnchorGrid for the positive case).
// A StepGrid spine still carries a grid ReanchorRange leaves un-re-anchored
// (CloneNode'd verbatim), so it must fail closed to route A with
// Reason=not-sliceable.
func TestPlan_NonRangeWindowSpineRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		plan func() chplan.Node
	}{
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
			p := &Planner{Cfg: autoCfg()}
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
	p := &Planner{Cfg: cfg}
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
	p := &Planner{Cfg: autoCfg()}
	d, routed := p.Plan(plan, oomMeta())
	if routed {
		t.Fatal("scalar-heavy plan must not route")
	}
	if d.Reason != ReasonScalarHeavy {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonScalarHeavy)
	}
}

// TestPlan_ScalarAnchorCompatibleRoutes: a ScalarSubquery whose interior is a
// per-step RangeLWR sitting EXACTLY on the grid the request predicts at the
// point it is embedded — the shape #1455/#1886 produce for a computed scalar
// argument such as `clamp_max(v, scalar(bound))` — must be admitted (NOT
// scalar-heavy), because route B never re-anchors an Expr-embedded interior:
// it is shared verbatim into every shard exactly as route A would evaluate
// it, so admitting it changes cost, never correctness. This pins EPIC #1469's
// checkScalarHeavy half: kind-based conservatism is replaced by an
// anchor-compatibility proof.
func TestPlan_ScalarAnchorCompatibleRoutes(t *testing.T) {
	t.Parallel()
	agg := oomWindow().(*chplan.Aggregate)
	anchoredInner := &chplan.RangeLWR{
		Input:         leafScan(),
		Start:         gridStart,
		End:           gridEnd,
		Step:          gridStep,
		Lookback:      5 * time.Minute,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
	plan := &chplan.Filter{
		Input: agg,
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "Value"},
			Right: &chplan.ScalarSubquery{Input: anchoredInner},
		},
	}
	p := &Planner{Cfg: autoCfg()}
	d, routed := p.Plan(plan, oomMeta())
	if !routed {
		t.Fatalf("anchor-compatible scalar interior must route; reason=%q", d.Reason)
	}
	if d.Reason != ReasonRouted {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonRouted)
	}
	if d.K != 8 {
		t.Fatalf("K = %d, want 8 (same OOM shape as TestPlan_OOMShapeRoutes)", d.K)
	}
}

// TestPlan_ScalarNativeAnchorCompatibleRoutes is the positive counterpart for
// the native arm of the anchor-compatibility check: a native timeSeries*ToGrid
// interior sitting EXACTLY on the grid AND cadence predicted where it is
// embedded is bounded to one value per outer anchor, so it is admitted and the
// plan routes.
//
// Without it the negative rows below would be satisfied by an arm that refuses
// EVERY native interior, which is a different (and needlessly narrow) policy
// than the one the fan-out family gets.
func TestPlan_ScalarNativeAnchorCompatibleRoutes(t *testing.T) {
	t.Parallel()
	anchoredInner := &chplan.RangeWindowGridNative{
		Input:           leafScan(),
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            gridStep,
		Start:           gridStart,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	plan := &chplan.Filter{
		Input: oomWindow().(*chplan.Aggregate),
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "Value"},
			Right: &chplan.ScalarSubquery{Input: anchoredInner},
		},
	}
	d, routed := (&Planner{Cfg: autoCfg()}).Plan(plan, oomMeta())
	if !routed {
		t.Fatalf("an anchor-compatible native scalar interior must route; reason=%q", d.Reason)
	}
	if d.Reason != ReasonRouted {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonRouted)
	}
}

// TestPlan_ScalarAnchorIncompatibleRejected is the negative table for the
// anchor-compatibility carve-out: each case builds a windowed interior that
// resembles TestPlan_ScalarAnchorCompatibleRoutes's admitted shape in every
// way but ONE, and every one of them must still be scalar-heavy — a false
// ADMISSION here would let route B replicate a genuinely unbounded scan K×,
// which is the mistake this check exists to rule out.
func TestPlan_ScalarAnchorIncompatibleRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		inner chplan.Node
	}{
		{
			// Same (Start, End) as the outer grid, but a DIFFERENT cadence
			// (1m vs the outer grid's 15s): the interior is not provably
			// bounded to one value per OUTER anchor, so it stays heavy even
			// though its span coincides with the outer grid's span. Mirrors
			// TestPlan_ScalarHeavyRejected's heavyInner.
			name: "grid matches, step diverges",
			inner: &chplan.RangeWindow{
				Input:           leafScan(),
				Func:            "sum_over_time",
				Range:           24 * time.Hour,
				Step:            time.Minute,
				OuterRange:      time.Hour,
				Start:           gridStart,
				End:             gridEnd,
				TimestampColumn: "TimeUnix",
				ValueColumn:     "Value",
			},
		},
		{
			// Same cadence as the outer grid, but a completely independent
			// span (a window 30 days in the past) — an @-pinned-style
			// divergence that really would need full, unbounded replication.
			name: "step matches, span diverges",
			inner: &chplan.RangeLWR{
				Input:         leafScan(),
				Start:         gridStart.Add(-30 * 24 * time.Hour),
				End:           gridEnd.Add(-30 * 24 * time.Hour),
				Step:          gridStep,
				Lookback:      5 * time.Minute,
				MetricNameCol: "MetricName",
				AttributesCol: "Attributes",
				TimestampCol:  "TimeUnix",
				ValueCol:      "Value",
			},
		},
		{
			// A native timeSeries*ToGrid interior whose span is independent of
			// the outer grid. It is registered slice-invariant (issue #2117), so
			// walkScalarInterior's sweep no longer refuses it and
			// scalarInteriorAnchorCompatible's own arm is the only thing between
			// this and replicating a 30-day single-pass grid aggregate K times.
			name: "RangeWindowGridNative, span diverges",
			inner: &chplan.RangeWindowGridNative{
				Input:           leafScan(),
				Func:            "rate",
				Range:           5 * time.Minute,
				Step:            gridStep,
				Start:           gridStart.Add(-30 * 24 * time.Hour),
				End:             gridEnd.Add(-30 * 24 * time.Hour),
				TimestampColumn: "TimeUnix",
				ValueColumn:     "Value",
			},
		},
		{
			// The same native interior on the outer grid's exact span but at a
			// coarser cadence: not provably one value per OUTER anchor, so it
			// stays heavy for the same reason the fan-out row above does.
			name: "RangeWindowGridNative, grid matches, step diverges",
			inner: &chplan.RangeWindowGridNative{
				Input:           leafScan(),
				Func:            "rate",
				Range:           5 * time.Minute,
				Step:            time.Minute,
				Start:           gridStart,
				End:             gridEnd,
				TimestampColumn: "TimeUnix",
				ValueColumn:     "Value",
			},
		},
		{
			// A RangeBucketFanout sitting EXACTLY on the outer grid and
			// cadence — still never admitted, because it is outside the
			// routable spine family on the main spine too (signal 1b) and
			// this package has no argument that makes it safe here that it
			// does not already have there.
			name: "RangeBucketFanout, grid AND step match",
			inner: &chplan.RangeBucketFanout{
				Input:        leafScan(),
				Start:        gridStart,
				End:          gridEnd,
				Step:         gridStep,
				Lookback:     5 * time.Minute,
				AnchorAlias:  "anchor_ts",
				TimestampCol: "TimeUnix",
				AggFuncs: []chplan.AggFunc{{
					Fn:    chplan.FnSumForEach,
					Args:  []chplan.Expr{&chplan.ColumnRef{Name: "BucketCounts"}},
					Alias: "BucketCounts",
				}},
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			agg := oomWindow().(*chplan.Aggregate)
			plan := &chplan.Filter{
				Input: agg,
				Predicate: &chplan.Binary{
					Op:    chplan.OpGt,
					Left:  &chplan.ColumnRef{Name: "Value"},
					Right: &chplan.ScalarSubquery{Input: tc.inner},
				},
			}
			p := &Planner{Cfg: autoCfg()}
			d, routed := p.Plan(plan, oomMeta())
			if routed {
				t.Fatalf("%s: must not route", tc.name)
			}
			if d.Reason != ReasonScalarHeavy {
				t.Fatalf("%s: reason = %q, want %q", tc.name, d.Reason, ReasonScalarHeavy)
			}
		})
	}
}

// nestedSpineStep is the outer grid step the commensurability tests classify
// on: a 1-minute grid keeps the N / K / quantum arithmetic in each test's
// comment checkable by hand.
const nestedSpineStep = time.Minute

// nestedSpine wraps inner in the canonical outer matrix spine the
// commensurability tests share: a max_over_time window on the
// nestedSpineStep grid spanning [gridStart, gridStart+outerRange], pinned
// exactly where the returned RequestMeta predicts it, so the only signal
// under test is the nested grid's phase.
func nestedSpine(inner chplan.Node, outerRange time.Duration) (*chplan.RangeWindow, RequestMeta) {
	start := gridStart
	end := start.Add(outerRange)
	outer := &chplan.RangeWindow{
		Input:           inner,
		Func:            "max_over_time",
		Range:           5 * time.Minute,
		Step:            nestedSpineStep,
		OuterRange:      outerRange,
		Start:           start,
		End:             end,
		TimestampColumn: "anchor_ts",
		ValueColumn:     "Value",
	}
	return outer, RequestMeta{Lang: "promql", Start: start, End: end, Step: nestedSpineStep}
}

// shardedPlanner floors the cost thresholds so eligibility — not cost —
// decides these rows.
func shardedPlanner() *Planner {
	cfg := DefaultConfig()
	cfg.Mode = ModeSharded
	return &Planner{Cfg: cfg}
}

// TestPlan_IncommensurateNestedSpine pins that the selected slice quantum,
// rather than merely some theoretical quantum, preserves an end-phased nested
// grid. A nested RangeLWR generates its anchors as `End - i*Step` with no
// epoch snap, so a quantum that shifts End off its resolution moves the grid.
func TestPlan_IncommensurateNestedSpine(t *testing.T) {
	t.Parallel()
	inner := &chplan.RangeLWR{
		Input:        leafScan(),
		Step:         7 * time.Second, // requires a multiple-of-7 quantum.
		Lookback:     time.Minute,
		TimestampCol: "TimeUnix",
		ValueCol:     "Value",
	}
	// N = 65, K = 4, m = ceil(65/4) = 17 anchors → 17m per shard, and
	// 17m mod 7s = 5s, so every shard boundary would re-phase the leaf.
	outer, meta := nestedSpine(inner, 64*time.Minute)
	d, routed := shardedPlanner().Plan(outer, meta)
	if routed {
		t.Fatal("incommensurate nested spine must not route")
	}
	if d.Reason != ReasonIncommensurate {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonIncommensurate)
	}
}

func TestPlan_CommensurateNestedRangeLWRRoutes(t *testing.T) {
	t.Parallel()
	inner := &chplan.RangeLWR{
		Input:        leafScan(),
		Step:         40 * time.Second, // requires an even quantum.
		Lookback:     time.Minute,
		TimestampCol: "TimeUnix",
		ValueCol:     "Value",
	}
	// N = 64, K = 4, m = ceil(64/4) = 16 anchors → 16m, an exact multiple
	// of the leaf's 40s resolution, so every shard keeps its phase.
	outer, meta := nestedSpine(inner, 63*time.Minute)
	d, routed := shardedPlanner().Plan(outer, meta)
	if !routed {
		t.Fatalf("commensurate nested RangeLWR must route; got reason=%q", d.Reason)
	}
}

// TestPlan_IncommensurateNestedMatrixSpine pins that a nested MATRIX window
// whose grid is NOT epoch-aligned is end-phased too: its anchors walk back
// from the node's own End, so the same quantum arithmetic applies as for the
// nested RangeLWR above.
func TestPlan_IncommensurateNestedMatrixSpine(t *testing.T) {
	t.Parallel()
	inner := &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "rate",
		Range:           time.Minute,
		Step:            7 * time.Second, // requires a multiple-of-7 quantum.
		StepAlign:       false,           // anchors walk back from End verbatim.
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	// Same geometry as the RangeLWR case: N = 65, K = 4, m = 17 → 17m,
	// which is not a multiple of 7s.
	outer, meta := nestedSpine(inner, 64*time.Minute)
	d, routed := shardedPlanner().Plan(outer, meta)
	if routed {
		t.Fatal("incommensurate nested matrix spine must not route")
	}
	if d.Reason != ReasonIncommensurate {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonIncommensurate)
	}
}

// TestPlan_EpochAlignedNestedSpineRoutes is the counterpart: the SAME
// arithmetically-hostile nested resolution routes when the nested grid is
// StepAlign'd. The epoch snap (chplan.RangeWindow.StepAlign → chsql's
// epochAlignedEndFrag) puts every nested anchor on an absolute-epoch multiple
// of the nested Step, so moving End to a shard boundary picks a different
// newest anchor but never moves the grid off phase — the per-shard anchor set
// stays a subset of the unsliced one. This is the ordinary PromQL
// `expr[range:res]` subquery shape (`max_over_time(sum(up)[5m:1m])` on a 15s
// request grid), which must keep routing: refusing it would strand the whole
// subquery family on route A.
func TestPlan_EpochAlignedNestedSpineRoutes(t *testing.T) {
	t.Parallel()
	inner := &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "rate",
		Range:           time.Minute,
		Step:            7 * time.Second, // hostile to every quantum below…
		StepAlign:       true,            // …but epoch-anchored, so phase-invariant.
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
	outer, meta := nestedSpine(inner, 64*time.Minute)
	d, routed := shardedPlanner().Plan(outer, meta)
	if !routed {
		t.Fatalf("epoch-aligned nested spine must route; got reason=%q", d.Reason)
	}
}

// TestPlan_NativeFanoutStillRoutes is the counterweight to the classic-path
// tests above, and the reason PeakIndependentOfGrid is a per-construction-site
// flag rather than a property of the node type.
//
// The exponential/native histogram lowerings build the SAME RangeBucketFanout
// node, but their route A is where issue #2385's 19 observed
// MEMORY_LIMIT_EXCEEDED failures happened — slicing is what bounds their memory
// at all. Nothing has measured route A to fit for them, so they must keep
// routing exactly as they do today. A blanket per-node-kind bit would have
// silently taken that protection away.
func TestPlan_NativeFanoutStillRoutes(t *testing.T) {
	t.Parallel()
	spine := rangeBucketFanoutSpine()
	spine.PeakIndependentOfGrid = false // what every native lowering leaves it as

	p := &Planner{Cfg: autoCfg()}
	d, routed := p.Plan(spine, oomMeta())
	if !routed {
		t.Fatalf("a fan-out that has NOT been measured to fit route A must keep routing; got reason=%q", d.Reason)
	}
	if d.Reason != ReasonRouted {
		t.Fatalf("reason = %q, want %q", d.Reason, ReasonRouted)
	}
}

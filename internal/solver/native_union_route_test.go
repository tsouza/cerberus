package solver

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// unionArmRange is the [range] both arms of the mixed-union fixture carry. With
// gridStep = 15s it gives the fan-out arm F = 20, which clears the default
// MinFanout (16) — so the union routes on THAT arm's fan-out while the native
// arm contributes singlePassFanout. Naming it makes the "which arm cleared the
// gate" question answerable from the fixture rather than from arithmetic in a
// reader's head.
const unionArmRange = 5 * time.Minute

// nativeUnionPlan builds the plan shape issue #2117 exists to make routable:
// `sum(<UnionAll{RangeWindowNative, RangeWindow}>)` — the CUMULATIVE/DELTA
// temporality split, where the cumulative half lowers to the ClickHouse-native
// timeSeriesRateToGrid aggregate and the delta half stays on the arrayJoin
// fan-out. Both arms sit on the same request grid and project the same
// positional column shape, which is what makes the UNION ALL well-formed.
func nativeUnionPlan() chplan.Node {
	native := &chplan.RangeWindowNative{
		Input:           leafScan(),
		Func:            "rate",
		Range:           unionArmRange,
		Step:            gridStep,
		Start:           gridStart,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	fanout := &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "rate",
		Range:           unionArmRange,
		Step:            gridStep,
		OuterRange:      gridEnd.Sub(gridStart),
		Start:           gridStart,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	return &chplan.Aggregate{
		Input:    &chplan.UnionAll{Inputs: []chplan.Node{native, fanout}},
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
	}
}

// TestPlan_NativeUnionSpineRoutes is the end-to-end pin for issue #2117: the
// mixed `UnionAll{RangeWindowNative, RangeWindow}` spine routes B under
// ModeAuto.
//
// Before #2117 it was refused twice over — RangeWindowNative was absent from
// chplan's slice-invariance registry (gate 1) AND marked non-re-anchorable
// (gate 1b) — so shipping the CUMULATIVE/DELTA split would have turned a
// previously-routable counter panel into a not-sliceable rejection for any
// all-delta deployment.
//
// The routing decision rests on the FAN-OUT arm's F (the native arm reports
// singlePassFanout, below MinFanout), which is exactly right: the union's
// memory pressure is the fan-out arm's matrix, and slicing the grid is what
// bounds it.
func TestPlan_NativeUnionSpineRoutes(t *testing.T) {
	t.Parallel()

	plan := nativeUnionPlan()
	snapshot := chplan.CloneNode(plan)

	d, routed := (&Planner{Cfg: autoCfg()}).Plan(plan, oomMeta())
	if !routed {
		t.Fatalf("the mixed native/fan-out union must route; got reason=%q", d.Reason)
	}
	if d.Reason != ReasonRouted {
		t.Errorf("reason = %q, want %q", d.Reason, ReasonRouted)
	}
	if d.K < 2 {
		t.Errorf("K = %d; a route is K >= 2 by definition", d.K)
	}
	if want := int64(unionArmRange / gridStep); d.Fanout != want {
		t.Errorf("Fanout = %d, want %d — maxFanout is a MAX over carriers, so the fan-out arm's "+
			"matrix is what the union is priced on", d.Fanout, want)
	}
	if !plan.Equal(snapshot) {
		t.Error("classification mutated the input plan")
	}
}

// TestPlan_NativeUnionShardsMoveBothArms is the correctness half of the pin
// above: routing the union is only sound if EVERY arm moves onto the shard's
// grid.
//
// The arms are concatenated positionally, so an arm left at the full request
// grid would contribute its whole answer to every shard — the composed result
// is then that arm's rows K times over, beside the other arm's correctly
// partitioned rows. Nothing downstream can detect that: both arms still project
// the same column shape, so the union parses, executes and returns plausible
// numbers.
//
// It fails on any of the three independent seams this change had to widen:
// chplan.ReanchorRange's UnionAll arm, its RangeWindowNative arm, and the
// slicer's UnpinSpine (which must zero the native node's bounds, or every slice
// aborts with ErrReanchorGridMismatch and sliceAndDecide silently falls back to
// route A).
func TestPlan_NativeUnionShardsMoveBothArms(t *testing.T) {
	t.Parallel()

	plan := nativeUnionPlan()
	d, routed := (&Planner{Cfg: autoCfg()}).Plan(plan, oomMeta())
	if !routed {
		t.Fatalf("the mixed union must route; got reason=%q", d.Reason)
	}

	nativeSeen := map[int64]int{}
	fanoutSeen := map[int64]int{}
	for _, s := range d.Slices {
		var (
			native *chplan.RangeWindowNative
			fanout *chplan.RangeWindow
		)
		chplan.Walk(s.Plan, func(n chplan.Node) bool {
			switch v := n.(type) {
			case *chplan.RangeWindowNative:
				native = v
			case *chplan.RangeWindow:
				fanout = v
			}
			return true
		})
		if native == nil || fanout == nil {
			t.Fatalf("shard %d lost an arm (native=%v fanout=%v)", s.Index, native != nil, fanout != nil)
		}
		if !native.Start.Equal(s.Start) || !native.End.Equal(s.End) {
			t.Fatalf("shard %d native arm at [%v,%v], want the shard grid [%v,%v]",
				s.Index, native.Start, native.End, s.Start, s.End)
		}
		if !fanout.Start.Equal(s.Start) || !fanout.End.Equal(s.End) {
			t.Fatalf("shard %d fan-out arm at [%v,%v], want the shard grid [%v,%v]",
				s.Index, fanout.Start, fanout.End, s.Start, s.End)
		}
		if fanout.OuterRange != s.End.Sub(s.Start) {
			t.Fatalf("shard %d fan-out arm OuterRange = %s, want %s",
				s.Index, fanout.OuterRange, s.End.Sub(s.Start))
		}
		for a := s.End; !a.Before(s.Start); a = a.Add(-gridStep) {
			nativeSeen[a.UnixNano()]++
			fanoutSeen[a.UnixNano()]++
		}
	}

	orig := originalAnchors(gridStart, gridEnd, gridStep)
	for name, seen := range map[string]map[int64]int{"native": nativeSeen, "fan-out": fanoutSeen} {
		if len(seen) != len(orig) {
			t.Errorf("%s arm covers %d anchors, original grid has %d", name, len(seen), len(orig))
		}
		for _, a := range orig {
			if c := seen[a.UnixNano()]; c != 1 {
				t.Errorf("%s arm: anchor %v covered %d times, want exactly 1", name, a, c)
			}
		}
	}
}

// TestPlan_ProductionTemporalitySplitReanchorsBothArms proves the lowering
// introduced for #2114 reaches the Route-B shape exercised above. The synthetic
// fixture pins solver mechanics; this one prevents a future eligibility change
// from leaving that machinery disconnected from production lowering.
func TestPlan_ProductionTemporalitySplitReanchorsBothArms(t *testing.T) {
	t.Parallel()

	plan := productionTemporalitySplitPlan(t)
	d, routed := (&Planner{Cfg: autoCfg()}).Plan(plan, oomMeta())
	if !routed {
		t.Fatalf("production temporality split must route; got reason=%q", d.Reason)
	}
	for _, shard := range d.Slices {
		var nativeCount, fanoutCount int
		chplan.Walk(shard.Plan, func(n chplan.Node) bool {
			switch arm := n.(type) {
			case *chplan.RangeWindowNative:
				nativeCount++
				if !arm.Start.Equal(shard.Start) || !arm.End.Equal(shard.End) {
					t.Errorf("shard %d native arm = [%v,%v], want [%v,%v]", shard.Index, arm.Start, arm.End, shard.Start, shard.End)
				}
			case *chplan.RangeWindow:
				fanoutCount++
				if !arm.Start.Equal(shard.Start) || !arm.End.Equal(shard.End) {
					t.Errorf("shard %d fan-out arm = [%v,%v], want [%v,%v]", shard.Index, arm.Start, arm.End, shard.Start, shard.End)
				}
			}
			return true
		})
		if nativeCount != 1 || fanoutCount != 1 {
			t.Fatalf("shard %d has native=%d fan-out=%d arms, want one of each", shard.Index, nativeCount, fanoutCount)
		}
	}
}

func productionTemporalitySplitPlan(t *testing.T) chplan.Node {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{}).ParseExpr("rate(cerberus_queries_total[5m])")
	if err != nil {
		t.Fatalf("parse production rate: %v", err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(), gridStart, gridEnd, gridStep,
		promql.LowerOpts{Lowerers: promql.RangeLowerers{Rate: promql.NativeRateLowerer{
			Fallback: promql.FanoutRateLowerer{},
		}}})
	if err != nil {
		t.Fatalf("lower production rate: %v", err)
	}
	return plan
}

// nativeSpinePlan builds `<wrap>(RangeWindowNative)` on the canonical grid —
// the pure-native shape, with the wrapper chosen by the caller so both of
// UnpinSpine's two structurally different routes to the same node are covered.
func nativeSpinePlan(wrap func(chplan.Node) chplan.Node) chplan.Node {
	return wrap(&chplan.RangeWindowNative{
		Input:           leafScan(),
		Func:            "rate",
		Range:           unionArmRange,
		Step:            gridStep,
		Start:           gridStart,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	})
}

// TestUnpinSpine_ZeroesNativeBounds pins the slicer half on its own, because
// the failure it guards is INVISIBLE at the Decision seam: a native node left
// pinned makes every ReanchorRange call return ErrReanchorGridMismatch, which
// sliceAndDecide converts into a not-sliceable route-A Decision. The plan then
// looks like one the solver declined on structure rather than one whose slicer
// is broken, and no route-B assertion anywhere would notice.
//
// The three fixtures are the three structurally different ways UnpinSpine
// reaches the node, and they exercise DIFFERENT code:
//
//   - under a Project: the copy-on-write spine walk (unpinSpineCOW's own
//     RangeWindowNative arm);
//   - under an Aggregate, and inside the mixed union: the off-spine
//     descend-and-clone fallback (subtreeHasZeroableSpine must SEE the node,
//     then zeroSpineInPlace must zero it).
//
// A change that teaches only one of those paths passes the other fixtures, so
// covering one shape would leave half the slicer unpinned.
func TestUnpinSpine_ZeroesNativeBounds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		plan func() chplan.Node
	}{
		{
			name: "project-spine",
			plan: func() chplan.Node {
				return nativeSpinePlan(func(n chplan.Node) chplan.Node {
					return &chplan.Project{
						Input:       n,
						Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"}},
					}
				})
			},
		},
		{
			name: "aggregate-root",
			plan: func() chplan.Node {
				return nativeSpinePlan(func(n chplan.Node) chplan.Node {
					return &chplan.Aggregate{
						Input:    n,
						AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
					}
				})
			},
		},
		{name: "mixed-union", plan: nativeUnionPlan},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plan := tc.plan()
			snapshot := chplan.CloneNode(plan)

			base := UnpinSpine(plan)

			found := 0
			chplan.Walk(base, func(n chplan.Node) bool {
				v, ok := n.(*chplan.RangeWindowNative)
				if !ok {
					return true
				}
				found++
				if !v.Start.IsZero() || !v.End.IsZero() {
					t.Errorf("UnpinSpine left the native node pinned at [%v,%v]; ReanchorRange "+
						"only fills an unpinned or already-on-target node, so every slice "+
						"would abort", v.Start, v.End)
				}
				return true
			})
			if found != 1 {
				t.Fatalf("expected exactly 1 RangeWindowNative in the unpinned view, found %d", found)
			}
			if !plan.Equal(snapshot) {
				t.Error("UnpinSpine mutated the caller's plan")
			}
		})
	}
}

// TestPlan_NativeGridGuardsRefuse pins the bound-pinning and grid-prediction
// gates the native spine acquired the moment it became routable (issue #2117).
//
// Before it was routable these checks were unnecessary — a node that never
// slices cannot slice onto wrong bounds — so they are entirely new surface, and
// their failure mode is the worst kind: an @-pinned node re-anchored onto the
// request grid answers a DIFFERENT query than the one asked, silently and with
// plausible numbers.
//
// Each fixture below is refused by exactly one gate, and the reason token is
// asserted rather than just the route bit: a refusal that lands on the right
// answer via the wrong gate is a gate that will stop working when its neighbour
// moves.
func TestPlan_NativeGridGuardsRefuse(t *testing.T) {
	t.Parallel()

	native := func(mut func(*chplan.RangeWindowNative)) chplan.Node {
		n := &chplan.RangeWindowNative{
			Input:           leafScan(),
			Func:            "rate",
			Range:           unionArmRange,
			Step:            gridStep,
			Start:           gridStart,
			End:             gridEnd,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		}
		mut(n)
		return n
	}

	for _, tc := range []struct {
		name       string
		plan       chplan.Node
		wantReason string
	}{
		{
			// The @-modifier shape: bounds pinned somewhere the request grid
			// does not predict. Re-anchoring would move them onto the request
			// grid and answer the un-pinned query.
			name: "at-pinned bounds diverge from the request grid",
			plan: native(func(n *chplan.RangeWindowNative) {
				n.Start = gridStart.Add(-24 * time.Hour)
				n.End = gridEnd.Add(-24 * time.Hour)
			}),
			wantReason: ReasonGridMismatch,
		},
		{
			// No lowering emits an unpinned native node; if one appeared, its
			// grid is whatever the re-anchor invents, which is not a query
			// anybody asked for.
			name: "both bounds unpinned",
			plan: native(func(n *chplan.RangeWindowNative) {
				n.Start = time.Time{}
				n.End = time.Time{}
			}),
			wantReason: ReasonInstant,
		},
		{
			name: "half-pinned bounds",
			plan: native(func(n *chplan.RangeWindowNative) {
				n.Start = time.Time{}
			}),
			wantReason: ReasonInstant,
		},
		{
			// Nested one level under a routable spine: the grid predicted there
			// is widened by the outer window's Range, so a native node pinned at
			// the OUTER grid does not sit where its own depth predicts.
			name:       "nested under a routable window",
			plan:       geomRoutableSpine(native(func(*chplan.RangeWindowNative) {})),
			wantReason: ReasonGridMismatch,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d, routed := (&Planner{Cfg: autoCfg()}).Plan(tc.plan, oomMeta())
			if routed {
				t.Fatalf("plan routed B (K=%d); a native node off the predicted grid must not be "+
					"re-anchored onto it", d.K)
			}
			if d.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", d.Reason, tc.wantReason)
			}
			// Eligible bypasses the cost thresholds, so it is the stricter seam:
			// a refusal that only holds because the plan was too cheap to shard
			// is not a correctness gate.
			if ed, eligible := (&Planner{Cfg: autoCfg()}).Eligible(tc.plan, oomMeta()); eligible {
				t.Errorf("Eligible routed the plan (K=%d) — the threshold-free seam is where a "+
					"missing correctness gate shows up first", ed.K)
			}
		})
	}
}

// nestedNativeStep is the inner native grid's own resolution in the
// commensurability fixture below, chosen so that it does NOT divide the
// quantum the slicer emits for the outer grid (m = ceil(241/8) = 31 anchors,
// 465s) — the whole point being that the shard boundary would move the inner
// grid's phase.
const nestedNativeStep = 7 * time.Second

// nestedNativeRange is the inner native window's [range]. It is distinct from
// the outer spine's own range so a cumulative-D assertion can tell the two
// contributions apart.
const nestedNativeRange = time.Minute

// nestedNativeSpine builds a routable outer RangeWindow over a native inner
// grid pinned EXACTLY where that depth predicts, at the given inner resolution.
// It is the only shape that reaches the native arm's end-phase bookkeeping: a
// nested native node whose bounds diverge at all is refused by the
// grid-prediction gate long before the quantum is considered.
func nestedNativeSpine(innerStep time.Duration) chplan.Node {
	outer := geomRoutableSpine(nil)
	outer.Input = &chplan.RangeWindowNative{
		Input:           leafScan(),
		Func:            "rate",
		Range:           nestedNativeRange,
		Step:            innerStep,
		Start:           gridStart.Add(-geomOuterSpineRange),
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	return outer
}

// TestPlan_NestedNativeGridConstrainsTheSliceQuantum pins the last gate the
// native spine needed once it became routable: a NESTED native grid is
// generated from the node's own bounds with no epoch snap, so shifting a shard
// boundary by a non-multiple of that grid's resolution moves its phase and the
// per-shard anchor set stops being a subset of the unsliced one.
//
// The two fixtures differ ONLY in the inner resolution, so the refusal cannot
// be resting on anything else in the shape: at a resolution that divides the
// emitted quantum the same plan routes, and at one that does not it is refused
// as incommensurate.
func TestPlan_NestedNativeGridConstrainsTheSliceQuantum(t *testing.T) {
	t.Parallel()

	t.Run("incommensurate inner resolution is refused", func(t *testing.T) {
		t.Parallel()

		plan := nestedNativeSpine(nestedNativeStep)
		d, eligible := (&Planner{Cfg: autoCfg()}).Eligible(plan, oomMeta())
		if eligible {
			t.Fatalf("a nested native grid at %s routed under a %s quantum (K=%d); the shard "+
				"boundary moves its phase", nestedNativeStep, gridStep, d.K)
		}
		if d.Reason != ReasonIncommensurate {
			t.Errorf("reason = %q, want %q — any other token means the plan was refused by a "+
				"different gate and the quantum check is untested", d.Reason, ReasonIncommensurate)
		}
	})

	t.Run("commensurate inner resolution routes", func(t *testing.T) {
		t.Parallel()

		plan := nestedNativeSpine(gridStep)
		d, eligible := (&Planner{Cfg: autoCfg()}).Eligible(plan, oomMeta())
		if !eligible {
			t.Fatalf("the control shape must route, else the refusal above proves nothing about "+
				"the inner resolution; reason=%q", d.Reason)
		}
		if want := geomOuterSpineRange + nestedNativeRange; d.CumulativeD != want {
			t.Errorf("CumulativeD = %s, want %s — the nested native node was never measured, so "+
				"the walk did not reach it and neither fixture says anything about it",
				d.CumulativeD, want)
		}
	})
}

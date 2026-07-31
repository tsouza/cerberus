package solver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The per-anchor lookback each carrier kind gets in the table below. Every value
// is DISTINCT so a extractor that stamped one hard-coded duration onto every
// kind — or that read the wrong kind's field — cannot satisfy the table.
const (
	geomRangeWindowRange   = 5 * time.Minute
	geomRangeLWRLookback   = 4 * time.Minute
	geomNativeRange        = 3 * time.Minute
	geomResampleLookback   = 2 * time.Minute
	geomFanoutLookback     = 6 * time.Minute
	geomAbsentOverTimeSpan = 7 * time.Minute
)

// geomAnchors is the anchor count of the shared canonical grid: gridStart..gridEnd
// is 1h at gridStep = 15s, end-inclusive, so N = 3600/15 + 1.
const geomAnchors = 241

// carrierCase is one row of the seven-kind extraction table.
type carrierCase struct {
	// kind is the chplan struct name, used to prove the table covers the whole
	// GridCarrier set (see TestCarrierGeometry_CoversEveryCarrier).
	kind string
	// plan builds a plan whose ONLY grid carrier is this kind, on the canonical
	// gridStart..gridEnd / gridStep grid.
	plan func() chplan.Node
	// wantD is the cumulative per-anchor lookback the extractor must record.
	wantD time.Duration
	// wantFanout is wantD / gridStep.
	wantFanout int64
	// reanchorable says whether chplan.ReanchorRange re-grids this kind. It is
	// the ONLY thing that decides whether the plan may be sliced; every row
	// records real features either way. A row with reanchorable == false must
	// still be refused, which is the correctness half of this table.
	reanchorable bool
}

// carrierCases is the full GridCarrier table. Every kind builds its own leaf
// plan on the same grid so the expected N / OuterRange are shared and only the
// per-anchor lookback varies.
func carrierCases() []carrierCase {
	return []carrierCase{
		{
			kind: "RangeWindow",
			plan: func() chplan.Node {
				return &chplan.RangeWindow{
					Input:           leafScan(),
					Func:            "rate",
					Range:           geomRangeWindowRange,
					Step:            gridStep,
					OuterRange:      gridEnd.Sub(gridStart),
					Start:           gridStart,
					End:             gridEnd,
					TimestampColumn: "TimeUnix",
					ValueColumn:     "Value",
					GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
				}
			},
			wantD:        geomRangeWindowRange,
			wantFanout:   int64(geomRangeWindowRange / gridStep),
			reanchorable: true,
		},
		{
			kind: "RangeLWR",
			plan: func() chplan.Node {
				return &chplan.RangeLWR{
					Input:         leafScan(),
					Start:         gridStart,
					End:           gridEnd,
					Step:          gridStep,
					Lookback:      geomRangeLWRLookback,
					MetricNameCol: "MetricName",
					AttributesCol: "Attributes",
					TimestampCol:  "TimeUnix",
					ValueCol:      "Value",
				}
			},
			wantD:        geomRangeLWRLookback,
			wantFanout:   int64(geomRangeLWRLookback / gridStep),
			reanchorable: true,
		},
		{
			// The regression this whole table exists for: a natively-lowered
			// rate() is the plan's ONLY carrier, so an extractor blind to it
			// records nothing at all.
			kind: "RangeWindowNative",
			plan: func() chplan.Node {
				return &chplan.RangeWindowNative{
					Input:           leafScan(),
					Func:            "rate",
					Range:           geomNativeRange,
					Step:            gridStep,
					Start:           gridStart,
					End:             gridEnd,
					TimestampColumn: "TimeUnix",
					ValueColumn:     "Value",
					GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
				}
			},
			wantD:        geomNativeRange,
			wantFanout:   int64(geomNativeRange / gridStep),
			reanchorable: false,
		},
		{
			kind: "RangeWindowResample",
			plan: func() chplan.Node {
				return &chplan.RangeWindowResample{
					Input:         leafScan(),
					Start:         gridStart,
					End:           gridEnd,
					Step:          gridStep,
					Lookback:      geomResampleLookback,
					MetricNameCol: "MetricName",
					AttributesCol: "Attributes",
					TimestampCol:  "TimeUnix",
					ValueCol:      "Value",
				}
			},
			wantD:        geomResampleLookback,
			wantFanout:   int64(geomResampleLookback / gridStep),
			reanchorable: false,
		},
		{
			kind: "RangeBucketFanout",
			plan: func() chplan.Node {
				return &chplan.RangeBucketFanout{
					Input:          leafScan(),
					Start:          gridStart,
					End:            gridEnd,
					Step:           gridStep,
					Lookback:       geomFanoutLookback,
					GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
					GroupByAliases: []string{"Attributes"},
					AggFuncs: []chplan.AggFunc{
						{Name: "argMax", Args: []chplan.Expr{&chplan.ColumnRef{Name: "BucketCounts"}}, Alias: "BucketCounts"},
					},
					AnchorAlias:  "anchor_ts",
					TimestampCol: "TimeUnix",
				}
			},
			wantD:        geomFanoutLookback,
			wantFanout:   int64(geomFanoutLookback / gridStep),
			reanchorable: false,
		},
		{
			kind: "AbsentOverTime",
			plan: func() chplan.Node {
				return &chplan.AbsentOverTime{
					Input:            leafScan(),
					Range:            geomAbsentOverTimeSpan,
					Start:            gridStart,
					End:              gridEnd,
					Step:             gridStep,
					TimestampColumn:  "TimeUnix",
					ValueColumn:      "Value",
					MetricNameColumn: "MetricName",
					AttributesColumn: "Attributes",
				}
			},
			wantD:        geomAbsentOverTimeSpan,
			wantFanout:   int64(geomAbsentOverTimeSpan / gridStep),
			reanchorable: false,
		},
		{
			// A bare anchor axis (time(), vector(scalar), the date functions).
			// It reads no samples, so a zero cumulative_d is the CORRECT
			// extraction here, not a missed one — and the anchor grid it does
			// carry must still be recorded, which is what separates it from an
			// unmeasured plan.
			kind: "StepGrid",
			plan: func() chplan.Node {
				return &chplan.StepGrid{Start: gridStart, End: gridEnd, Step: gridStep}
			},
			wantD:        0,
			wantFanout:   0,
			reanchorable: false,
		},
	}
}

// TestCarrierGeometry_ExtractsFeaturesForEveryKind is the table the briefing
// calls for: every [chplan.GridCarrier] kind must yield a real (n_anchors,
// outer_range, cumulative_d, fanout) readout on the Decision that reaches the
// calibration corpus.
//
// Reverting the fix breaks this on four rows at once — RangeWindowNative,
// RangeWindowResample, RangeBucketFanout and StepGrid all collapse to
// NAnchors=0 / OuterRange=0 / CumulativeD=0, because walkNode had no arm that
// recorded them and every feature is produced by that walk. AbsentOverTime
// collapses on CumulativeD alone for the same reason.
//
// It also pins the correctness half in the same rows: measuring a carrier must
// never make it sliceable. Only the two re-anchorable families may be eligible;
// the other five are refused with a non-zero grid, which is exactly the state
// the corpus needs (what it cost, and why we declined).
func TestCarrierGeometry_ExtractsFeaturesForEveryKind(t *testing.T) {
	t.Parallel()

	wantOuterRange := gridEnd.Sub(gridStart)

	for _, tc := range carrierCases() {
		tc := tc
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()

			plan := tc.plan()

			// Derive the request grid from the plan exactly as engine.classify
			// does, so the fixture cannot smuggle in a grid the plan does not
			// actually carry.
			start, end, step := GridOf(plan)
			if step != gridStep || !start.Equal(gridStart) || !end.Equal(gridEnd) {
				t.Fatalf("fixture grid drifted: GridOf = (%s, %s, %s), want (%s, %s, %s)",
					start, end, step, gridStart, gridEnd, gridStep)
			}
			meta := RequestMeta{Lang: LangPromQL, Start: start, End: end, Step: step}

			p := &Planner{Cfg: autoCfg()}
			d, routed := p.Plan(plan, meta)

			if d.NAnchors != geomAnchors {
				t.Errorf("NAnchors = %d, want %d — a carrier the walk cannot see contributes "+
					"no anchor count and the corpus row is unreplayable", d.NAnchors, geomAnchors)
			}
			if d.OuterRange != wantOuterRange {
				t.Errorf("OuterRange = %s, want %s", d.OuterRange, wantOuterRange)
			}
			if d.CumulativeD != tc.wantD {
				t.Errorf("CumulativeD = %s, want %s", d.CumulativeD, tc.wantD)
			}
			if d.Fanout != tc.wantFanout {
				t.Errorf("Fanout = %d, want %d", d.Fanout, tc.wantFanout)
			}
			if d.Step != gridStep {
				t.Errorf("Step = %s, want %s", d.Step, gridStep)
			}

			// Extraction succeeded, so the reason must never be the
			// nothing-was-measured token.
			if d.Reason == ReasonExtractionFailed {
				t.Errorf("reason = %q on a plan whose carrier WAS measured", d.Reason)
			}

			// The correctness gate: only ReanchorRange's own families slice.
			if !tc.reanchorable {
				if routed {
					t.Errorf("plan routed B (K=%d) on a carrier ReanchorRange clones VERBATIM — "+
						"every shard would emit the original full-grid bounds", d.K)
				}
				if d.Reason != ReasonNotSliceable {
					t.Errorf("reason = %q, want %q — a non-re-anchorable carrier is refused by the "+
						"slice-invariance gates, not by a cost threshold", d.Reason, ReasonNotSliceable)
				}
				if !chplanRefusesSlicing(plan) {
					t.Errorf("plan is slice-invariant end-to-end; the refusal above is then resting on " +
						"the spine flag alone with no structural backstop")
				}
			}
		})
	}
}

// chplanRefusesSlicing reports whether the plan is refused by chplan's own
// slice-invariance registry OR by the solver's routable-spine flag. It is the
// belt-and-braces read of the two independent correctness gates a
// non-re-anchorable carrier must trip, checked from the test so a change that
// silently dropped one of them is visible.
func chplanRefusesSlicing(plan chplan.Node) bool {
	refused := false
	chplan.Walk(plan, func(n chplan.Node) bool {
		if !chplan.IsSliceInvariant(n) {
			refused = true
			return false
		}
		if gc, ok := n.(chplan.GridCarrier); ok {
			if geom, known := carrierGeometryOf(gc); !known || !geom.reanchorable {
				if _, _, step := gc.EvalGrid(); step > 0 {
					refused = true
					return false
				}
			}
		}
		return true
	})
	return refused
}

// TestCarrierGeometry_CoversEveryCarrier closes the table above against
// chplan's own definition of the carrier set, so a new grid-bearing node cannot
// be added upstream while this package keeps extracting nothing from it.
//
// It recognises a carrier the same way chplan's completeness ratchet does — by
// the (Start time.Time, End time.Time, Step time.Duration) field signature —
// rather than by importing a list, because the registry there is unexported and
// a second hand-kept list is exactly the drift this test exists to catch.
func TestCarrierGeometry_CoversEveryCarrier(t *testing.T) {
	t.Parallel()

	declared := gridDeclaringChplanStructs(t)
	if len(declared) == 0 {
		t.Fatal("scanned internal/chplan and found no eval-grid structs; the scan is broken, " +
			"not the package")
	}

	covered := make(map[string]bool, len(carrierCases()))
	for _, tc := range carrierCases() {
		covered[tc.kind] = true
	}

	for _, name := range declared {
		if !covered[name] {
			t.Errorf("chplan.%s declares an eval grid but has no row in carrierCases — the solver "+
				"would extract an all-zero feature vector from every plan rooted on it", name)
		}
		delete(covered, name)
	}
	for name := range covered {
		t.Errorf("carrierCases has a row for chplan.%s, which no longer declares an eval grid — "+
			"drop the row", name)
	}
}

// TestCarrierGeometryOf_UnclassifiableCarrierFailsClosed pins the default arm:
// a carrier value the type switch cannot classify yields no geometry, and the
// recorder then contributes NO features and forces route A instead of inventing
// a cost.
//
// chplan.Node is sealed by an unexported marker method, so a carrier kind
// outside chplan cannot be constructed here at all — the untyped nil carrier is
// the only value that reaches the default arm from this package, and it stands
// in for the kind-added-upstream case that
// TestCarrierGeometry_CoversEveryCarrier independently rules out.
func TestCarrierGeometryOf_UnclassifiableCarrierFailsClosed(t *testing.T) {
	t.Parallel()

	if _, ok := carrierGeometryOf(nil); ok {
		t.Fatal("carrierGeometryOf claimed a geometry for a carrier it cannot classify; a guessed " +
			"geometry would route a plan on numbers nobody derived")
	}

	sig := &signals{}
	(&Planner{Cfg: autoCfg()}).recordGridCarrier(nil, 0, sig)
	if sig.sawGridCarrier {
		t.Error("sawGridCarrier set for a carrier whose geometry was never derived — the resulting " +
			"corpus row would claim measured features it does not have")
	}
	if !sig.sawNonRangeWindowSpine {
		t.Error("sawNonRangeWindowSpine not set; an unmeasurable carrier must fail CLOSED to route A")
	}
	if sig.cumulativeD != 0 || sig.outerN != 0 || sig.hasWindow {
		t.Errorf("unclassifiable carrier contributed features (D=%s N=%d hasWindow=%v); refusing to "+
			"guess means recording nothing", sig.cumulativeD, sig.outerN, sig.hasWindow)
	}
}

// TestCarrierGeometry_NativeIsMeasuredButNeverSliceable is the correctness pin
// for the exact hazard widening extraction creates: making the solver SEE a
// carrier must not make the slicer TAKE it.
//
// The subject is the shape the whole defect was found on — a natively-lowered
// rate() whose RangeWindowNative is the plan's only grid carrier. Before the
// fix it extracted nothing; after it, the Decision carries a real cost grid.
// Both halves are asserted together, because either one alone is satisfiable by
// a wrong change: extracting nothing still "never slices", and registering the
// node slice-invariant would extract features while shipping wrong answers
// (ReanchorRange clones the node verbatim, so every shard would evaluate the
// ORIGINAL full grid and the merged result would be the query repeated K times
// rather than partitioned).
func TestCarrierGeometry_NativeIsMeasuredButNeverSliceable(t *testing.T) {
	t.Parallel()

	var native chplan.Node
	for _, tc := range carrierCases() {
		if tc.kind == "RangeWindowNative" {
			native = tc.plan()
		}
	}
	if native == nil {
		t.Fatal("carrierCases lost its RangeWindowNative row; this pin has no subject")
	}

	// The dominant real shape: sum(rate(...)) with the rate lowered natively.
	plan := &chplan.Aggregate{
		Input:    native,
		AggFuncs: []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}}},
	}

	start, end, step := GridOf(plan)
	meta := RequestMeta{Lang: LangPromQL, Start: start, End: end, Step: step}

	// Measurement half.
	d, routed := (&Planner{Cfg: autoCfg()}).Plan(plan, meta)
	if d.NAnchors == 0 || d.OuterRange == 0 || d.CumulativeD == 0 || d.Fanout == 0 {
		t.Errorf("natively-lowered rate() extracted N=%d OuterRange=%s D=%s F=%d — an all-zero "+
			"cost grid is the extractor blindness this pin exists to catch",
			d.NAnchors, d.OuterRange, d.CumulativeD, d.Fanout)
	}

	// Correctness half, at both public seams. Eligible deliberately bypasses
	// the cost thresholds, so it is the stricter of the two.
	if routed {
		t.Errorf("Plan routed a RangeWindowNative spine (K=%d)", d.K)
	}
	if ed, eligible := (&Planner{Cfg: autoCfg()}).Eligible(plan, meta); eligible {
		t.Errorf("Eligible routed a RangeWindowNative spine (K=%d) — the threshold-free seam is "+
			"where an over-wide extraction shows up first", ed.K)
	}

	// And the structural backstop underneath both: the node is NOT registered
	// slice-invariant, and must stay that way until ReanchorRange learns its
	// grid. A change that "fixes" this test by registering it is the bug.
	if chplan.IsSliceInvariant(native) {
		t.Error("RangeWindowNative is registered slice-invariant, but chplan.ReanchorRange has no " +
			"arm for it — every shard would emit the original full-grid bounds")
	}
}

// geomOuterSpineRange is the per-anchor range of the routable outer window the
// placement fixtures below build around. It is distinct from every per-kind
// lookback above so an assertion on the cumulative D can tell the outer spine's
// contribution apart from the placed carrier's — i.e. prove the walk reached the
// carrier in that position rather than stopping at the wrapper.
const geomOuterSpineRange = 90 * time.Second

// geomRoutableSpine is a pinned matrix RangeWindow over inner, on the canonical
// grid. On its own (inner = leafScan) it is the routable shape: re-anchorable,
// both bounds pinned, sitting exactly on the predicted grid — so it contributes
// NO refusal of its own, and whatever refuses a plan built from it is a property
// of what was placed around it.
func geomRoutableSpine(inner chplan.Node) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:           inner,
		Func:            "sum_over_time",
		Range:           geomOuterSpineRange,
		Step:            gridStep,
		OuterRange:      gridEnd.Sub(gridStart),
		Start:           gridStart,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// carrierPlacement is one position a carrier can occupy in a plan that is
// otherwise routable. The seven-kind table above only ever builds the carrier as
// the plan's ROOT and sole carrier; these are the positions where the fail-close
// is load-bearing rather than incidental.
type carrierPlacement struct {
	name string
	// wrap builds a plan in which carrier sits at this placement, beside or
	// beneath a spine that would route on its own.
	wrap func(carrier chplan.Node) chplan.Node
}

func carrierPlacements() []carrierPlacement {
	return []carrierPlacement{
		{
			// Depth 1: the subquery shape `<agg>_over_time(<inner>[1h:15s])`,
			// whose inner spine walkNode reaches one level down. This is the
			// placement the depth-0 table cannot reach — and the one where a
			// depth-scoped fail-close would let a non-re-anchorable carrier
			// through while every shard re-evaluated it over the ORIGINAL grid.
			name: "nested-under-routable-window",
			wrap: func(carrier chplan.Node) chplan.Node {
				return geomRoutableSpine(carrier)
			},
		},
		{
			// Beside, not below: a step-aligned vector-vector join whose left
			// arm alone would route. The carrier is then neither the plan's
			// root nor its only carrier, so the outer grid is supplied by the
			// routable arm and the refusal cannot be resting on the carrier
			// being the sole source of the cost grid.
			name: "beside-routable-spine",
			wrap: func(carrier chplan.Node) chplan.Node {
				return &chplan.VectorJoin{
					Left:             geomRoutableSpine(leafScan()),
					Right:            carrier,
					Op:               chplan.OpDiv,
					Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
					StepAligned:      true, // range mode: not the now64 instant shape
					MetricNameColumn: "MetricName",
					AttributesColumn: "Attributes",
					TimestampColumn:  "TimeUnix",
					ValueColumn:      "Value",
				}
			},
		},
	}
}

// nonReanchorableCases is the subset of the table whose carriers ReanchorRange
// clones VERBATIM — the rows whose refusal is a correctness requirement rather
// than a cost judgement.
func nonReanchorableCases(t *testing.T) []carrierCase {
	t.Helper()

	var out []carrierCase
	for _, tc := range carrierCases() {
		if !tc.reanchorable {
			out = append(out, tc)
		}
	}
	if len(out) == 0 {
		t.Fatal("no non-re-anchorable rows in carrierCases; the fail-close this file pins has no " +
			"subject left and every assertion below is vacuous")
	}
	return out
}

// TestCarrierGeometry_NonReanchorableCarrierRefusedAtEveryPlacement is the
// placement half of the correctness proof.
//
// The seven-kind table puts every carrier at depth 0 as the plan's sole carrier,
// where the refusal is over-determined: the carrier is also the only thing
// supplying the outer grid. These fixtures put the same carriers where they are
// NOT the plan's grid source — one level down a routable spine, and beside one —
// so the fail-close is exercised in the positions where it is the only barrier.
//
// The hazard is concrete: chplan.ReanchorRange has no arm for these kinds, so
// slicer.ReanchorRange CloneNode's them verbatim and every shard would evaluate
// the carrier over the ORIGINAL full grid. The merged result is then the
// carrier's full-window answer repeated K times rather than partitioned.
// Narrowing recordGridCarrier's fail-close to `depth == 0` passes the whole
// seven-row table and fails here.
func TestCarrierGeometry_NonReanchorableCarrierRefusedAtEveryPlacement(t *testing.T) {
	t.Parallel()

	cases := nonReanchorableCases(t)
	for _, pl := range carrierPlacements() {
		for _, tc := range cases {
			t.Run(pl.name+"/"+tc.kind, func(t *testing.T) {
				t.Parallel()

				plan := pl.wrap(tc.plan())

				// The wrapper pins the canonical grid, so the request the
				// planner sees is the same one the table uses.
				start, end, step := GridOf(plan)
				if step != gridStep || !start.Equal(gridStart) || !end.Equal(gridEnd) {
					t.Fatalf("fixture grid drifted: GridOf = (%s, %s, %s), want (%s, %s, %s)",
						start, end, step, gridStart, gridEnd, gridStep)
				}
				meta := RequestMeta{Lang: LangPromQL, Start: start, End: end, Step: step}

				d, routed := (&Planner{Cfg: autoCfg()}).Plan(plan, meta)
				if routed {
					t.Errorf("plan routed B (K=%d) with a %s at %s — ReanchorRange clones that "+
						"carrier verbatim, so every shard would evaluate it over the original "+
						"full grid", d.K, tc.kind, pl.name)
				}
				if d.Reason != ReasonNotSliceable {
					t.Errorf("reason = %q, want %q", d.Reason, ReasonNotSliceable)
				}

				// Eligible bypasses the cost thresholds, so it is the stricter
				// seam: a fail-close that only holds because the plan was too
				// cheap to bother slicing is not a fail-close.
				if ed, eligible := (&Planner{Cfg: autoCfg()}).Eligible(plan, meta); eligible {
					t.Errorf("Eligible routed a %s at %s (K=%d)", tc.kind, pl.name, ed.K)
				}

				// Non-vacuity: the carrier really was walked in this position.
				// Both placements contribute the routable spine's own range on
				// top of the carrier's per-anchor lookback.
				if wantD := geomOuterSpineRange + tc.wantD; d.CumulativeD != wantD {
					t.Errorf("CumulativeD = %s, want %s — the %s at %s was never measured, so the "+
						"refusal above proves nothing about that placement",
						d.CumulativeD, wantD, tc.kind, pl.name)
				}
			})
		}
	}
}

// TestCarrierGeometry_NestedRefusalRestsOnTheDepthFreeGate names the gate doing
// the work in the test above, for the kinds where only ONE gate can be.
//
// Two independent gates refuse a non-re-anchorable carrier: (1) chplan's
// slice-invariance registry, and (1b) recordGridCarrier's reanchorable
// fail-close. For RangeWindowNative / RangeWindowResample / AbsentOverTime the
// registry alone would refuse. But RangeBucketFanout and StepGrid ARE registered
// slice-invariant — per-node they genuinely are — so for those kinds gate (1) is
// silent and (1b) is the whole barrier. This test asserts that silence, so a
// change that weakened (1b) cannot be excused by "the registry catches it".
func TestCarrierGeometry_NestedRefusalRestsOnTheDepthFreeGate(t *testing.T) {
	t.Parallel()

	var soleBarrier []carrierCase
	for _, tc := range nonReanchorableCases(t) {
		if everyNodeSliceInvariant(tc.plan()) {
			soleBarrier = append(soleBarrier, tc)
		}
	}
	if len(soleBarrier) == 0 {
		t.Fatal("no non-re-anchorable carrier is registered slice-invariant, so this pin has no " +
			"subject; if the registry really lost them, the fixtures — not this assertion — are stale")
	}

	for _, tc := range soleBarrier {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()

			plan := geomRoutableSpine(tc.plan())
			if !everyNodeSliceInvariant(plan) {
				t.Fatalf("the %s fixture is not slice-invariant end-to-end; gate (1) would refuse "+
					"it and this test would no longer isolate the depth-free fail-close", tc.kind)
			}

			meta := RequestMeta{Lang: LangPromQL, Start: gridStart, End: gridEnd, Step: gridStep}
			d, routed := (&Planner{Cfg: autoCfg()}).Plan(plan, meta)
			if routed {
				t.Fatalf("a nested %s routed B (K=%d) — with the registry silent, the only thing "+
					"that can refuse it is the reanchorable fail-close in recordGridCarrier",
					tc.kind, d.K)
			}
			if d.Reason != ReasonNotSliceable {
				t.Errorf("reason = %q, want %q", d.Reason, ReasonNotSliceable)
			}
		})
	}
}

// everyNodeSliceInvariant reports whether chplan's registry admits every node in
// the plan — i.e. whether the solver's gate (1) stays silent on it.
func everyNodeSliceInvariant(plan chplan.Node) bool {
	ok := true
	chplan.Walk(plan, func(n chplan.Node) bool {
		if !chplan.IsSliceInvariant(n) {
			ok = false
			return false
		}
		return true
	})
	return ok
}

// TestClassify_NoCarrierRecordsExtractionFailed pins the reason token that
// keeps an unmeasured plan out of the calibration population.
//
// Reverting the gate makes this plan report below-threshold with an all-zero
// cost grid — the same row a genuinely trivial query produces. The two are then
// indistinguishable in the corpus, and every threshold fitted over the refused
// population is dragged toward zero by rows that were never measured.
func TestClassify_NoCarrierRecordsExtractionFailed(t *testing.T) {
	t.Parallel()

	// A carrier-free plan: matcher filter over a scan, nothing that owns a grid.
	plan := &chplan.Filter{
		Input: leafScan(),
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "m"},
		},
	}
	if _, _, step := GridOf(plan); step != 0 {
		t.Fatalf("fixture must carry no grid; GridOf reported step=%s", step)
	}

	// The request nonetheless declares a real range grid — the state a plan
	// whose carrier the walk cannot see would present.
	meta := oomMeta()

	d, routed := (&Planner{Cfg: autoCfg()}).Plan(plan, meta)
	if routed {
		t.Fatalf("an unmeasured plan routed (K=%d)", d.K)
	}
	if d.Reason != ReasonExtractionFailed {
		t.Errorf("reason = %q, want %q — a zero cost grid must say it was never measured, not "+
			"imply the plan is cheap", d.Reason, ReasonExtractionFailed)
	}
	if d.NAnchors != 0 || d.Fanout != 0 || d.OuterRange != 0 || d.CumulativeD != 0 {
		t.Errorf("extraction-failed row carries features N=%d F=%d OuterRange=%s D=%s; nothing was "+
			"measured, so nothing may be reported",
			d.NAnchors, d.Fanout, d.OuterRange, d.CumulativeD)
	}
}

// gridDeclaringChplanStructs parses internal/chplan and returns the names of
// every struct declaring the eval-grid field signature, sorted.
func gridDeclaringChplanStructs(t *testing.T) []string {
	t.Helper()

	const chplanDir = "../chplan"
	entries, err := os.ReadDir(chplanDir)
	if err != nil {
		t.Fatalf("read internal/chplan: %v", err)
	}

	fset := token.NewFileSet()
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(chplanDir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			if declaresEvalGrid(st) {
				names = append(names, ts.Name.Name)
			}
			return true
		})
	}
	sort.Strings(names)
	return names
}

// declaresEvalGrid reports whether a struct carries all three eval-grid fields
// with the exact types that make it a grid owner: Start/End as time.Time and
// Step as time.Duration.
func declaresEvalGrid(st *ast.StructType) bool {
	var start, end, step bool
	for _, f := range st.Fields.List {
		typeName := exprTypeName(f.Type)
		for _, nm := range f.Names {
			switch {
			case nm.Name == "Start" && typeName == "time.Time":
				start = true
			case nm.Name == "End" && typeName == "time.Time":
				end = true
			case nm.Name == "Step" && typeName == "time.Duration":
				step = true
			}
		}
	}
	return start && end && step
}

// exprTypeName renders a qualified type expression (pkg.Name) back to source
// form; anything else returns "" and therefore never matches a grid field.
func exprTypeName(e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + sel.Sel.Name
}

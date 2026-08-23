package solver

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// [chplan.RangeBucketWindowSlide] answers TRUE to
// [chplan.GridCarrier.AnchorGridDivides] — see [chplan.RangeBucketWindowSlide]'s
// own AnchorGridDivides doc and carrierGeometryOf's *chplan.RangeBucketWindowSlide
// case in planner.go — and that answer is honest for the same reason
// [chplan.RangeBucketGridNative]'s is (see range_bucket_grid_native_route_test.go):
// the node's UNION ALL source materialises series x anchors sentinel rows before
// its single sliding window function ever runs, so its intermediate really is
// linear in the grid width.
//
// What makes the honest answer SAFE is, again, that the solver never reaches
// the question for this kind: the plan is refused before any threshold reads
// the geometry, by the SAME two independent refusals pinned for
// RangeBucketGridNative:
//
//  1. the kind is deliberately absent from chplan.IsSliceInvariant's registry —
//     no slice-invariance proof has been argued for it, so a plan carrying one
//     is route-A only;
//  2. carrierGeometryOf reports reanchorable=false for it — chplan.ReanchorRange
//     has no arm that re-grids it, so recordGridCarrier's fail-closed branch
//     fires on the live grid.
//
// Why this needs a test at all rather than a comment. `AnchorGridDivides()
// == true` is the solver's licence to SHARD a carrier. Registering a kind as
// slice-invariant is a one-line change that reads like obvious enablement;
// the moment someone makes it here, the brake this kind relies on is gone
// and the carrier is a step away from being sharded with the licence
// already granted — exactly the regression #2396 shipped to fix for the
// carrier kinds that came before this one. This file turns the two
// refusals from prose into failures, mirroring
// range_bucket_grid_native_route_test.go's own pattern for its sibling node
// (the SUM-fold anchor-injection mechanism this branch adds, vs. that
// node's native rate ladder — same two-refusal brake, same reason it must
// not be assumed to still hold just because the sibling's test does).
//
// Note what is NOT the fix if one of these ever goes red: flipping
// AnchorGridDivides() to false would silence it while telling the next reader
// something untrue about the node's cost shape. The refusals are the brake.

// windowSlideCarrier builds a RangeBucketWindowSlide over the canonical
// eligibility grid (see gridStart / gridEnd / gridStep), with the same 5m
// window the routing tests use for every other carrier (ratio 5m/15s = 20,
// clearing windowSlideMinLookbackStepRatio with headroom), so its geometry
// is comparable to theirs rather than a shape of its own.
func windowSlideCarrier() *chplan.RangeBucketWindowSlide {
	return &chplan.RangeBucketWindowSlide{
		Input:             leafScan(),
		Start:             gridStart,
		End:               gridEnd,
		Step:              gridStep,
		Range:             5 * time.Minute,
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		GroupByAliases:    []string{"Attributes"},
		AnchorAlias:       "anchor_ts",
		TimestampCol:      "TimeUnix",
		BucketCountsCol:   "BucketCounts",
		ExplicitBoundsCol: "ExplicitBounds",
	}
}

// windowSlideSpine wraps that carrier in the across-series Aggregate the
// classic-histogram lowering always puts above a window stage's bare node
// (mirroring bucketGridNativeSpine exactly — the merge-across-series step is
// the same shape for either window-stage lowering, only the carrier kind
// underneath differs), so the plan under test has the shape production
// emits rather than a bare node.
func windowSlideSpine() chplan.Node {
	return &chplan.Aggregate{
		Input:    windowSlideCarrier(),
		GroupBy:  []chplan.Expr{&chplan.ColumnRef{Name: "anchor_ts"}},
		AggFuncs: []chplan.AggFunc{{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: "BucketCounts"}}}},
	}
}

// TestRangeBucketWindowSlide_NeverRoutesUnderAuto pins the OUTCOME: a plan
// whose spine carries the window-slide anchor-injection ladder stays on
// route A under ModeAuto, and stays there for a STRUCTURAL reason.
//
// Asserting the reason, not just the route, is what keeps this from passing
// vacuously — same distinction TestRangeBucketGridNative_NeverRoutesUnderAuto
// draws for its own sibling node. Removing both gates named in this file's
// doc comment does NOT by itself make this plan route: a third refusal
// stands behind them, because a single-pass anchor-injection aggregate
// reports fanout 1 (carrierGeometry.singlePass) and so falls under
// ModeAuto's MinFanout, yielding ReasonBelowThreshold. That is a CALIBRATED
// number, not a structural gate — one Config change away from moving — so a
// carrier held only by it is a carrier one tuning decision away from being
// sharded with the AnchorGridDivides() licence already granted. Pinning
// ReasonNotSliceable is what makes this test notice that the structural
// gates went away, instead of quietly resting on the threshold that
// happened to catch it next.
//
// The control half pins the other direction: a RangeLWR over the IDENTICAL
// grid and lookback DOES route (that is TestPlan_RangeLWRSpineRoutes' own
// subject), so a refusal here is never just a grid that was too small to be
// interesting.
func TestRangeBucketWindowSlide_NeverRoutesUnderAuto(t *testing.T) {
	t.Parallel()

	p := &Planner{Cfg: autoCfg()}

	if _, routed := p.Plan(lwrSpine(0), oomMeta()); !routed {
		t.Fatalf("control: a RangeLWR spine over the same grid must route, or this test's " +
			"negative half proves nothing about the carrier kind")
	}

	d, routed := p.Plan(windowSlideSpine(), oomMeta())
	if routed {
		t.Fatalf("a RangeBucketWindowSlide spine routed (K=%d, reason=%q). Its "+
			"AnchorGridDivides() answers TRUE — the solver's licence to shard — and that "+
			"answer is only sound while the plan is refused before any threshold reads it. "+
			"Restore the refusal (see this file's doc comment); do NOT flip "+
			"AnchorGridDivides() to false, which would be untrue about the node's cost shape.",
			d.K, d.Reason)
	}
	if d.Reason != ReasonNotSliceable {
		t.Errorf("a RangeBucketWindowSlide spine is refused with reason %q, want %q. The plan "+
			"is still on route A, but no longer because of a structural gate — see this test's "+
			"doc comment for why resting on a calibrated threshold is not the same guarantee.",
			d.Reason, ReasonNotSliceable)
	}
}

// TestRangeBucketWindowSlide_SlicingRefusedAtBothGates pins the two
// MECHANISMS the outcome above rests on, separately, so a change that
// removes one of them fails here naming which — rather than surviving on
// the other until it too is removed and the carrier silently becomes
// shardable.
func TestRangeBucketWindowSlide_SlicingRefusedAtBothGates(t *testing.T) {
	t.Parallel()

	n := windowSlideCarrier()

	if chplan.IsSliceInvariant(n) {
		t.Errorf("chplan.RangeBucketWindowSlide is registered slice-invariant. It answers TRUE " +
			"to AnchorGridDivides(), so registering it here hands the solver a licence to shard " +
			"a carrier whose slice-invariance nobody has argued. Registering it needs the proof " +
			"+ §Parity lanes chplan's sliceInvariantKinds doc demands, AND a chplan.ReanchorRange " +
			"arm, AND a re-derivation of whether AnchorGridDivides() is still the right answer.")
	}

	geom, ok := carrierGeometryOf(n)
	if !ok {
		t.Fatalf("carrierGeometryOf does not enumerate chplan.RangeBucketWindowSlide — the " +
			"solver would extract an all-zero feature vector from every plan rooted on it")
	}
	if geom.reanchorable {
		t.Errorf("carrierGeometryOf reports reanchorable=true for chplan.RangeBucketWindowSlide, " +
			"but chplan.ReanchorRange has no arm that re-grids it: a routed shard would evaluate " +
			"the FULL grid K times over instead of its own slice")
	}
	if !geom.singlePass {
		t.Errorf("carrierGeometryOf reports singlePass=false for chplan.RangeBucketWindowSlide. " +
			"The anchor-injection UNION ALL feeds exactly ONE sliding window function over the " +
			"whole partition, not a per-anchor fan-out — singlePass=true is what makes this " +
			"kind's fanout report 1 and fall under ModeAuto's MinFanout, the third (calibrated, " +
			"non-structural) refusal TestRangeBucketWindowSlide_NeverRoutesUnderAuto's doc " +
			"comment names.")
	}

	if !n.AnchorGridDivides() {
		t.Errorf("AnchorGridDivides() answers false for chplan.RangeBucketWindowSlide. That is " +
			"not a safety improvement, it is a false statement about the node: the anchor-" +
			"injection aggregate materialises series x anchors sentinel rows before its window " +
			"function runs, so its intermediate IS linear in the grid width. The brake on this " +
			"kind is the two refusals above.")
	}
}

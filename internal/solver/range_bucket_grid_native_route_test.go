package solver

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// [chplan.RangeBucketGridNative] answers TRUE to
// [chplan.GridCarrier.AnchorGridDivides], and that answer is HONEST: the native
// ladder materialises one Array of N grid points per (series, `le` rung), so its
// intermediate really is linear in the grid width, exactly as
// [chplan.RangeWindowGridNative]'s is.
//
// Until #2677 that licence was held back by two structural refusals — the kind
// was absent from chplan.IsSliceInvariant's registry, and carrierGeometryOf
// reported it non-re-anchorable. Both are now ADMISSIONS rather than refusals,
// each with its own argued proof:
//
//  1. chplan.IsSliceInvariant registers the kind, with the stage-by-stage
//     scan-lower-bound-independence argument its registry doc demands;
//  2. chplan.ReanchorRange carries a *RangeBucketGridNative arm that re-grids
//     (Start, End) and widens the input spine by Offset+Range, so a routed
//     shard evaluates its OWN sub-grid.
//
// Why that inversion was the point. This node's memory grows with the anchor
// count and with the in-window raw rows, so a wide window busts a single
// ClickHouse query's memory cap outright (#2677: a real 6h dashboard panel at
// 253 series / bucket width 68). Time-slicing divides both axes per shard, and
// it is the only relief that removes window width as a memory term rather than
// relocating the wall — every other lever (raising the density ceiling, cutting
// the per-row constant, downsampling the panel) moves the cliff without
// removing the growth.
//
// The tests below pin both admissions separately, so a change that withdraws
// one fails here naming which — rather than silently returning the carrier to
// route A and taking the wide-window relief with it.

// bucketGridNativeCarrier builds a RangeBucketGridNative over the canonical
// eligibility grid (see gridStart / gridEnd / gridStep), with the same 5m window
// the routing tests use for every other carrier, so its geometry is comparable
// to theirs rather than a shape of its own.
func bucketGridNativeCarrier() *chplan.RangeBucketGridNative {
	return &chplan.RangeBucketGridNative{
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

// bucketGridNativeSpine wraps that carrier in the across-series Aggregate the
// classic-histogram lowering always puts above it, so the plan under test has
// the shape production emits rather than a bare node.
func bucketGridNativeSpine() chplan.Node {
	return &chplan.Aggregate{
		Input:    bucketGridNativeCarrier(),
		GroupBy:  []chplan.Expr{&chplan.ColumnRef{Name: "anchor_ts"}},
		AggFuncs: []chplan.AggFunc{{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: "BucketCounts"}}}},
	}
}

// TestRangeBucketGridNative_SlicingAdmittedAtBothGates pins the two MECHANISMS
// the wide-window relief rests on, separately, so a change that withdraws one
// fails here naming which — rather than leaving the other in place and letting
// the carrier silently fall back to route A with no memory relief at all.
func TestRangeBucketGridNative_SlicingAdmittedAtBothGates(t *testing.T) {
	t.Parallel()

	n := bucketGridNativeCarrier()

	if !chplan.IsSliceInvariant(n) {
		t.Errorf("chplan.RangeBucketGridNative is NOT registered slice-invariant. Without that " +
			"registration the whole-plan walk refuses any plan carrying it, so a wide-window " +
			"classic-histogram quantile has no route-B relief and must fit one ClickHouse " +
			"query's memory cap — the #2677 failure. The registry entry carries the " +
			"stage-by-stage proof; restore it rather than working around this.")
	}

	geom, ok := carrierGeometryOf(n)
	if !ok {
		t.Fatalf("carrierGeometryOf does not enumerate chplan.RangeBucketGridNative — the solver " +
			"would extract an all-zero feature vector from every plan rooted on it")
	}
	if !geom.reanchorable {
		t.Errorf("carrierGeometryOf reports reanchorable=false for chplan.RangeBucketGridNative. " +
			"chplan.ReanchorRange DOES carry an arm that re-grids it, so reporting false here " +
			"strands the node on route A even though slicing it is sound and implemented.")
	}

	if !n.AnchorGridDivides() {
		t.Errorf("AnchorGridDivides() answers false for chplan.RangeBucketGridNative. That is not " +
			"a safety improvement, it is a false statement about the node: the native aggregate " +
			"materialises one Array of N grid points per (series, rung), so its intermediate IS " +
			"linear in the grid width — which is exactly why slicing it relieves memory.")
	}
}

// TestRangeBucketGridNative_ReanchorsOntoSubGrid pins that the ReanchorRange arm
// actually re-grids the node onto a shard's own sub-window and widens its input
// spine by the membership lookback — the property that makes a shard evaluate
// ITS OWN slice rather than the full grid K times over.
//
// It runs the node through UnpinSpine first, because that is the real slicer's
// own order (slicer.go): the lowering builds this node pinned at the full
// request grid, and ReanchorRange fills only a node that is unpinned or already
// on the target grid. Re-anchoring a still-pinned node onto a sub-grid is
// SUPPOSED to fail — that is the @-modifier guard — so a test that skipped the
// unpin would be asserting the wrong contract.
func TestRangeBucketGridNative_ReanchorsOntoSubGrid(t *testing.T) {
	t.Parallel()

	subStart := gridStart.Add(10 * time.Minute)
	subEnd := gridStart.Add(20 * time.Minute)

	unpinned := UnpinSpine(bucketGridNativeCarrier())
	if u, ok := unpinned.(*chplan.RangeBucketGridNative); !ok {
		t.Fatalf("UnpinSpine returned %T, want *chplan.RangeBucketGridNative", unpinned)
	} else if !u.Start.IsZero() || !u.End.IsZero() {
		t.Fatalf("UnpinSpine left the grid pinned (Start=%v End=%v); every slice would then "+
			"fail ErrReanchorGridMismatch and fall back to route A", u.Start, u.End)
	}

	out, err := chplan.ReanchorRange(unpinned, subStart, subEnd)
	if err != nil {
		t.Fatalf("ReanchorRange refused a RangeBucketGridNative onto a sub-grid: %v", err)
	}
	got, ok := out.(*chplan.RangeBucketGridNative)
	if !ok {
		t.Fatalf("ReanchorRange returned %T, want *chplan.RangeBucketGridNative", out)
	}
	if !got.Start.Equal(subStart) || !got.End.Equal(subEnd) {
		t.Errorf("re-anchored bounds are (%v, %v), want the shard's own (%v, %v) — a shard that "+
			"kept the full grid would evaluate every anchor K times over and defeat the point "+
			"of slicing", got.Start, got.End, subStart, subEnd)
	}
	// The original must be untouched: shards share the pre-slice tree.
	orig := bucketGridNativeCarrier()
	if !got.Input.Equal(orig.Input) {
		t.Errorf("re-anchoring rewrote the input subtree's identity; the arm must widen the " +
			"scan bound without changing what the input reads")
	}
}

// TestRangeBucketGridNative_EligibleForMemoDrivenRouting pins the relief path
// that actually answers #2677.
//
// The distinction this test exists to hold is subtle and load-bearing. Under
// ModeAuto's PREDICTIVE thresholds this plan still does not route: a
// single-pass grid aggregate reports fanout 1 (carrierGeometry.singlePass), so
// it falls under MinFanout and yields ReasonBelowThreshold. That is correct —
// the predictive proxy has no way to know this particular window is heavy.
//
// What changed is that Planner.Eligible — the re-derivation the failure-driven
// route memo runs after a real route-A resource failure — now ADMITS the plan
// and returns a sliced Decision. Eligible deliberately does not apply the
// ModeAuto cost thresholds (a measured OOM is stronger evidence than a static
// proxy), so once the structural gates admit the node, an observed memory
// failure is enough to shard the retry. That is the whole mechanism by which a
// wide-window classic-histogram quantile stops being a hard 422.
func TestRangeBucketGridNative_EligibleForMemoDrivenRouting(t *testing.T) {
	t.Parallel()

	p := &Planner{Cfg: autoCfg()}

	// Predictive path: still below threshold, by the singlePass fanout.
	d, routed := p.Plan(bucketGridNativeSpine(), oomMeta())
	if routed {
		t.Errorf("a RangeBucketGridNative spine routed under ModeAuto's predictive thresholds "+
			"(K=%d, reason=%q). Slicing it is sound, but its single-pass fanout of 1 is "+
			"genuinely below MinFanout — routing it predictively would be the threshold "+
			"drifting, not this fix.", d.K, d.Reason)
	}
	if d.Reason != ReasonBelowThreshold {
		t.Errorf("predictive refusal reason is %q, want %q. A structural reason here "+
			"(not-sliceable) would mean the slicing gates silently closed again and the "+
			"memo path below cannot fire either.", d.Reason, ReasonBelowThreshold)
	}

	// Memo path: admitted, sliced, and genuinely sharded.
	ed, eligible := p.Eligible(bucketGridNativeSpine(), oomMeta())
	if !eligible {
		t.Fatalf("Planner.Eligible refused a RangeBucketGridNative spine (reason=%q). This is "+
			"the re-derivation the failure-driven route memo runs after a real route-A "+
			"memory failure; refusing here means a wide-window classic-histogram quantile "+
			"has no relief and stays a hard 422 (#2677).", ed.Reason)
	}
	if ed.K < 2 {
		t.Errorf("Eligible produced K=%d, want >= 2 — one shard is route A with extra "+
			"machinery, and divides neither the anchor axis nor the raw-row axis", ed.K)
	}
	if ed.Reason != ReasonRouted {
		t.Errorf("Eligible returned reason %q, want %q", ed.Reason, ReasonRouted)
	}
}

package chsql

import "testing"

// sentinelReferenceGroups is test/perf/nightly/sentinels.go's own
// classic_histogram_quantile_by_route sentinel cardinality — 3,741 series x
// 12 rungs (11 finite ExplicitBounds + the Inf overflow rung) — the real
// production sample range_bucket_grid_native_bound.go's own axis1
// calibration has been anchored against since issue #2486. Duplicated here
// as a plain arithmetic constant, not imported, because test/perf/nightly
// is an integration package this internal package cannot depend on (and
// should not: this test exists to catch a REGRESSION in the CONSTANT below,
// not to exercise the sentinel itself).
const sentinelReferenceGroups = 3_741 * 12 // 44,892

// minSentinelGroupHeadroomAnchors is the minimum anchor count ANY
// deployment at (or modestly above) sentinelReferenceGroups must be able to
// query without tripping axis1 (maxRangeBucketGridNativeRows).
//
// Issue #2651's own real production incident is the reason this floor
// exists: a deployment at 3,962 series x 16 rungs = 63,392 groups (1.41x
// sentinelReferenceGroups) was hard-rejected at just 315 anchors — a
// completely ordinary ~5-6h/1m Grafana dashboard window — because the
// pre-#2651 maxRangeBucketGridNativeRows (20,000,000) had only 40% real
// margin below the genuine ClickHouse OOM cliff a fresh real-ClickHouse
// 25.9-alpine calibration found at that exact shape (safe through
// 33,534,368, OOM at 33,597,760 — see range_bucket_grid_native_bound.go's
// own "Issue #2651 recalibration" doc for the full measurement). 500 is
// chosen deliberately past that report's own 315-anchor failure point
// (roughly an 8h/1m window) while staying inside real, measured safe
// territory even at the SMALLER sentinel reference cardinality this test
// checks against — at the current 25,000,000 threshold,
// sentinelReferenceGroups alone supports 556 anchors of headroom (well past
// this floor), and the #2651 production shape itself supports ~394 (see
// this file's own doc — smaller than the sentinel's own headroom precisely
// because 63,392 > 44,892; this test intentionally checks the SENTINEL's
// own, already-lower, floor so it fires on ANY future recalibration that
// erodes real-world headroom, not only one keyed to a specific incident's
// own numbers).
const minSentinelGroupHeadroomAnchors = 500

// TestRangeBucketGridNativeBound_AxisOneHasSentinelHeadroom is a forward
// guard against issue #2651's own root cause recurring: a FUTURE
// recalibration of maxRangeBucketGridNativeRows (for an unrelated reason,
// e.g. tightening it back down after some other finding) that silently
// drops the guard's real capacity below what a deployment at the sentinel's
// own reference cardinality needs at an ordinary dashboard anchor count.
//
// Deliberately NOT chDB/real-ClickHouse-gated: this is pure plan-time
// arithmetic (the same division axis1's own throwIf performs internally),
// so it runs in every `go test -race ./...` invocation with no chDB
// dependency — catching a regression the moment the constant changes,
// rather than only when someone happens to run the chdb-tagged suite this
// package's other range_bucket_grid_native_bound_test.go tests need.
//
// This does NOT replace real-ClickHouse recalibration when the constant
// legitimately needs to move (see this file's own "Recalibrate by binary
// search..." doc) — it only catches an UNREVIEWED regression. A deliberate,
// evidence-backed lowering of maxRangeBucketGridNativeRows should update
// minSentinelGroupHeadroomAnchors in the same change, with the same kind of
// real-ClickHouse citation this file's header comment already requires.
func TestRangeBucketGridNativeBound_AxisOneHasSentinelHeadroom(t *testing.T) {
	got := maxRangeBucketGridNativeRows / sentinelReferenceGroups
	if got < minSentinelGroupHeadroomAnchors {
		t.Fatalf("maxRangeBucketGridNativeRows (%d) / sentinelReferenceGroups (%d) = %d anchors of "+
			"headroom, below minSentinelGroupHeadroomAnchors (%d) — issue #2651 found this guard's real "+
			"safety margin can be much thinner than the raw constant suggests (recalibrate against a real "+
			"ClickHouse per this file's own header doc before lowering maxRangeBucketGridNativeRows, and "+
			"update minSentinelGroupHeadroomAnchors deliberately alongside it if the new value is a "+
			"reviewed, evidence-backed choice rather than an accidental regression)",
			maxRangeBucketGridNativeRows, sentinelReferenceGroups, got, minSentinelGroupHeadroomAnchors)
	}
}

// sentinelReferenceDensityCostUnits is the real #2486/#2408 reference
// density point range_bucket_grid_native_bound.go's own "Density guard" doc
// has cited since issue #2523: test/perf/nightly's own
// classic_histogram_quantile_by_route sentinel shape (3,741 series x 12
// rungs x 60 anchors, 2,693,520 groups x anchors) at its own real
// ~350-samples/series reference density (1,309,350 raw rows, 11-finite-bound
// width) — `2,693,520 + 1,309,350 x 11^2 = 161,124,870`. Duplicated here as
// a plain arithmetic constant for the same reason sentinelReferenceGroups
// is: test/perf/nightly is an integration package this internal package
// cannot depend on, and this test exists to catch a regression in the
// CONSTANT below, not to exercise the sentinel itself.
const sentinelReferenceDensityCostUnits = 161_124_870

// minDensityHeadroomRatio is the minimum multiple of
// sentinelReferenceDensityCostUnits that maxRangeBucketGridNativeDensityUnits
// must stay above.
//
// Issue #2665's own real production incident is the reason this floor
// exists: the pre-#2665 85,000,000 ceiling left this exact reference point
// NO real headroom at all — 85,000,000 < 161,124,870, so the guard rejected
// the file's own established reference density outright — while real
// ClickHouse 25.9-alpine measurement WITH the spill settings every real
// production query gets (internal/engine/spill.go's applySpillSettings)
// found that same reference point genuinely safe at only 20.2% of the 1 GiB
// cap (see range_bucket_grid_native_bound.go's own "Issue #2665
// recalibration" doc). The new 400,000,000 ceiling gives this reference
// point 2.48x headroom; 2.0 is chosen as the floor a future recalibration
// must not silently erode below — comfortably under the real 2.48x this fix
// establishes, but still catching a regression that would put the guard
// back within a hair's breadth of rejecting the reference shape the way the
// old 85,000,000 ceiling did.
const minDensityHeadroomRatio = 2.0

// TestRangeBucketGridNativeBound_DensityAxisHasSentinelHeadroom is issue
// #2665's own forward guard, mirroring
// TestRangeBucketGridNativeBound_AxisOneHasSentinelHeadroom's own shape for
// axis1: a FUTURE recalibration of maxRangeBucketGridNativeDensityUnits (for
// an unrelated reason, e.g. tightening it back down after some other
// finding) that silently drops this guard's real capacity below what the
// file's own established reference density needs must fail loudly here,
// rather than only being caught the next time a real production deployment
// hits the guard the way #2665 itself did.
//
// Deliberately NOT chDB/real-ClickHouse-gated — pure plan-time arithmetic,
// same rationale as TestRangeBucketGridNativeBound_AxisOneHasSentinelHeadroom's
// own doc.
//
// This does NOT replace real-ClickHouse recalibration when the constant
// legitimately needs to move (see range_bucket_grid_native_bound.go's own
// "Recalibrate by binary search..." doc, WITH the spill settings this
// issue's own root-cause finding requires) — it only catches an UNREVIEWED
// regression. A deliberate, evidence-backed lowering of
// maxRangeBucketGridNativeDensityUnits should update minDensityHeadroomRatio
// in the same change, with the same kind of real-ClickHouse citation this
// file's header comment already requires.
func TestRangeBucketGridNativeBound_DensityAxisHasSentinelHeadroom(t *testing.T) {
	got := float64(maxRangeBucketGridNativeDensityUnits) / float64(sentinelReferenceDensityCostUnits)
	if got < minDensityHeadroomRatio {
		t.Fatalf("maxRangeBucketGridNativeDensityUnits (%d) / sentinelReferenceDensityCostUnits (%d) = "+
			"%.2fx headroom, below minDensityHeadroomRatio (%.2f) — issue #2665 found this guard's real "+
			"safety margin can leave the file's own established reference density with NO headroom at all "+
			"(recalibrate against a real ClickHouse, WITH the production spill settings, per this file's own "+
			"header doc before lowering maxRangeBucketGridNativeDensityUnits, and update "+
			"minDensityHeadroomRatio deliberately alongside it if the new value is a reviewed, "+
			"evidence-backed choice rather than an accidental regression)",
			maxRangeBucketGridNativeDensityUnits, sentinelReferenceDensityCostUnits, got, minDensityHeadroomRatio)
	}
}

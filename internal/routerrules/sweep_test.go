package routerrules

import (
	"context"
	"testing"
)

// nominalSweepAxes is the reference grid: each knob swept independently around
// the p95 / min-support-5 / nominal-prevalence operating point, on a fixed seed.
func nominalSweepAxes() SweepAxes {
	return SweepAxes{
		Watermarks:        []float64{0.50, 0.75, 0.90, 0.95, 0.99},
		MinSupports:       []int{1, 3, 5, 8, 12},
		Prevalences:       []float64{0.5, 1.0, 2.0},
		NominalWatermark:  0.95,
		NominalMinSupport: 5,
		NominalPrevalence: 1.0,
		Seed:              1,
	}
}

// TestDegradationSweep prints the sensitivity surface and asserts its qualitative
// shape: detection degrades the way the design predicts as each knob moves off
// the nominal, and severe pathologies are NEVER lost anywhere on the grid.
func TestDegradationSweep(t *testing.T) {
	cat := loadCatalogT(t)
	ax := nominalSweepAxes()
	pts, err := RunSweep(context.Background(), cat, ax)
	if err != nil {
		t.Fatalf("run sweep: %v", err)
	}
	t.Logf("router-rules degradation sweep (seed=%d):\n%s", ax.Seed, FormatSweepTable(pts))

	// 1) Severe pathologies are caught at EVERY operating point. A severe class
	//    sits far past its watermark, so no reasonable tuning should miss it; if
	//    this breaks, a rule has genuinely regressed, not merely become
	//    conservative.
	for _, p := range pts {
		if p.SevereRecall < 1.0 {
			t.Errorf("severe pathology missed at wm=%.2f msup=%d prev=%.2f: severe recall=%.3f",
				p.WatermarkPctile, p.MinSupport, p.Prevalence, p.SevereRecall)
		}
	}

	// 2) The watermark precision/recall tradeoff is monotone in the expected
	//    direction: loosening the watermark cannot IMPROVE precision (it admits
	//    more of the healthy body as false positives). Compare the loosest and
	//    nominal watermark points.
	loose := pointAt(t, pts, 0.50, 5, 1.0)
	nominal := pointAt(t, pts, 0.95, 5, 1.0)
	if loose.Overall.Precision >= nominal.Overall.Precision {
		t.Errorf("loosening the watermark to 0.50 should hurt precision vs nominal 0.95: loose=%.3f nominal=%.3f",
			loose.Overall.Precision, nominal.Overall.Precision)
	}

	// 3) The min_support floor trades recall for precision: a tiny floor (1)
	//    admits thin false-positive classes, so its precision is strictly worse
	//    than the nominal floor.
	tinyFloor := pointAt(t, pts, 0.95, 1, 1.0)
	if tinyFloor.Overall.Precision >= nominal.Overall.Precision {
		t.Errorf("a min_support floor of 1 should admit thin false positives vs nominal floor 5: floor1=%.3f nominal=%.3f",
			tinyFloor.Overall.Precision, nominal.Overall.Precision)
	}

	// 4) An over-tight watermark (0.99) starts losing the marginal class set —
	//    the design's stated tradeoff (precision over recall as you tighten). It
	//    must lose SOME marginal recall relative to nominal but keep severe
	//    recall perfect (already asserted in 1).
	tight := pointAt(t, pts, 0.99, 5, 1.0)
	if tight.MarginalRecall >= nominal.MarginalRecall {
		t.Errorf("over-tight watermark 0.99 should lose marginal recall vs nominal 0.95: tight=%.3f nominal=%.3f",
			tight.MarginalRecall, nominal.MarginalRecall)
	}

	// 5) At nominal, precision and recall are both perfect — the corpus is
	//    well-separated, so the catalog should make no errors there. This pins the
	//    sweet spot the other axes degrade away from.
	if nominal.Overall.Precision != 1.0 || nominal.Overall.Recall != 1.0 {
		t.Errorf("nominal operating point should be perfect, got precision=%.3f recall=%.3f",
			nominal.Overall.Precision, nominal.Overall.Recall)
	}

	// 6) A rare deployment (prevalence < nominal) starves its marginal pathologies
	//    below the support floor, so marginal recall drops while severe recall
	//    stays perfect. This proves the prevalence axis actually moves a metric —
	//    the old size clamp pinned every prevalence to an identical corpus, leaving
	//    the axis inert.
	rare := pointAt(t, pts, 0.95, 5, 0.5)
	if rare.MarginalRecall >= nominal.MarginalRecall {
		t.Errorf("rare prevalence 0.50 should lose marginal recall vs nominal 1.00: rare=%.3f nominal=%.3f",
			rare.MarginalRecall, nominal.MarginalRecall)
	}
	if rare.SevereRecall != 1.0 {
		t.Errorf("severe recall must stay perfect even at rare prevalence 0.50: got %.3f", rare.SevereRecall)
	}

	// 7) Inert-axis guard: every swept axis must move SOME metric. If an axis's
	//    entire grid collapses to one identical score, the axis is dead weight
	//    (the prevalence regression that triggered this redo). This guard fails
	//    loudly so a future clamp/grid change can't silently neuter an axis.
	assertAxisNotInert(t, "watermark", pts, ax, func(p SweepPoint) (float64, bool) {
		return p.WatermarkPctile, p.MinSupport == ax.NominalMinSupport && p.Prevalence == ax.NominalPrevalence
	})
	assertAxisNotInert(t, "min_support", pts, ax, func(p SweepPoint) (float64, bool) {
		return float64(p.MinSupport), p.WatermarkPctile == ax.NominalWatermark && p.Prevalence == ax.NominalPrevalence
	})
	assertAxisNotInert(t, "prevalence", pts, ax, func(p SweepPoint) (float64, bool) {
		return p.Prevalence, p.WatermarkPctile == ax.NominalWatermark && p.MinSupport == ax.NominalMinSupport
	})
}

// scoreFingerprint is the tuple of measured scores at one operating point. Two
// grid points sharing a fingerprint are indistinguishable — the mark of an inert
// axis when they sit on the same swept axis at different knob values.
type scoreFingerprint struct {
	tp, fp, fn                   int
	marginalRecall, severeRecall float64
	precision, recall            float64
}

func fingerprint(p SweepPoint) scoreFingerprint {
	return scoreFingerprint{
		tp: p.Overall.TP, fp: p.Overall.FP, fn: p.Overall.FN,
		marginalRecall: p.MarginalRecall, severeRecall: p.SevereRecall,
		precision: p.Overall.Precision, recall: p.Overall.Recall,
	}
}

// assertAxisNotInert fails if EVERY point on the named axis (selected by sel,
// which returns the axis knob value and whether the point lies on that axis with
// the other knobs nominal) shares one identical score fingerprint. An axis whose
// whole grid collapses to a single score moves no metric and is dead weight —
// the prevalence regression this guard exists to catch. (A saturated plateau
// where SOME but not all points match is fine; the axis still bends elsewhere.)
func assertAxisNotInert(t *testing.T, axis string, pts []SweepPoint, _ SweepAxes, sel func(SweepPoint) (float64, bool)) {
	t.Helper()
	type knobPoint struct {
		knob float64
		fp   scoreFingerprint
	}
	var axisPts []knobPoint
	for _, p := range pts {
		if knob, on := sel(p); on {
			axisPts = append(axisPts, knobPoint{knob: knob, fp: fingerprint(p)})
		}
	}
	if len(axisPts) < 2 {
		t.Fatalf("inert-axis guard: %s axis has %d grid points, need at least 2 to detect inertness", axis, len(axisPts))
	}
	allSame := true
	for _, kp := range axisPts[1:] {
		if kp.fp != axisPts[0].fp {
			allSame = false
			break
		}
	}
	if allSame {
		t.Errorf("inert %s axis: all %d grid points produce identical scores %+v — the axis moves no metric across its entire range",
			axis, len(axisPts), axisPts[0].fp)
	}
}

// pointAt returns the swept point matching the three knobs, failing if absent.
func pointAt(t *testing.T, pts []SweepPoint, wm float64, ms int, prev float64) SweepPoint {
	t.Helper()
	for _, p := range pts {
		if p.WatermarkPctile == wm && p.MinSupport == ms && p.Prevalence == prev {
			return p
		}
	}
	t.Fatalf("no swept point at wm=%.2f msup=%d prev=%.2f", wm, ms, prev)
	return SweepPoint{}
}

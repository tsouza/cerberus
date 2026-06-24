package routerrules

import (
	"context"
	"testing"
)

// Regression floors for router-rules detection. These NUMERIC thresholds live in
// test code by design (the no-numbers invariant governs only the shipped
// catalog YAML). They are the contract that a future catalog or generator change
// must not silently erode: if a rule is weakened, a watermark formula drifts, or
// the corpus stops exercising a rule, one of these floors trips.
//
// The floors are deliberately set BELOW the measured nominal (precision/recall
// 1.0) so normal run-to-run determinism has headroom, while still catching a
// real regression (a rule that stops firing, or starts over-firing on healthy
// classes).
const (
	// nominalF1Floor: at the nominal operating point the catalog must score at
	// least this overall F1. Measured nominal is 1.0; the floor leaves margin.
	nominalF1Floor = 0.95
	// nominalPrecisionFloor / nominalRecallFloor: nominal per-axis floors.
	nominalPrecisionFloor = 0.95
	nominalRecallFloor    = 0.95
	// saneRangeRecallFloor: across the operationally sane tuning range (watermark
	// 0.90–0.99, min_support 3–8, prevalence 0.5–2.0) recall must stay at least
	// this high — detection must not collapse just because a deployment tunes a
	// little off-nominal.
	saneRangeRecallFloor = 0.90
	// saneRangePrecisionFloor: across the SAME sane range precision must stay at
	// least this high — the rules must not flood an operator with false positives
	// under reasonable tuning. (Loosening the watermark to 0.50 deliberately
	// breaks this; the sane range starts at 0.90, where the healthy body no
	// longer crosses the watermark.)
	saneRangePrecisionFloor = 0.80
	// severeRecallFloor: severe pathologies must be caught at every sane point.
	severeRecallFloor = 1.0
)

// TestRegressionFloorNominal pins the nominal scorecard above the stated floors.
func TestRegressionFloorNominal(t *testing.T) {
	cat := loadCatalogT(t)
	corpus := GenerateBenchCorpus(nominalBenchParams())
	m, err := ScoreCatalog(context.Background(), cat, benchConfig(), corpus)
	if err != nil {
		t.Fatalf("score catalog: %v", err)
	}
	if m.Overall.F1 < nominalF1Floor {
		t.Errorf("nominal overall F1 %.3f below floor %.3f:\n%s", m.Overall.F1, nominalF1Floor, FormatMetricsTable(m))
	}
	if m.Overall.Precision < nominalPrecisionFloor {
		t.Errorf("nominal overall precision %.3f below floor %.3f", m.Overall.Precision, nominalPrecisionFloor)
	}
	if m.Overall.Recall < nominalRecallFloor {
		t.Errorf("nominal overall recall %.3f below floor %.3f", m.Overall.Recall, nominalRecallFloor)
	}

	// Every rule that is labeled in the corpus must score a positive F1 — a rule
	// that scores 0 is one that never fired on its planted pathology (a silenced
	// rule), the single most important regression to catch per-rule.
	for _, r := range m.PerRule {
		if r.TP+r.FN == 0 {
			continue // rule has no labeled positives in the corpus
		}
		if r.F1 == 0 {
			t.Errorf("rule %q has labeled positives but F1=0 — it detected none of them", r.Rule)
		}
	}
}

// TestRegressionFloorSaneRange pins recall + precision floors across the
// operationally sane TUNING range (watermark + min_support, at nominal
// prevalence), and severe-recall at 1.0 everywhere including off-nominal
// prevalence.
//
// Prevalence is held at nominal for the overall recall/precision floors on
// purpose: a rare deployment legitimately starves its marginal pathologies below
// the support floor (that is the modelled behaviour the prevalence axis now
// exercises — see pathologySize), so a prevalence-driven drop in overall recall
// is expected degradation, not a regression. SevereRecall, by contrast, must
// stay perfect at every prevalence — a severe pathology is never lost merely
// because the deployment is rare.
func TestRegressionFloorSaneRange(t *testing.T) {
	cat := loadCatalogT(t)
	ax := SweepAxes{
		Watermarks:        []float64{0.90, 0.95, 0.99},
		MinSupports:       []int{3, 5, 8},
		Prevalences:       []float64{0.5, 1.0, 2.0},
		NominalWatermark:  0.95,
		NominalMinSupport: 5,
		NominalPrevalence: 1.0,
		Seed:              1,
	}
	pts, err := RunSweep(context.Background(), cat, ax)
	if err != nil {
		t.Fatalf("run sweep: %v", err)
	}
	for _, p := range pts {
		// Overall recall/precision floors apply across the tuning knobs at nominal
		// prevalence; off-nominal prevalence is allowed to degrade overall recall.
		if p.Prevalence == ax.NominalPrevalence {
			if p.Overall.Recall < saneRangeRecallFloor {
				t.Errorf("recall %.3f below sane-range floor %.3f at wm=%.2f msup=%d prev=%.2f",
					p.Overall.Recall, saneRangeRecallFloor, p.WatermarkPctile, p.MinSupport, p.Prevalence)
			}
			if p.Overall.Precision < saneRangePrecisionFloor {
				t.Errorf("precision %.3f below sane-range floor %.3f at wm=%.2f msup=%d prev=%.2f",
					p.Overall.Precision, saneRangePrecisionFloor, p.WatermarkPctile, p.MinSupport, p.Prevalence)
			}
		}
		// Severe recall stays perfect everywhere, including off-nominal prevalence.
		if p.SevereRecall < severeRecallFloor {
			t.Errorf("severe recall %.3f below floor %.3f at wm=%.2f msup=%d prev=%.2f",
				p.SevereRecall, severeRecallFloor, p.WatermarkPctile, p.MinSupport, p.Prevalence)
		}
	}
}

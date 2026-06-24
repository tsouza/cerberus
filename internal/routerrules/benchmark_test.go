package routerrules

import (
	"context"
	"testing"
)

// benchConfig is the nominal config the benchmark scores the catalog at: a p95
// watermark on both percentile knobs and a min_rows_per_class matching the
// generator's benchNominalMinSupport, so the planted marginal classes sit one
// row above the firing floor.
func benchConfig() BenchConfig {
	return BenchConfig{
		"router_rules.watermark_percentile":    "0.95",
		"router_rules.cumulative_d_percentile": "0.95",
		"router_rules.min_rows_per_class":      "5",
		"query.max_memory_bytes":               "1073741824",
		"query.max_samples":                    "50000000",
	}
}

// nominalBenchParams is the benchmark's reference distribution: a fixed seed for
// reproducibility and the nominal class sizes.
func nominalBenchParams() BenchParams {
	return BenchParams{Seed: 1, MinSupport: 5}
}

func loadCatalogT(t *testing.T) *Catalog {
	t.Helper()
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return cat
}

// TestBenchmarkScorecard runs the catalog over the nominal labeled corpus and
// prints the full precision/recall/F1 table. It measures DETECTION FIDELITY — how
// consistently each rule fires on the pathologies a synthetic, self-labeled
// corpus plants for it — not real-world rule effectiveness. The corpus labels are
// derived from the same p95-watermark model the rules' thresholds resolve against,
// so a perfect score proves the rules detect what the model says they should and
// guards against detection regressions; it does NOT prove the rules catch real
// production incidents. It is the source of the numbers reported in the PR.
func TestBenchmarkScorecard(t *testing.T) {
	cat := loadCatalogT(t)
	corpus := GenerateBenchCorpus(nominalBenchParams())
	m, err := ScoreCatalog(context.Background(), cat, benchConfig(), corpus)
	if err != nil {
		t.Fatalf("score catalog: %v", err)
	}
	t.Logf("router-rules detection fidelity on %d rows / %d labeled classes (seed=%d):\n%s",
		len(corpus.Rows), len(corpus.Classes), nominalBenchParams().Seed, FormatMetricsTable(m))

	// Sanity: every rule that is labeled in the corpus must appear in the
	// scorecard, and the overall must have measured at least one true positive
	// (a catalog that detects nothing is a broken benchmark, not a passing one).
	if m.Overall.TP == 0 {
		t.Fatalf("overall TP is 0 — the benchmark detected nothing:\n%s", FormatMetricsTable(m))
	}
}

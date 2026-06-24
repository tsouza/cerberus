package routerrules

import (
	"context"
	"fmt"
	"strings"
)

// This file is the degradation sweep: it scores the catalog over a grid of
// operating points (watermark percentile, min_support, pathology prevalence)
// and reports how detection holds up as each knob moves away from the nominal.
// It turns the single nominal scorecard into a sensitivity surface — the answer
// to "how robust is detection when the deployment is tuned differently or the
// pathology is rare?".

// SweepPoint is one operating point and its measured overall + marginal scores.
type SweepPoint struct {
	WatermarkPctile float64
	MinSupport      int
	Prevalence      float64

	Overall RuleMetrics
	// MarginalRecall is recall restricted to classes planted at SevMarginal — the
	// hardest true positives, the first to be lost as a knob tightens.
	MarginalRecall float64
	// SevereRecall is recall over SevSevere classes — these should stay caught
	// far longer than marginal ones.
	SevereRecall float64
}

// SweepAxes declares the grid to sweep. Each axis is varied independently
// around the nominal (the other two held at their nominal value), so the table
// reads as three one-dimensional sensitivity curves rather than a full product.
type SweepAxes struct {
	Watermarks  []float64
	MinSupports []int
	Prevalences []float64

	NominalWatermark  float64
	NominalMinSupport int
	NominalPrevalence float64
	Seed              int64
}

// RunSweep scores every operating point on the three one-dimensional axes and
// returns the points in a deterministic order (watermark curve, then
// min_support curve, then prevalence curve).
func RunSweep(ctx context.Context, cat *Catalog, ax SweepAxes) ([]SweepPoint, error) {
	var out []SweepPoint
	add := func(wm float64, ms int, prev float64) error {
		pt, err := scorePoint(ctx, cat, ax.Seed, wm, ms, prev)
		if err != nil {
			return err
		}
		out = append(out, pt)
		return nil
	}
	for _, wm := range ax.Watermarks {
		if err := add(wm, ax.NominalMinSupport, ax.NominalPrevalence); err != nil {
			return nil, err
		}
	}
	for _, ms := range ax.MinSupports {
		if err := add(ax.NominalWatermark, ms, ax.NominalPrevalence); err != nil {
			return nil, err
		}
	}
	for _, prev := range ax.Prevalences {
		if err := add(ax.NominalWatermark, ax.NominalMinSupport, prev); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scorePoint generates a corpus and scores the catalog at one operating point.
// The min_support knob is threaded into BOTH the generator (so marginal class
// sizes track the floor) and the catalog config (so the rule applies the floor).
func scorePoint(ctx context.Context, cat *Catalog, seed int64, wm float64, ms int, prev float64) (SweepPoint, error) {
	params := BenchParams{Seed: seed, MinSupport: ms, PathologyPrevalence: prev}
	corpus := GenerateBenchCorpus(params)
	cfg := BenchConfig{
		"router_rules.watermark_percentile":    formatNumeric(wm),
		"router_rules.cumulative_d_percentile": formatNumeric(wm),
		"router_rules.min_rows_per_class":      fmt.Sprintf("%d", ms),
		"query.max_memory_bytes":               "1073741824",
		"query.max_samples":                    "50000000",
	}
	src := corpus.AsCorpusSource()
	rep, err := NewEvaluator(cat, staticConfigLookup(cfg), src).
		Evaluate(ctx, EvalOptions{IncludeExperimental: true})
	if err != nil {
		return SweepPoint{}, err
	}
	m := scoreReport(rep, corpus)
	pt := SweepPoint{
		WatermarkPctile: wm, MinSupport: ms, Prevalence: prev,
		Overall:        m.Overall,
		MarginalRecall: severityRecall(rep, corpus, SevMarginal),
		SevereRecall:   severityRecall(rep, corpus, SevSevere),
	}
	return pt, nil
}

// severityRecall computes recall restricted to classes of one planted severity:
// of all (class, expected-rule) positive pairs at that severity, the fraction
// the catalog actually fired.
func severityRecall(rep *Report, corpus *BenchCorpus, sev PathologySeverity) float64 {
	fired := map[string]map[string]struct{}{}
	for _, f := range rep.Findings {
		id := matchClassID(f, corpus)
		if id == "" {
			continue
		}
		set := fired[id]
		if set == nil {
			set = map[string]struct{}{}
			fired[id] = set
		}
		set[f.RuleID] = struct{}{}
	}
	var want, got int
	for i := range corpus.Classes {
		c := &corpus.Classes[i]
		if c.Severity != sev {
			continue
		}
		for _, rule := range c.Expect {
			want++
			if set, ok := fired[classID(*c)]; ok {
				if _, hit := set[rule]; hit {
					got++
				}
			}
		}
	}
	if want == 0 {
		return 1 // no positives at this severity: vacuously perfect.
	}
	return float64(got) / float64(want)
}

// FormatSweepTable renders the sensitivity grid as an aligned text table.
func FormatSweepTable(points []SweepPoint) string {
	var sb strings.Builder
	header := fmt.Sprintf("%8s %4s %6s %5s %5s %5s %9s %9s %9s %9s\n",
		"WMARK", "MSUP", "PREV", "TP", "FP", "FN", "PRECISION", "RECALL", "REC.MARG", "REC.SEV")
	sb.WriteString(header)
	sb.WriteString(strings.Repeat("-", len(header)) + "\n")
	for _, p := range points {
		fmt.Fprintf(&sb, "%8.2f %4d %6.2f %5d %5d %5d %9.3f %9.3f %9.3f %9.3f\n",
			p.WatermarkPctile, p.MinSupport, p.Prevalence,
			p.Overall.TP, p.Overall.FP, p.Overall.FN,
			p.Overall.Precision, p.Overall.Recall, p.MarginalRecall, p.SevereRecall)
	}
	return sb.String()
}

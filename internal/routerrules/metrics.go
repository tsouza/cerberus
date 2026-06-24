package routerrules

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// This file is the quantified detection-fidelity measure for the router-rules
// catalog: it runs the catalog over a SYNTHETIC LABELED benchmark corpus and
// scores each rule's detection as precision / recall / F1 against the class-level
// ground truth, plus a micro-averaged overall. The corpus labels share provenance
// with the rules' p95-watermark thresholds, so the score measures detection
// CONSISTENCY and guards against regressions — it is not a claim about real-world
// rule effectiveness. It turns the binary fire/no-fire of the effectiveness
// fixture into a graded, regression-pinnable number.
//
// Ground truth is per CLASS (a group_by group), because that is the granularity
// a rule fires at after the min_support floor:
//   - true positive  (TP): a class labeled with rule R, on which R fired.
//   - false positive (FP): a class NOT labeled with R, on which R fired.
//   - false negative (FN): a class labeled with R, on which R did not fire.
// True negatives are not scored (precision/recall don't use them); the healthy
// majority's contribution shows up as the absence of false positives.

// RuleMetrics is one rule's confusion counts and derived scores over a corpus.
type RuleMetrics struct {
	Rule      string
	TP        int
	FP        int
	FN        int
	Precision float64
	Recall    float64
	F1        float64
}

// BenchmarkMetrics is the full scorecard: per-rule rows plus a micro-averaged
// overall (TP/FP/FN summed across rules, then scored once).
type BenchmarkMetrics struct {
	PerRule []RuleMetrics
	Overall RuleMetrics
}

// BenchConfig is the resolved config the benchmark scores the catalog at. It is
// a plain map so the harness and the CLI subcommand share one shape; numbers
// here are benchmark settings, not catalog thresholds.
type BenchConfig map[string]string

// ScoreCatalog runs the catalog over the corpus and scores every rule. It folds
// in experimental rules (the read-amplification detector is labeled in the
// corpus, so excluding it would understate recall) and returns the per-rule +
// overall metrics, sorted by rule id for a deterministic table.
func ScoreCatalog(ctx context.Context, cat *Catalog, cfg BenchConfig, corpus *BenchCorpus) (*BenchmarkMetrics, error) {
	src := corpus.AsCorpusSource()
	rep, err := NewEvaluator(cat, staticConfigLookup(cfg), src).
		Evaluate(ctx, EvalOptions{IncludeExperimental: true})
	if err != nil {
		return nil, err
	}
	return scoreReport(rep, corpus), nil
}

// scoreReport scores a finished report against the corpus ground truth. It is
// split out so the degradation sweep can score reports it already has without
// re-evaluating.
func scoreReport(rep *Report, corpus *BenchCorpus) *BenchmarkMetrics {
	// fired[rule] = set of class ids the rule fired on.
	fired := map[string]map[string]struct{}{}
	for _, f := range rep.Findings {
		id := matchClassID(f, corpus)
		if id == "" {
			// A finding that matches no labeled class is a false positive against
			// a synthetic class id, so it still counts: bucket it under a unique
			// key so it is never silently a true positive.
			id = "unlabeled:" + f.RuleID + ":" + classOfFinding(f)
		}
		set := fired[f.RuleID]
		if set == nil {
			set = map[string]struct{}{}
			fired[f.RuleID] = set
		}
		set[id] = struct{}{}
	}

	// labeled[rule] = set of class ids labeled positive for the rule.
	labeled := map[string]map[string]struct{}{}
	allLabeledIDs := map[string]struct{}{}
	for _, c := range corpus.Classes {
		cid := classID(c)
		allLabeledIDs[cid] = struct{}{}
		for _, r := range c.Expect {
			set := labeled[r]
			if set == nil {
				set = map[string]struct{}{}
				labeled[r] = set
			}
			set[cid] = struct{}{}
		}
	}

	rules := ruleUniverse(fired, labeled)
	out := &BenchmarkMetrics{}
	var sumTP, sumFP, sumFN int
	for _, rule := range rules {
		m := scoreRule(rule, fired[rule], labeled[rule], allLabeledIDs)
		out.PerRule = append(out.PerRule, m)
		sumTP += m.TP
		sumFP += m.FP
		sumFN += m.FN
	}
	out.Overall = finalize(RuleMetrics{Rule: "OVERALL (micro)", TP: sumTP, FP: sumFP, FN: sumFN})
	return out
}

// scoreRule computes one rule's confusion counts. A fired class id that is not a
// real labeled class id (an "unlabeled:" bucket) is always a false positive.
func scoreRule(rule string, fired, labeled, allLabeledIDs map[string]struct{}) RuleMetrics {
	m := RuleMetrics{Rule: rule}
	for id := range fired {
		if _, real := allLabeledIDs[id]; !real {
			m.FP++ // fired on a class that does not exist in ground truth
			continue
		}
		if _, want := labeled[id]; want {
			m.TP++
		} else {
			m.FP++ // fired on a real class that is not labeled for this rule
		}
	}
	for id := range labeled {
		if _, ok := fired[id]; !ok {
			m.FN++ // labeled positive but the rule stayed silent
		}
	}
	return finalize(m)
}

func finalize(m RuleMetrics) RuleMetrics {
	if m.TP+m.FP > 0 {
		m.Precision = float64(m.TP) / float64(m.TP+m.FP)
	}
	if m.TP+m.FN > 0 {
		m.Recall = float64(m.TP) / float64(m.TP+m.FN)
	}
	if m.Precision+m.Recall > 0 {
		m.F1 = 2 * m.Precision * m.Recall / (m.Precision + m.Recall)
	}
	return m
}

// matchClassID resolves the labeled class a finding fired on by matching the
// finding's group-key values against each class's dimensions. A rule only
// group_bys a subset of dimensions, so the match uses exactly the keys present
// in the finding: shape_id (or normalized_query_hash for the slow rule) plus
// language uniquely identify a planted class.
func matchClassID(f Finding, corpus *BenchCorpus) string {
	for i := range corpus.Classes {
		c := &corpus.Classes[i]
		if classMatchesFinding(c, f.GroupKey) {
			return classID(*c)
		}
	}
	return ""
}

// classMatchesFinding reports whether every dimension the finding keys on agrees
// with the class. Keys the finding does not carry are not constrained.
func classMatchesFinding(c *LabeledClass, gk map[string]string) bool {
	if v, ok := gk["language"]; ok && v != c.Language {
		return false
	}
	if v, ok := gk["shape_id"]; ok && v != c.ShapeID {
		return false
	}
	if v, ok := gk["decision_reason"]; ok && v != c.DecisionReason {
		return false
	}
	if v, ok := gk["normalized_query_hash"]; ok && v != formatNumeric(float64(c.QueryHash)) {
		return false
	}
	// At least one identifying dimension (shape_id or query hash) must be present
	// and have matched, or the match is vacuous.
	_, hasShape := gk["shape_id"]
	_, hasHash := gk["normalized_query_hash"]
	return hasShape || hasHash
}

// ruleUniverse is the sorted union of rules that fired or are labeled, so a rule
// that never fires (all FN) and a rule that only false-positives both appear.
func ruleUniverse(fired, labeled map[string]map[string]struct{}) []string {
	seen := map[string]struct{}{}
	for r := range fired {
		seen[r] = struct{}{}
	}
	for r := range labeled {
		seen[r] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		// Drop the synthetic unlabeled bucket keys: they are class ids, not rule
		// ids, and never appear as map keys here, but guard anyway.
		if strings.HasPrefix(r, "unlabeled:") {
			continue
		}
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func classOfFinding(f Finding) string {
	keys := make([]string, 0, len(f.GroupKey))
	for k := range f.GroupKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + f.GroupKey[k]
	}
	return strings.Join(parts, ",")
}

// staticConfigLookup adapts a BenchConfig map to the ConfigLookup the resolver
// consumes.
func staticConfigLookup(cfg BenchConfig) ConfigLookup {
	return func(key string) (string, bool) {
		v, ok := cfg[key]
		return v, ok
	}
}

// FormatMetricsTable renders the scorecard as an aligned text table. It is the
// human-facing output of both the go-test harness and the CLI subcommand.
func FormatMetricsTable(m *BenchmarkMetrics) string {
	var sb strings.Builder
	header := fmt.Sprintf("%-36s %5s %5s %5s %9s %9s %9s\n",
		"RULE", "TP", "FP", "FN", "PRECISION", "RECALL", "F1")
	sb.WriteString(header)
	sb.WriteString(strings.Repeat("-", len(header)) + "\n")
	for _, r := range m.PerRule {
		sb.WriteString(formatMetricRow(r))
	}
	sb.WriteString(strings.Repeat("-", len(header)) + "\n")
	sb.WriteString(formatMetricRow(m.Overall))
	return sb.String()
}

func formatMetricRow(r RuleMetrics) string {
	return fmt.Sprintf("%-36s %5d %5d %5d %9.3f %9.3f %9.3f\n",
		r.Rule, r.TP, r.FP, r.FN, r.Precision, r.Recall, r.F1)
}

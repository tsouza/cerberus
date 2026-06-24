package routerrules

import (
	"context"
	"testing"
)

// staticConfig is a config lookup over a fixed map, standing in for the
// deployment config + --param overrides the CLI assembles.
func staticConfig(m map[string]string) ConfigLookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func seedSource(t *testing.T) CorpusSource {
	t.Helper()
	return NewJSONLCorpusSource("testdata/seed.jsonl", 0)
}

// evalConfig resolves every config-kind param the embedded catalog needs. The
// percentile is 0.5 (median) so the seeded fixture's watermarks land on
// predictable values; min_rows_per_class is 1 so no class is dropped.
func evalConfig() ConfigLookup {
	return staticConfig(map[string]string{
		"router_rules.watermark_percentile":    "0.5",
		"router_rules.cumulative_d_percentile": "0.5",
		"router_rules.min_rows_per_class":      "1",
		"query.max_memory_bytes":               "1073741824",
		"query.max_samples":                    "5000000",
	})
}

func TestEvaluateEmbeddedCatalogFindings(t *testing.T) {
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	ev := NewEvaluator(cat, evalConfig(), seedSource(t))
	report, err := ev.Evaluate(context.Background(), EvalOptions{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// Index findings by (rule, class) for assertions.
	type key struct{ rule, class string }
	got := map[key]Finding{}
	for _, f := range report.Findings {
		got[key{f.RuleID, classOf(f.GroupKey)}] = f
	}

	want := []struct {
		rule    string
		class   string
		support int64
	}{
		// --- shipped 7-rule baseline (catalogVersion 1) ---------------------
		{"oom_on_route_a", "language=promql,shape_id=cerb:sum", 2},
		{"oom_on_route_a", "language=traceql,shape_id=trc:spans", 1},
		{"route_a_memory_near_cap", "language=promql,shape_id=cerb:sum", 2},
		{"route_a_high_fanout_should_shard", "language=logql,shape_id=cerb:rate", 2},
		{"route_a_timeout_should_shard", "language=logql,shape_id=cerb:rate", 1},
		{"route_a_timeout_should_shard", "language=traceql,shape_id=trc:spans", 1},
		{"route_a_hit_sample_budget", "language=logql,shape_id=cerb:rate", 1},
		{"route_b_overshard_low_fanout", "language=promql,shape_id=cerb:topk", 3},
		// route_a_slow_hot_shape now carries decision_reason in its group key
		// (the catalogVersion-2 one-line group_by amendment).
		{"route_a_slow_hot_shape", "decision_reason=below-threshold,language=promql,normalized_query_hash=11", 4},
		{"route_a_slow_hot_shape", "decision_reason=below-threshold,language=logql,normalized_query_hash=22", 2},
		{"route_a_slow_hot_shape", "decision_reason=instant,language=traceql,normalized_query_hash=55", 2},
		// --- catalogVersion 2: N1 failure_cluster_by_reason -----------------
		{"failure_cluster_by_reason", "decision_reason=below-threshold,language=promql,shape_id=cerb:sum", 2},
		{"failure_cluster_by_reason", "decision_reason=below-threshold,language=logql,shape_id=cerb:rate", 1},
		{"failure_cluster_by_reason", "decision_reason=not-sliceable,language=traceql,shape_id=trc:compare", 2},
		{"failure_cluster_by_reason", "decision_reason=instant,language=traceql,shape_id=trc:spans", 2},
		// --- N2 route_b_still_failing ---------------------------------------
		{"route_b_still_failing", "decision_reason=not-sliceable,language=traceql,shape_id=trc:compare", 2},
		// --- N3 cerberus_side_rejection_pressure (one per exit_status) -------
		{"cerberus_side_rejection_pressure", "exit_status=sample_budget,language=logql,shape_id=cerb:rate", 1},
		{"cerberus_side_rejection_pressure", "exit_status=breaker,language=traceql,shape_id=trc:breaker", 2},
		{"cerberus_side_rejection_pressure", "exit_status=rejected,language=traceql,shape_id=trc:rejected", 2},
		// --- N4 heavy_shape_geometry_failing --------------------------------
		{"heavy_shape_geometry_failing", "decision_reason=below-threshold,language=promql,shape_id=cerb:sum", 2},
		{"heavy_shape_geometry_failing", "decision_reason=below-threshold,language=logql,shape_id=cerb:rate", 1},
		{"heavy_shape_geometry_failing", "decision_reason=not-sliceable,language=traceql,shape_id=trc:compare", 2},
		// N5 read_amplification_hot_shape is experimental (asserted separately).
	}
	for _, w := range want {
		f, ok := got[key{w.rule, w.class}]
		if !ok {
			t.Errorf("expected finding for rule %q class %q, none fired", w.rule, w.class)
			continue
		}
		if f.Support != w.support {
			t.Errorf("rule %q class %q support = %d, want %d", w.rule, w.class, f.Support, w.support)
		}
	}

	// Assert the EXACT total finding count, not just that the expected ones are
	// present. The (rule,class) index above is last-wins, so a rule that
	// double-emits a class (e.g. a broken per-partition restrict re-emitting the
	// same group once per partition value) would collapse to one entry and slip
	// past the membership checks. A raw count over report.Findings catches any
	// over-firing or duplicate emission.
	if len(report.Findings) != len(want) {
		t.Errorf("total finding count = %d, want %d (over-firing / double-emission?):\n%+v",
			len(report.Findings), len(want), report.Findings)
	}

	// Spot-check that the per-language partitioned watermark substituted the
	// correct concrete number into the message (promql median memory of the
	// healthy route-A rows is 80).
	if f, ok := got[key{"route_a_memory_near_cap", "language=promql,shape_id=cerb:sum"}]; ok {
		if !containsToken(f.Message, "80") {
			t.Errorf("expected memory watermark 80 in message, got: %q", f.Message)
		}
	}
}

// TestEvaluateMinSupportDropsThinClasses raises the support floor so the
// single-row timeout/sample_budget classes are dropped.
func TestEvaluateMinSupportDropsThinClasses(t *testing.T) {
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	cfg := staticConfig(map[string]string{
		"router_rules.watermark_percentile":    "0.5",
		"router_rules.cumulative_d_percentile": "0.5",
		"router_rules.min_rows_per_class":      "2", // drop the 1-row classes
		"query.max_memory_bytes":               "1073741824",
		"query.max_samples":                    "5000000",
	})
	ev := NewEvaluator(cat, cfg, seedSource(t))
	report, err := ev.Evaluate(context.Background(), EvalOptions{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	for _, f := range report.Findings {
		if f.Support < 2 {
			t.Errorf("min_support=2 should have dropped %q/%v (support %d)", f.RuleID, f.GroupKey, f.Support)
		}
		if f.RuleID == "route_a_timeout_should_shard" || f.RuleID == "route_a_hit_sample_budget" {
			t.Errorf("single-row class %q should have been dropped at min_support=2", f.RuleID)
		}
	}
}

// TestEvaluateOrdering asserts findings are ordered severity-desc.
func TestEvaluateOrdering(t *testing.T) {
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	ev := NewEvaluator(cat, evalConfig(), seedSource(t))
	report, err := ev.Evaluate(context.Background(), EvalOptions{})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	prev := Severity(255)
	for _, f := range report.Findings {
		sev, _ := parseSeverity(f.Severity)
		if sev > prev {
			t.Fatalf("findings not ordered by severity desc: %q after a lower severity", f.RuleID)
		}
		prev = sev
	}
}

// TestEvaluateMissingConfigKeyFails confirms a missing required config param is
// a hard error naming the key (no data-derived fallback const).
func TestEvaluateMissingConfigKeyFails(t *testing.T) {
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	// Omit router_rules.watermark_percentile (supply every other key so the
	// missing-key error names exactly the omitted one).
	cfg := staticConfig(map[string]string{
		"router_rules.cumulative_d_percentile": "0.5",
		"router_rules.min_rows_per_class":      "1",
		"query.max_memory_bytes":               "1073741824",
		"query.max_samples":                    "5000000",
	})
	ev := NewEvaluator(cat, cfg, seedSource(t))
	_, err = ev.Evaluate(context.Background(), EvalOptions{})
	if err == nil {
		t.Fatalf("expected a hard error for the missing config key")
	}
	if !containsToken(err.Error(), "router_rules.watermark_percentile") {
		t.Fatalf("error should name the missing key, got: %v", err)
	}
}

func classOf(gk map[string]string) string {
	// Reuse the CLI's stable ordering by sorting keys.
	keys := make([]string, 0, len(gk))
	for k := range gk {
		keys = append(keys, k)
	}
	sortStringsLocal(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ","
		}
		out += k + "=" + gk[k]
	}
	return out
}

func sortStringsLocal(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

func containsToken(s, tok string) bool {
	return len(tok) > 0 && stringIndex(s, tok) >= 0
}

func stringIndex(s, sub string) int {
	n, m := len(s), len(sub)
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}

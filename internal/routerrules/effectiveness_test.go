package routerrules

import (
	"context"
	"math"
	"sort"
	"testing"
)

// The effectiveness fixture (testdata/effectiveness.jsonl) is a FABRICATED
// corpus calibrated to two generalized production profiles (no prod-identifying
// values): a PromQL-dominant, range-heavy, healthy-majority deployment plus the
// failure surface a healthy deployment lacks. It exists to prove the catalog is
// not just well-formed but EFFECTIVE — every rule fires on its planted pathology
// and stays quiet on the healthy bulk — and to pin the meaningfulness golden
// (which shapes get flagged, with which action) against ground truth.
//
// Two classes of rule behave differently on this corpus and the assertions
// reflect that honestly:
//   - hard-failure rules gate on a non-ok exit_status (oom / timeout /
//     sample_budget / breaker / rejected). On the all-ok healthy majority they
//     must produce ZERO findings — that is the real false-positive check.
//   - self-relative tail detectors (memory cap, slow, high-fanout, read-amp)
//     flag the top of a per-shape / per-language distribution by design. The
//     fixture keeps each healthy shape's body strictly below its own p95 with a
//     sub-min_support tail, so these stay quiet on the healthy bulk and fire
//     only on the planted pathology shapes that clear both the watermark and
//     min_support.

// effConfig resolves every config-kind param at production-like settings: p95
// watermarks and a min_rows_per_class of 5 (so thin failing sets fall below
// support and exercise the support no-fire path).
func effConfig() ConfigLookup {
	return staticConfig(map[string]string{
		"router_rules.watermark_percentile":     "0.95",
		"router_rules.cumulative_d_percentile":  "0.95",
		"router_rules.min_rows_per_class":       "5",
		"router_rules.memory_near_cap_fraction": "0.8",
		"query.max_memory_bytes":                "1073741824",
		"query.max_samples":                     "50000000",
	})
}

func effSource() CorpusSource {
	return NewJSONLCorpusSource("testdata/effectiveness.jsonl", 0)
}

func effReport(t *testing.T, includeExperimental bool) *Report {
	t.Helper()
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	rep, err := NewEvaluator(cat, effConfig(), effSource()).
		Evaluate(context.Background(), EvalOptions{IncludeExperimental: includeExperimental})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return rep
}

// effFinding is one golden row: the rule, its class key, support, and action.
type effFinding struct {
	rule    string
	class   string
	support int64
	action  string
}

// effectivenessGolden is the maintainer-reviewable ground-truth set: exactly
// which shapes the active catalog flags over the effectiveness fixture, and the
// action recommended for each. A change here is a deliberate change in what the
// router-rules catalog considers a problem; review it as such. The experimental
// read-amplification rule is asserted separately (TestEffectivenessReadAmp).
var effectivenessGolden = []effFinding{
	// route-A OOM cluster (and the route-agnostic / heavy-geometry generalizers).
	{"oom_on_route_a", "language=promql,shape_id=prom:rate_sum_by_wide", 8, "force_route_b"},
	{"failure_cluster_by_reason", "decision_reason=high-cardinality,language=promql,shape_id=prom:rate_sum_by_wide", 8, "investigate_failure_cluster"},
	{"heavy_shape_geometry_failing", "decision_reason=high-cardinality,language=promql,shape_id=prom:rate_sum_by_wide", 8, "investigate_heavy_geometry"},
	// route-A timeout cluster (logql).
	{"route_a_timeout_should_shard", "language=logql,shape_id=log:unwrap_rate", 6, "force_route_b"},
	{"failure_cluster_by_reason", "decision_reason=high-cardinality,language=logql,shape_id=log:unwrap_rate", 6, "investigate_failure_cluster"},
	{"heavy_shape_geometry_failing", "decision_reason=high-cardinality,language=logql,shape_id=log:unwrap_rate", 6, "investigate_heavy_geometry"},
	// route-A sample-budget cluster.
	{"route_a_hit_sample_budget", "language=promql,shape_id=prom:topk_rate", 6, "force_route_b"},
	{"cerberus_side_rejection_pressure", "exit_status=sample_budget,language=promql,shape_id=prom:topk_rate", 6, "review_rejection_guardrail"},
	// cerberus-side breaker + rejected sets.
	{"cerberus_side_rejection_pressure", "exit_status=breaker,language=traceql,shape_id=trc:breaker", 5, "review_rejection_guardrail"},
	{"cerberus_side_rejection_pressure", "exit_status=rejected,language=promql,shape_id=prom:rejected", 5, "review_rejection_guardrail"},
	// route-B still-failing cluster with non-zero k_shards (N2 guardrail) + N1/N4.
	{"route_b_still_failing", "decision_reason=not-sliceable,language=promql,shape_id=prom:rate_sum_by_huge", 6, "cap_cardinality_or_reject"},
	{"failure_cluster_by_reason", "decision_reason=not-sliceable,language=promql,shape_id=prom:rate_sum_by_huge", 6, "investigate_failure_cluster"},
	{"heavy_shape_geometry_failing", "decision_reason=not-sliceable,language=promql,shape_id=prom:rate_sum_by_huge", 6, "investigate_heavy_geometry"},
	// route-B overshard-regret class.
	{"route_b_overshard_low_fanout", "language=promql,shape_id=prom:rate_overshard", 7, "raise_route_b_threshold"},
	// high-fanout route-A class.
	{"route_a_high_fanout_should_shard", "language=promql,shape_id=prom:rate_sum_by_hot", 7, "lower_route_b_threshold"},
	// memory-near-cap route-A ok class (cap-relative gate): the ONLY ok shape
	// whose peak memory clears 0.8 x the configured 1 GiB cap (~859 MB). The
	// healthy-but-above-own-p95 shapes prom:hq_rate_heavy (700 MB, 65% of cap) and
	// prom:rate_sum_by_hot (180 MB, 17% of cap) sit above their corpus p95 yet
	// well below the cap, so they deliberately do NOT fire here anymore — that is
	// the false-positive-by-construction the cap-relative gate removes.
	{"route_a_memory_near_cap", "language=promql,shape_id=prom:mem_near_cap", 7, "lower_route_b_threshold"},
	// slow hot shapes: the two slow failure clusters are also in their language's
	// duration p95 tail (oom 4s / timeout 9s), so the slow-shape detector flags
	// them by normalized_query_hash.
	{"route_a_slow_hot_shape", "decision_reason=high-cardinality,language=logql,normalized_query_hash=202", 6, "lower_route_b_threshold"},
	{"route_a_slow_hot_shape", "decision_reason=high-cardinality,language=promql,normalized_query_hash=105", 8, "lower_route_b_threshold"},
}

// TestEffectivenessGolden is the meaningfulness-vs-ground-truth assertion: every
// golden finding is present with the expected support and action, AND the total
// finding count matches exactly (so a rule that over-fires or double-emits a
// class is caught, not masked by the membership checks).
func TestEffectivenessGolden(t *testing.T) {
	rep := effReport(t, false)

	type key struct{ rule, class string }
	got := map[key]Finding{}
	for _, f := range rep.Findings {
		got[key{f.RuleID, classOf(f.GroupKey)}] = f
	}

	for _, w := range effectivenessGolden {
		f, ok := got[key{w.rule, w.class}]
		if !ok {
			t.Errorf("missing golden finding: rule=%q class=%q", w.rule, w.class)
			continue
		}
		if f.Support != w.support {
			t.Errorf("rule %q class %q support = %d, want %d", w.rule, w.class, f.Support, w.support)
		}
		if f.Action != w.action {
			t.Errorf("rule %q class %q action = %q, want %q", w.rule, w.class, f.Action, w.action)
		}
	}

	if len(rep.Findings) != len(effectivenessGolden) {
		t.Errorf("total finding count = %d, want %d (over-firing / double-emission?):\n%s",
			len(rep.Findings), len(effectivenessGolden), dumpFindings(rep.Findings))
	}
	if len(rep.Skipped) != 0 {
		t.Errorf("no rule should be skipped on the full fixture (route B is populated), got: %+v", rep.Skipped)
	}
}

// firingClasses returns the set of class keys a rule fired on.
func firingClasses(rep *Report, rule string) map[string]int64 {
	out := map[string]int64{}
	for _, f := range rep.Findings {
		if f.RuleID == rule {
			out[classOf(f.GroupKey)] = f.Support
		}
	}
	return out
}

// healthyShapes are the all-ok, below-watermark classes that form the healthy
// majority. No hard-failure rule may fire on any of them.
var healthyShapes = []string{
	"prom:selector", "prom:sum", "prom:rate_sum_by", "prom:hq_rate",
	"log:count_over_time", "trc:compare",
}

// hardFailureRules gate on a non-ok exit_status. On the all-ok healthy majority
// they must produce zero findings — the real false-positive check.
var hardFailureRules = []string{
	"oom_on_route_a",
	"route_a_timeout_should_shard",
	"route_a_hit_sample_budget",
	"failure_cluster_by_reason",
	"route_b_still_failing",
	"cerberus_side_rejection_pressure",
	"heavy_shape_geometry_failing",
}

// TestEffectivenessNoFalsePositivesOnHealthy asserts the hard-failure rules stay
// completely quiet on every healthy shape: their findings (if any) only ever
// land on planted-pathology classes, never on the all-ok majority.
func TestEffectivenessNoFalsePositivesOnHealthy(t *testing.T) {
	rep := effReport(t, true) // include experimental so the check is exhaustive
	healthy := map[string]struct{}{}
	for _, s := range healthyShapes {
		healthy[s] = struct{}{}
	}
	for _, rule := range hardFailureRules {
		for _, f := range rep.Findings {
			if f.RuleID != rule {
				continue
			}
			if _, isHealthy := healthy[f.GroupKey["shape_id"]]; isHealthy {
				t.Errorf("false positive: hard-failure rule %q fired on healthy shape %q (class %q)",
					rule, f.GroupKey["shape_id"], classOf(f.GroupKey))
			}
		}
	}
}

// --- per-rule fire + no-fire edges ----------------------------------------
//
// Each test pins one rule's planted-pathology fire AND a no-fire boundary, so a
// regression that silences a rule or makes it spill onto the wrong class is
// localized (the golden's total-count alone cannot say WHICH rule drifted).

func TestRuleOOMOnRouteAFiresAndQuiet(t *testing.T) {
	rep := effReport(t, false)
	fired := firingClasses(rep, "oom_on_route_a")
	if fired["language=promql,shape_id=prom:rate_sum_by_wide"] != 8 {
		t.Errorf("oom_on_route_a should fire on the route-A oom cluster (support 8), got %v", fired)
	}
	// The route-B oom cluster is NOT route A, so oom_on_route_a must skip it.
	if _, ok := fired["language=promql,shape_id=prom:rate_sum_by_huge"]; ok {
		t.Error("oom_on_route_a must not fire on the route-B oom cluster")
	}
}

func TestRuleMemoryNearCapFiresAndQuiet(t *testing.T) {
	rep := effReport(t, false)
	fired := firingClasses(rep, "route_a_memory_near_cap")
	// Fires only on the genuine near-cap class: 950 MB peak = 88% of the 1 GiB
	// configured cap, clearing the 0.8 x cap (~859 MB) near-cap line.
	if fired["language=promql,shape_id=prom:mem_near_cap"] != 7 {
		t.Errorf("route_a_memory_near_cap should fire on the genuine near-cap class (support 7), got %v", fired)
	}
	// The cap-relative gate's whole point: ok classes that sit above their corpus
	// p95 but well below the configured cap are NOT near-cap and must stay quiet.
	// prom:hq_rate_heavy (700 MB, 65% of cap) and prom:rate_sum_by_hot (180 MB,
	// 17% of cap) both cleared the OLD corpus-p95 gate; under the cap-relative gate
	// they no longer fire — the false-positive-by-construction is gone.
	for _, belowCap := range []string{"prom:hq_rate_heavy", "prom:rate_sum_by_hot"} {
		if n, ok := fired["language=promql,shape_id="+belowCap]; ok {
			t.Errorf("route_a_memory_near_cap fired on below-cap ok class %q (support %d); a class above its own p95 but far below the configured cap must NOT fire", belowCap, n)
		}
	}
	assertNotFiredOnHealthy(t, rep, "route_a_memory_near_cap")
}

// assertNotFiredOnHealthy fails if a rule fired on any healthy shape, matching
// shape_id exactly (a substring match would conflate prom:rate_sum_by with the
// distinct prom:rate_sum_by_hot pathology shape).
func assertNotFiredOnHealthy(t *testing.T, rep *Report, rule string) {
	t.Helper()
	healthy := map[string]struct{}{}
	for _, s := range healthyShapes {
		healthy[s] = struct{}{}
	}
	for _, f := range rep.Findings {
		if f.RuleID != rule {
			continue
		}
		if _, isHealthy := healthy[f.GroupKey["shape_id"]]; isHealthy {
			t.Errorf("%s fired on healthy shape %q (class %q)", rule, f.GroupKey["shape_id"], classOf(f.GroupKey))
		}
	}
}

func TestRuleHighFanoutFiresAndQuiet(t *testing.T) {
	rep := effReport(t, false)
	fired := firingClasses(rep, "route_a_high_fanout_should_shard")
	if fired["language=promql,shape_id=prom:rate_sum_by_hot"] != 7 {
		t.Errorf("route_a_high_fanout_should_shard should fire on the high-fanout route-A class (support 7), got %v", fired)
	}
	// The healthy rate_sum_by shape has fanout below the route-B floor.
	assertNotFiredOnHealthy(t, rep, "route_a_high_fanout_should_shard")
}

func TestRuleSlowHotShapeFires(t *testing.T) {
	rep := effReport(t, false)
	fired := firingClasses(rep, "route_a_slow_hot_shape")
	want := []string{
		"decision_reason=high-cardinality,language=logql,normalized_query_hash=202",
		"decision_reason=high-cardinality,language=promql,normalized_query_hash=105",
	}
	for _, w := range want {
		if _, ok := fired[w]; !ok {
			t.Errorf("route_a_slow_hot_shape should fire on slow class %q, got %v", w, fired)
		}
	}
}

func TestRuleTimeoutShouldShardFiresAndQuiet(t *testing.T) {
	rep := effReport(t, false)
	fired := firingClasses(rep, "route_a_timeout_should_shard")
	if fired["language=logql,shape_id=log:unwrap_rate"] != 6 {
		t.Errorf("route_a_timeout_should_shard should fire on the route-A timeout cluster (support 6), got %v", fired)
	}
	if len(fired) != 1 {
		t.Errorf("route_a_timeout_should_shard should fire on exactly one class, got %v", fired)
	}
}

func TestRuleHitSampleBudgetFires(t *testing.T) {
	rep := effReport(t, false)
	fired := firingClasses(rep, "route_a_hit_sample_budget")
	if fired["language=promql,shape_id=prom:topk_rate"] != 6 {
		t.Errorf("route_a_hit_sample_budget should fire on the sample-budget cluster (support 6), got %v", fired)
	}
}

func TestRuleOvershardLowFanoutFires(t *testing.T) {
	rep := effReport(t, false)
	fired := firingClasses(rep, "route_b_overshard_low_fanout")
	if fired["language=promql,shape_id=prom:rate_overshard"] != 7 {
		t.Errorf("route_b_overshard_low_fanout should fire on the route-B overshard class (support 7), got %v", fired)
	}
}

func TestRuleRouteBStillFailingFires(t *testing.T) {
	rep := effReport(t, false)
	fired := firingClasses(rep, "route_b_still_failing")
	if fired["decision_reason=not-sliceable,language=promql,shape_id=prom:rate_sum_by_huge"] != 6 {
		t.Errorf("route_b_still_failing should fire on the route-B failing cluster (support 6), got %v", fired)
	}
}

func TestRuleRejectionPressureFiresPerExitStatus(t *testing.T) {
	rep := effReport(t, false)
	fired := firingClasses(rep, "cerberus_side_rejection_pressure")
	want := []string{
		"exit_status=breaker,language=traceql,shape_id=trc:breaker",
		"exit_status=rejected,language=promql,shape_id=prom:rejected",
		"exit_status=sample_budget,language=promql,shape_id=prom:topk_rate",
	}
	for _, w := range want {
		if _, ok := fired[w]; !ok {
			t.Errorf("cerberus_side_rejection_pressure should fire on %q, got %v", w, fired)
		}
	}
	if len(fired) != len(want) {
		t.Errorf("cerberus_side_rejection_pressure fired on %d classes, want %d: %v", len(fired), len(want), fired)
	}
}

func TestRuleFailureClusterByReasonFires(t *testing.T) {
	rep := effReport(t, false)
	fired := firingClasses(rep, "failure_cluster_by_reason")
	want := []string{
		"decision_reason=high-cardinality,language=logql,shape_id=log:unwrap_rate",
		"decision_reason=high-cardinality,language=promql,shape_id=prom:rate_sum_by_wide",
		"decision_reason=not-sliceable,language=promql,shape_id=prom:rate_sum_by_huge",
	}
	for _, w := range want {
		if _, ok := fired[w]; !ok {
			t.Errorf("failure_cluster_by_reason should fire on %q, got %v", w, fired)
		}
	}
}

func TestRuleHeavyGeometryFailingFires(t *testing.T) {
	rep := effReport(t, false)
	fired := firingClasses(rep, "heavy_shape_geometry_failing")
	want := []string{
		"decision_reason=high-cardinality,language=logql,shape_id=log:unwrap_rate",
		"decision_reason=high-cardinality,language=promql,shape_id=prom:rate_sum_by_wide",
		"decision_reason=not-sliceable,language=promql,shape_id=prom:rate_sum_by_huge",
	}
	for _, w := range want {
		if _, ok := fired[w]; !ok {
			t.Errorf("heavy_shape_geometry_failing should fire on %q, got %v", w, fired)
		}
	}
}

// TestEffectivenessReadAmp covers the experimental, self-relative read-
// amplification detector. It is asserted separately because it flags the top of
// each shape's own read_rows distribution by design, so it lights up on the
// planted amplification shape (the meaningful case) and is gated off the default
// report.
func TestEffectivenessReadAmp(t *testing.T) {
	// Off by default: not in the active golden.
	active := effReport(t, false)
	if _, ok := firingClasses(active, "read_amplification_hot_shape")["language=promql,shape_id=prom:rate_amp"]; ok {
		t.Error("experimental read_amplification_hot_shape must not appear without IncludeExperimental")
	}
	// On with experimental: fires on the planted amplification shape.
	exp := effReport(t, true)
	if _, ok := firingClasses(exp, "read_amplification_hot_shape")["language=promql,shape_id=prom:rate_amp"]; !ok {
		t.Errorf("read_amplification_hot_shape should fire on the planted amp shape, got %v",
			firingClasses(exp, "read_amplification_hot_shape"))
	}
}

// TestEffectivenessSupportNoFire confirms the min_support floor drops thin
// failing sets: the trc:spans_thin oom set (2 rows, below min_rows_per_class=5)
// meets every hard-failure condition yet produces no finding.
func TestEffectivenessSupportNoFire(t *testing.T) {
	rep := effReport(t, true)
	for _, f := range rep.Findings {
		if f.GroupKey["shape_id"] == "trc:spans_thin" {
			t.Errorf("thin sub-support class trc:spans_thin should be dropped, but %q fired: %+v", f.RuleID, f)
		}
	}
}

// TestEffectivenessParamsNonDegenerate proves the corpus-derived watermarks
// resolve to non-degenerate values on this realistic distribution where a
// population exists: neither 0 (the empty-population degenerate) nor NoSignal.
func TestEffectivenessParamsNonDegenerate(t *testing.T) {
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	env, err := NewParamResolver(effConfig(), effSource()).Resolve(context.Background(), cat)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// fanout_route_b_floor is the scalar route-B watermark; with route B
	// populated it must be a finite, positive value.
	floor, ok := env["fanout_route_b_floor"]
	if !ok {
		t.Fatal("fanout_route_b_floor not resolved")
	}
	if floor.NoSignal {
		t.Error("fanout_route_b_floor must have signal when route B is populated")
	}
	if floor.IsPartitioned() {
		t.Error("fanout_route_b_floor must be scalar")
	}
	if floor.Scalar <= 0 || math.IsInf(floor.Scalar, 0) || math.IsNaN(floor.Scalar) {
		t.Errorf("fanout_route_b_floor = %v, want a finite positive watermark", floor.Scalar)
	}

	// Partitioned watermarks must carry a positive value for every populated
	// language bucket.
	for _, name := range []string{"memory_high_watermark", "slow_duration_watermark", "d_high_watermark", "read_rows_high_watermark"} {
		v, ok := env[name]
		if !ok {
			t.Errorf("%s not resolved", name)
			continue
		}
		if !v.IsPartitioned() {
			t.Errorf("%s should be partition-keyed", name)
			continue
		}
		if len(v.Partition) == 0 {
			t.Errorf("%s has no partition buckets", name)
		}
		for k, scal := range v.Partition {
			if scal <= 0 || math.IsInf(scal, 0) || math.IsNaN(scal) {
				t.Errorf("%s[%q] = %v, want a finite positive watermark", name, k, scal)
			}
		}
	}

	// cerberus_reject_ratio is a MESSAGE-only count ratio: a finite fraction in
	// (0,1] on this corpus (it has rejected rows), and never NoSignal.
	ratio, ok := env["cerberus_reject_ratio"]
	if !ok {
		t.Fatal("cerberus_reject_ratio not resolved")
	}
	if ratio.NoSignal {
		t.Error("cerberus_reject_ratio is message-only and must never be NoSignal")
	}
	if ratio.Scalar <= 0 || ratio.Scalar > 1 {
		t.Errorf("cerberus_reject_ratio = %v, want a fraction in (0,1]", ratio.Scalar)
	}
}

// dumpFindings renders findings for a failure message, sorted for determinism.
func dumpFindings(fs []Finding) string {
	var ls []string
	for _, f := range fs {
		ls = append(ls, f.RuleID+" "+classOf(f.GroupKey))
	}
	sort.Strings(ls)
	out := ""
	for _, l := range ls {
		out += "  " + l + "\n"
	}
	return out
}

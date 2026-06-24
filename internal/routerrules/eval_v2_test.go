package routerrules

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The catalogVersion-2 harness. Every new rule (N1-N5) gets a seeded-corpus
// FIRE case and a NO-FIRE boundary case, all in the default (CGO-free) lane so
// they actually run in CI. The exact-total-count assertion in
// TestEvaluateEmbeddedCatalogFindings (eval_test.go) is the over-firing guard;
// the cases here pin the per-rule firing/non-firing edges that a total count
// alone cannot localize.

// evalReport runs the embedded catalog over the seed and returns the report,
// optionally including experimental rules.
func evalReport(t *testing.T, includeExperimental bool) *Report {
	t.Helper()
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	ev := NewEvaluator(cat, evalConfig(), seedSource(t))
	report, err := ev.Evaluate(context.Background(), EvalOptions{IncludeExperimental: includeExperimental})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	return report
}

// findingFor returns the finding for (rule, class), or false if none fired.
func findingFor(r *Report, rule, class string) (Finding, bool) {
	for _, f := range r.Findings {
		if f.RuleID == rule && classOf(f.GroupKey) == class {
			return f, true
		}
	}
	return Finding{}, false
}

// countFor returns how many findings a given rule produced.
func countFor(r *Report, rule string) int {
	n := 0
	for _, f := range r.Findings {
		if f.RuleID == rule {
			n++
		}
	}
	return n
}

// --- N1 failure_cluster_by_reason -----------------------------------------

// TestN1FiresOnHardFailureCluster confirms N1 fires on a route-agnostic hard
// failure cluster (route-B trc:compare oom+timeout) attributed by solver
// reason, and carries the geometry/cost evidence aggregates.
func TestN1FiresOnHardFailureCluster(t *testing.T) {
	r := evalReport(t, false)
	f, ok := findingFor(r, "failure_cluster_by_reason",
		"decision_reason=not-sliceable,language=traceql,shape_id=trc:compare")
	if !ok {
		t.Fatal("N1 should fire on the route-B not-sliceable hard-failure cluster")
	}
	if f.Support != 2 {
		t.Errorf("N1 support = %d, want 2", f.Support)
	}
	for _, key := range []string{"max(memory_usage)", "max(query_duration_ms)", "max(read_rows)"} {
		if _, ok := f.Evidence[key]; !ok {
			t.Errorf("N1 evidence missing %q: %+v", key, f.Evidence)
		}
	}
	if f.Action != "investigate_failure_cluster" {
		t.Errorf("N1 action = %q, want investigate_failure_cluster", f.Action)
	}
}

// TestN1NoFireOnHealthyCluster is the NO-FIRE boundary: a class that is entirely
// exit_status=ok (the trc:compare ok row is the only ok one, but the topk/sum
// healthy classes are pure-ok) must not produce an N1 finding.
func TestN1NoFireOnHealthyCluster(t *testing.T) {
	r := evalReport(t, false)
	for _, class := range []string{
		"decision_reason=routed,language=promql,shape_id=cerb:topk",
		"decision_reason=routed,language=logql,shape_id=cerb:rate",
	} {
		if _, ok := findingFor(r, "failure_cluster_by_reason", class); ok {
			t.Errorf("N1 must not fire on healthy class %q", class)
		}
	}
}

// TestN1AndOomOnRouteADoNotDedup proves the route-A below-threshold OOM class
// fires BOTH oom_on_route_a (force_route_b) and failure_cluster_by_reason
// (investigate_failure_cluster) as two distinct findings with distinct actions —
// the generalization is intentional, not a dedup bug.
func TestN1AndOomOnRouteADoNotDedup(t *testing.T) {
	r := evalReport(t, false)
	oom, okOom := findingFor(r, "oom_on_route_a", "language=promql,shape_id=cerb:sum")
	n1, okN1 := findingFor(r, "failure_cluster_by_reason",
		"decision_reason=below-threshold,language=promql,shape_id=cerb:sum")
	if !okOom || !okN1 {
		t.Fatalf("expected both oom_on_route_a (%v) and N1 (%v) to fire on the route-A OOM class", okOom, okN1)
	}
	if oom.Action == n1.Action {
		t.Errorf("the two findings must carry distinct actions, both = %q", oom.Action)
	}
	if oom.Action != "force_route_b" || n1.Action != "investigate_failure_cluster" {
		t.Errorf("actions = (%q, %q), want (force_route_b, investigate_failure_cluster)", oom.Action, n1.Action)
	}
}

// --- N2 route_b_still_failing ---------------------------------------------

// TestN2FiresOnRouteBFailure confirms N2 fires when a route-B class still fails
// and surfaces max(k_shards) so the operator reads the saturation from data.
func TestN2FiresOnRouteBFailure(t *testing.T) {
	r := evalReport(t, false)
	f, ok := findingFor(r, "route_b_still_failing",
		"decision_reason=not-sliceable,language=traceql,shape_id=trc:compare")
	if !ok {
		t.Fatal("N2 should fire on the route-B failing cluster")
	}
	if f.Support != 2 {
		t.Errorf("N2 support = %d, want 2", f.Support)
	}
	if _, ok := f.Evidence["max(k_shards)"]; !ok {
		t.Errorf("N2 evidence missing max(k_shards): %+v", f.Evidence)
	}
	if f.Action != "cap_cardinality_or_reject" {
		t.Errorf("N2 action = %q, want cap_cardinality_or_reject", f.Action)
	}
}

// TestN2NoFireOnRouteAFailureOrRouteBOk is the NO-FIRE boundary: a route-A
// failure (route=B leaf excludes it) and a route-B success (exit_status leaf
// excludes it) must not fire N2. The total N2 count must be exactly 1.
func TestN2NoFireOnRouteAFailureOrRouteBOk(t *testing.T) {
	r := evalReport(t, false)
	// route-A failure (trc:spans oom/timeout) is owned by N1, never N2.
	if _, ok := findingFor(r, "route_b_still_failing",
		"decision_reason=instant,language=traceql,shape_id=trc:spans"); ok {
		t.Error("N2 must not fire on a route-A failure")
	}
	// route-B healthy classes must not fire N2.
	if _, ok := findingFor(r, "route_b_still_failing",
		"decision_reason=routed,language=promql,shape_id=cerb:topk"); ok {
		t.Error("N2 must not fire on a healthy route-B class")
	}
	if got := countFor(r, "route_b_still_failing"); got != 1 {
		t.Errorf("route_b_still_failing fired %d times, want exactly 1", got)
	}
}

// --- N3 cerberus_side_rejection_pressure ----------------------------------

// TestN3FiresPerExitStatus confirms N3 fires once per cerberus-side terminal
// status (sample_budget / breaker / rejected), each as a distinct group.
func TestN3FiresPerExitStatus(t *testing.T) {
	r := evalReport(t, false)
	for _, c := range []struct {
		class   string
		support int64
	}{
		{"exit_status=sample_budget,language=logql,shape_id=cerb:rate", 1},
		{"exit_status=breaker,language=traceql,shape_id=trc:breaker", 2},
		{"exit_status=rejected,language=traceql,shape_id=trc:rejected", 2},
	} {
		f, ok := findingFor(r, "cerberus_side_rejection_pressure", c.class)
		if !ok {
			t.Errorf("N3 should fire on %q", c.class)
			continue
		}
		if f.Support != c.support {
			t.Errorf("N3 %q support = %d, want %d", c.class, f.Support, c.support)
		}
	}
	if got := countFor(r, "cerberus_side_rejection_pressure"); got != 3 {
		t.Errorf("N3 fired %d times, want exactly 3", got)
	}
}

// TestN3SurfacesDeploymentWideRejectRatio is the message-only param test: the
// corpus_count_ratio scalar (cerberus_reject_ratio) must be substituted into
// the finding MESSAGE even though no rule condition references it. This pins the
// invariant that Resolve resolves ALL registry params, not just rule-referenced
// ones — a "resolve only referenced params" optimization would silently leave
// the raw {cerberus_reject_ratio} placeholder in the message.
func TestN3SurfacesDeploymentWideRejectRatio(t *testing.T) {
	r := evalReport(t, false)
	f, ok := findingFor(r, "cerberus_side_rejection_pressure",
		"exit_status=rejected,language=traceql,shape_id=trc:rejected")
	if !ok {
		t.Fatal("N3 should fire on the rejected cluster")
	}
	// 2 rejected over 13 route-A rows = 0.15384615...; formatNumeric renders the
	// full-precision fraction. Assert the placeholder was substituted (no raw
	// token) and the leading digits of the ratio are present.
	if containsToken(f.Message, "{cerberus_reject_ratio}") {
		t.Errorf("the message-only ratio placeholder was not substituted: %q", f.Message)
	}
	if !containsToken(f.Message, "0.1538") {
		t.Errorf("expected the deployment-wide reject ratio (~0.1538) in the message, got: %q", f.Message)
	}
}

// TestN3NoFireOnSuccessOrCHsideFailure is the NO-FIRE boundary: a healthy class
// and a CH-side OOM/timeout (owned by N1) must not fire N3.
func TestN3NoFireOnSuccessOrCHsideFailure(t *testing.T) {
	r := evalReport(t, false)
	for _, class := range []string{
		"exit_status=ok,language=promql,shape_id=cerb:topk",
		"exit_status=oom,language=traceql,shape_id=trc:compare",
		"exit_status=timeout,language=logql,shape_id=cerb:rate",
	} {
		if _, ok := findingFor(r, "cerberus_side_rejection_pressure", class); ok {
			t.Errorf("N3 must not fire on %q (only sample_budget/breaker/rejected)", class)
		}
	}
}

// --- N4 heavy_shape_geometry_failing --------------------------------------

// TestN4FiresOnHeavyGeometry confirms N4 fires when a failing class's
// cumulative_d sits at/above its own per-language tail, and reports the geometry
// columns the grounding names (cumulative_d, n_anchors, fanout).
func TestN4FiresOnHeavyGeometry(t *testing.T) {
	r := evalReport(t, false)
	f, ok := findingFor(r, "heavy_shape_geometry_failing",
		"decision_reason=not-sliceable,language=traceql,shape_id=trc:compare")
	if !ok {
		t.Fatal("N4 should fire on the heavy-geometry traceql failure cluster")
	}
	for _, key := range []string{"max(cumulative_d)", "max(n_anchors)", "max(fanout)"} {
		if _, ok := f.Evidence[key]; !ok {
			t.Errorf("N4 evidence missing %q: %+v", key, f.Evidence)
		}
	}
	if f.Action != "investigate_heavy_geometry" {
		t.Errorf("N4 action = %q, want investigate_heavy_geometry", f.Action)
	}
}

// TestN4NoFireWhenFailingButLightGeometry is the critical boundary: a class that
// FAILS (so N1 fires) but whose cumulative_d is below its own tail must NOT fire
// N4. trc:spans (cumulative_d 40/50, below the traceql median 250) is exactly
// that case — it fires N1 but not N4.
func TestN4NoFireWhenFailingButLightGeometry(t *testing.T) {
	r := evalReport(t, false)
	if _, ok := findingFor(r, "failure_cluster_by_reason",
		"decision_reason=instant,language=traceql,shape_id=trc:spans"); !ok {
		t.Fatal("precondition: N1 should fire on trc:spans (it is a hard-failure class)")
	}
	if _, ok := findingFor(r, "heavy_shape_geometry_failing",
		"decision_reason=instant,language=traceql,shape_id=trc:spans"); ok {
		t.Error("N4 must NOT fire on a failing-but-light-geometry class (cumulative_d below tail)")
	}
}

// --- N5 read_amplification_hot_shape (experimental) -----------------------

// TestN5IsExperimentalGated confirms N5 is loaded+validated but produces zero
// findings unless IncludeExperimental is set, then fires on the read-amplified
// healthy classes when opted in.
func TestN5IsExperimentalGated(t *testing.T) {
	active := evalReport(t, false)
	if got := countFor(active, "read_amplification_hot_shape"); got != 0 {
		t.Errorf("N5 is experimental and must not fire in the active lane, fired %d", got)
	}
	exp := evalReport(t, true)
	if got := countFor(exp, "read_amplification_hot_shape"); got == 0 {
		t.Error("N5 should fire when experimental rules are opted in")
	}
	f, ok := findingFor(exp, "read_amplification_hot_shape", "language=traceql,shape_id=trc:compare")
	if !ok {
		t.Fatal("N5 should fire on the trc:compare healthy-read tail under --experimental")
	}
	for _, key := range []string{"max(read_rows)", "max(read_bytes)"} {
		if _, ok := f.Evidence[key]; !ok {
			t.Errorf("N5 evidence missing %q: %+v", key, f.Evidence)
		}
	}
}

// TestN5PartitionedMessageSubstitution pins the partitioned-param message
// substitution (the resolvePlaceholder partition-fallback path): N5's
// read_rows_high_watermark is partitioned by shape_id, so each finding's message
// must carry the per-shape watermark, not a raw placeholder. cerb:sum's healthy
// read_rows median is 800, so its N5 message must contain "800".
func TestN5PartitionedMessageSubstitution(t *testing.T) {
	exp := evalReport(t, true)
	f, ok := findingFor(exp, "read_amplification_hot_shape", "language=promql,shape_id=cerb:sum")
	if !ok {
		t.Fatal("N5 should fire on cerb:sum healthy-read tail under --experimental")
	}
	if containsToken(f.Message, "{read_rows_high_watermark}") {
		t.Errorf("partitioned watermark placeholder not substituted: %q", f.Message)
	}
	if !containsToken(f.Message, "800") {
		t.Errorf("expected cerb:sum per-shape read watermark 800 in message, got: %q", f.Message)
	}
}

// --- amendment regression -------------------------------------------------

// TestSlowHotShapeCarriesDecisionReason pins the catalogVersion-2 group_by
// amendment: route_a_slow_hot_shape now groups by decision_reason, so its group
// keys must include it. A revert of the amendment drops the key and fails here.
func TestSlowHotShapeCarriesDecisionReason(t *testing.T) {
	r := evalReport(t, false)
	any := false
	for _, f := range r.Findings {
		if f.RuleID != "route_a_slow_hot_shape" {
			continue
		}
		any = true
		if _, ok := f.GroupKey["decision_reason"]; !ok {
			t.Errorf("route_a_slow_hot_shape finding missing decision_reason in group key: %+v", f.GroupKey)
		}
	}
	if !any {
		t.Fatal("expected at least one route_a_slow_hot_shape finding")
	}
}

// --- empty corpus ---------------------------------------------------------

// TestEvaluateEmptyCorpus confirms an empty JSONL corpus yields a non-error,
// zero-finding report: every corpus param resolves to the empty-agg 0-contract,
// no panic, no NaN-driven spurious fire.
func TestEvaluateEmptyCorpus(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.jsonl"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	ev := NewEvaluator(cat, evalConfig(), NewJSONLCorpusSource(dir, 0))
	report, err := ev.Evaluate(context.Background(), EvalOptions{IncludeExperimental: true})
	if err != nil {
		t.Fatalf("empty corpus must not error: %v", err)
	}
	if len(report.Findings) != 0 {
		t.Errorf("empty corpus must yield zero findings, got %d: %+v", len(report.Findings), report.Findings)
	}
}

// --- no double emission ---------------------------------------------------

// TestNewRulesNoDoubleEmission asserts every (rule, group-key) pair is emitted
// exactly once. The three new failure rules group by decision_reason, so a
// broken expandPartitioned/restrict that re-emits a class once per partition
// value would surface here even when the global total happens to match.
func TestNewRulesNoDoubleEmission(t *testing.T) {
	r := evalReport(t, true)
	seen := map[string]int{}
	for _, f := range r.Findings {
		k := f.RuleID + "|" + classOf(f.GroupKey)
		seen[k]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("finding %q emitted %d times, want exactly 1 (double-emission)", k, n)
		}
	}
}

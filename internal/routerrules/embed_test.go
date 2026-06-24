package routerrules

import (
	"sort"
	"strings"
	"testing"
)

// expectedRuleIDs is the full set the split catalog must merge to — one file per
// id under catalog/rules/. Pinning the set here is the kill-mutant guard: if a
// rule file is dropped, renamed, or silently not embedded, the merged set drifts
// from this list and the test fails.
var expectedRuleIDs = []string{
	"cerberus_side_rejection_pressure",
	"failure_cluster_by_reason",
	"heavy_shape_geometry_failing",
	"oom_on_route_a",
	"read_amplification_hot_shape",
	"route_a_high_fanout_should_shard",
	"route_a_hit_sample_budget",
	"route_a_memory_near_cap",
	"route_a_slow_hot_shape",
	"route_a_timeout_should_shard",
	"route_b_overshard_low_fanout",
	"route_b_still_failing",
}

// TestEmbeddedSplitCatalogMergesAllRules asserts the split catalog loads every
// rule file and the merged set equals the pinned pre-split set (same ids, same
// count) — the structural-split-must-not-change-semantics guard.
func TestEmbeddedSplitCatalogMergesAllRules(t *testing.T) {
	cat, err := LoadEmbeddedCatalog()
	if err != nil {
		t.Fatalf("LoadEmbeddedCatalog: %v", err)
	}
	if len(cat.Rules) != len(expectedRuleIDs) {
		t.Fatalf("merged rule count = %d, want %d", len(cat.Rules), len(expectedRuleIDs))
	}
	got := make([]string, len(cat.Rules))
	for i, r := range cat.Rules {
		got[i] = r.ID
	}
	gotSorted := append([]string(nil), got...)
	sort.Strings(gotSorted)
	for i, id := range expectedRuleIDs {
		if gotSorted[i] != id {
			t.Fatalf("merged rule ids = %v, want %v", gotSorted, expectedRuleIDs)
		}
	}
}

// TestEmbeddedRuleFilesAreSortedDeterministically pins the merge order: rule
// files merge sorted by filename, so the in-memory rule slice order is stable
// and reproducible across runs and platforms (independent of FS iteration).
func TestEmbeddedRuleFilesAreSortedDeterministically(t *testing.T) {
	files, err := embeddedRuleFiles()
	if err != nil {
		t.Fatalf("embeddedRuleFiles: %v", err)
	}
	if len(files) != len(expectedRuleIDs) {
		t.Fatalf("rule file count = %d, want %d", len(files), len(expectedRuleIDs))
	}
	for i := 1; i < len(files); i++ {
		if files[i-1].Name >= files[i].Name {
			t.Fatalf("rule files not strictly sorted: %q before %q", files[i-1].Name, files[i].Name)
		}
	}
}

// TestMergeRejectsDuplicateRuleID is the kill-mutant for the cross-file dup-id
// guard: two files declaring the same rule id MUST fail the merge, naming both
// offending files.
func TestMergeRejectsDuplicateRuleID(t *testing.T) {
	base := CatalogFile{Name: "catalog.yaml", Bytes: []byte(`apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params: []
`)}
	const ruleBody = `rules:
  - id: dup_rule
    severity: low
    since: 1
    status: active
    group_by: [shape_id]
    condition: { col: route, op: eq, enum: A }
    finding: "x"
`
	a := CatalogFile{Name: "dup_rule.yaml", Bytes: []byte(ruleBody)}
	b := CatalogFile{Name: "dup_rule_copy.yaml", Bytes: []byte(ruleBody)}

	_, err := mergeCatalogFiles([]CatalogFile{base, a, b})
	if err == nil {
		t.Fatalf("expected merge to reject duplicate rule id across files")
	}
	if !strings.Contains(err.Error(), "dup_rule") {
		t.Fatalf("error should name the duplicate id, got: %v", err)
	}
	if !strings.Contains(err.Error(), "dup_rule.yaml") || !strings.Contains(err.Error(), "dup_rule_copy.yaml") {
		t.Fatalf("error should name both offending files, got: %v", err)
	}
}

// TestMergeRejectsMalformedRuleFile asserts a malformed rule file fails the
// merge and names the offending file.
func TestMergeRejectsMalformedRuleFile(t *testing.T) {
	base := CatalogFile{Name: "catalog.yaml", Bytes: []byte(`apiVersion: routerrules.cerberus/v1
catalogVersion: 1
params: []
`)}
	bad := CatalogFile{Name: "broken.yaml", Bytes: []byte("rules:\n  - id: r\n    bogus_field: 1\n")}

	_, err := mergeCatalogFiles([]CatalogFile{base, bad})
	if err == nil {
		t.Fatalf("expected merge to reject malformed rule file")
	}
	if !strings.Contains(err.Error(), "broken.yaml") {
		t.Fatalf("error should name the offending file, got: %v", err)
	}
}

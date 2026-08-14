package regression

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

// TestPromOutcomeFixturesStayOutsideSampleParity closes #1986's structural
// remainder without pretending endpoint projections or evaluation failures
// are Sample rows. Each class has an executable owner at the layer that
// produces its actual outcome; this inventory fails closed when fixture
// discovery drifts from those owners.
func TestPromOutcomeFixturesStayOutsideSampleParity(t *testing.T) {
	t.Parallel()

	root := repoRootForParity(t)
	fixtureDir := filepath.Join(root, "test", "spec", "promql")
	classes := []outcomeFixtureClass{
		{
			name: "metadata projections",
			fixtures: []string{
				"metadata_label_names_leaf_projection",
				"metadata_label_values_bucket_le",
				"metadata_label_values_collides_on_sanitized_name",
				"metadata_label_values_dotted_source_key",
				"metadata_label_values_leaf_projection",
			},
			prefix: "metadata_",
			owners: []outcomeOwner{
				{path: "internal/promql/lower_test.go", test: "TestLower"},
				{path: "internal/api/prom/handler_chdb_label_values_test.go", test: "TestLabelValues_MatchSelector_ChDB"},
			},
		},
		{
			name:     "query exemplars",
			fixtures: []string{"exemplars_basic", "exemplars_companion_fanout"},
			prefix:   "exemplars_",
			owners: []outcomeOwner{
				{path: "internal/chsql/query_exemplars_spec_test.go", test: "TestEmitQueryExemplars_Fixtures"},
				{path: "internal/api/prom/conformance_test.go", test: "TestConformance_PromExemplarsBasic"},
			},
		},
		{
			name: "duplicate-labelset errors",
			fixtures: []string{
				"ceil_duplicate_labelset",
				"label_join_duplicate_labelset",
				"label_replace_duplicate_labelset",
				"rate_multi_name_duplicate_labelset_guard",
			},
			prefix: "",
			owners: []outcomeOwner{
				{path: "internal/api/prom/handler_chdb_duplicate_labelset_test.go", test: "TestQuery_RateMultiName_DuplicateLabelset_ChDB"},
				{path: "internal/api/prom/handler_chdb_instant_duplicate_labelset_test.go", test: "TestQuery_LabelRewrite_DuplicateLabelset_ChDB"},
				{path: "internal/api/prom/handler_chdb_instant_duplicate_labelset_test.go", test: "TestQueryRange_DuplicateLabelsetCorpusShapes_ChDB"},
			},
		},
	}

	for _, class := range classes {
		class := class
		t.Run(class.name, func(t *testing.T) {
			assertOutcomeOwners(t, root, class.owners)
			assertOutcomeFixtureInventory(t, fixtureDir, class)
		})
	}
}

type outcomeFixtureClass struct {
	name     string
	fixtures []string
	prefix   string
	owners   []outcomeOwner
}

type outcomeOwner struct {
	path string
	test string
}

func assertOutcomeOwners(t *testing.T, root string, owners []outcomeOwner) {
	t.Helper()
	for _, owner := range owners {
		body, err := os.ReadFile(filepath.Join(root, owner.path))
		if err != nil {
			t.Fatalf("read outcome owner %s: %v", owner.path, err)
		}
		if !strings.Contains(string(body), "func "+owner.test+"(") {
			t.Errorf("outcome owner %s no longer defines %s", owner.path, owner.test)
		}
	}
}

func assertOutcomeFixtureInventory(t *testing.T, dir string, class outcomeFixtureClass) {
	t.Helper()
	want := append([]string(nil), class.fixtures...)
	sort.Strings(want)
	got := make([]string, 0, len(want))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixture directory %s: %v", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".txtar") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".txtar")
		if matchesOutcomeFixtureClass(class, name) {
			got = append(got, name)
			assertNotSampleParityFixture(t, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s fixture inventory = %v, want closed set %v; bind any new outcome-shaped fixture to an executable owner before it can join sample parity", class.name, got, want)
	}
}

func matchesOutcomeFixtureClass(class outcomeFixtureClass, name string) bool {
	if class.prefix != "" {
		return strings.HasPrefix(name, class.prefix)
	}
	return strings.Contains(name, "duplicate_labelset")
}

func assertNotSampleParityFixture(t *testing.T, path string) {
	t.Helper()
	c, err := spec.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	if _, ok := c.Section("parity"); ok {
		t.Errorf("%s has -- parity --; endpoint and error outcomes must not fabricate Sample-row parity", filepath.Base(path))
	}
}

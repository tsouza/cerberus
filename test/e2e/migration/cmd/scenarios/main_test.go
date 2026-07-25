package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// testFeatureDir holds the fixture features the enumerator's own contract is
// asserted against — a Scenario Outline, feature-level tags, scenario-level
// tags, and tags belonging to no vocabulary.
const testFeatureDir = "testdata/features"

// enumerate runs the enumerator over the fixture features.
func enumerate(t *testing.T, dir string) Document {
	t.Helper()

	root, err := lib.RepoRoot()
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	doc, err := Enumerate(root, dir)
	if err != nil {
		t.Fatalf("Enumerate(%s): %v", dir, err)
	}
	return doc
}

// find returns the record with the given scenario name.
func find(t *testing.T, doc Document, name string) Scenario {
	t.Helper()

	for _, sc := range doc.Scenarios {
		if sc.Name == name {
			return sc
		}
	}
	t.Fatalf("no scenario named %q in %d records", name, len(doc.Scenarios))
	return Scenario{}
}

// TestEnumerateCollapsesAnOutlineToOneRecord asserts a Scenario Outline emits a
// single record carrying its keyword and its step list — the Examples rows are
// varying data, not separate scenarios, so a two-row table must not inflate the
// coverage count the ratchet reads.
func TestEnumerateCollapsesAnOutlineToOneRecord(t *testing.T) {
	t.Parallel()

	doc := enumerate(t, testFeatureDir)
	if doc.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", doc.SchemaVersion, SchemaVersion)
	}
	if len(doc.Scenarios) != 3 {
		t.Fatalf("enumerated %d records, want 3 (one outline + two scenarios)", len(doc.Scenarios))
	}

	outline := find(t, doc, "an outline collapses to one record")
	if outline.Keyword != "Scenario Outline" {
		t.Errorf("Keyword = %q, want %q", outline.Keyword, "Scenario Outline")
	}
	wantSteps := []Step{
		{Type: "Context", Text: "the committed fixtures for the <case>"},
		{Type: "Action", Text: "the operator runs the offline command"},
		{Type: "Outcome", Text: "the artifact matches its committed golden"},
		{Type: "Conjunction", Text: "every entry names the source it came from"},
	}
	if !reflect.DeepEqual(outline.Steps, wantSteps) {
		t.Errorf("Steps = %+v, want %+v", outline.Steps, wantSteps)
	}
}

// TestEnumerateCollectsTagsPlacedOnTheExamplesBlock asserts a tag written on
// a Scenario Outline's Examples block — legal Gherkin, distinct from a tag on
// the Scenario line — reaches the same partitioned vocabularies a
// Scenario-level tag does. Without it, a suppression tag (@wip et al.) placed
// on an Examples block would be invisible to the coverage ratchet's
// unknown-tag check: it would never appear in UnknownTags at all, regardless
// of spelling or casing.
func TestEnumerateCollectsTagsPlacedOnTheExamplesBlock(t *testing.T) {
	t.Parallel()

	doc := enumerate(t, testFeatureDir)
	outline := find(t, doc, "an outline collapses to one record")
	if want := []string{"@examples-only-tag"}; !reflect.DeepEqual(outline.UnknownTags, want) {
		t.Errorf("UnknownTags = %v, want %v (the Examples-block tag)", outline.UnknownTags, want)
	}
}

// TestEnumeratePartitionsTheTagVocabularies asserts feature-level tags are
// inherited, scenario-level tags are merged in, each vocabulary lands in its own
// sorted list with the prefix stripped, and a tag matching no vocabulary is
// carried verbatim into UnknownTags rather than dropped.
func TestEnumeratePartitionsTheTagVocabularies(t *testing.T) {
	t.Parallel()

	doc := enumerate(t, testFeatureDir)

	tagged := find(t, doc, "a scenario carrying its own tags")
	if want := []string{"MIG-26"}; !reflect.DeepEqual(tagged.Stories, want) {
		t.Errorf("Stories = %v, want %v (inherited from the feature)", tagged.Stories, want)
	}
	if want := []string{"tier1"}; !reflect.DeepEqual(tagged.Tiers, want) {
		t.Errorf("Tiers = %v, want %v", tagged.Tiers, want)
	}
	if want := []string{"already-otel", "regulated-airgapped"}; !reflect.DeepEqual(tagged.Archetypes, want) {
		t.Errorf("Archetypes = %v, want %v (sorted, prefix stripped)", tagged.Archetypes, want)
	}
	// `@archetype:Bad-Name` matches no vocabulary — the archetype form is
	// lower-case — so it must surface rather than be accepted as an archetype.
	if want := []string{"@archetype:Bad-Name", "@nonvocabulary"}; !reflect.DeepEqual(tagged.UnknownTags, want) {
		t.Errorf("UnknownTags = %v, want %v", tagged.UnknownTags, want)
	}

	// The second scenario in the same feature overrides nothing: it inherits
	// the feature's story and adds its own tier, so both tiers are present.
	other := find(t, doc, "a scenario with no outcome step")
	if want := []string{"tier0", "tier1"}; !reflect.DeepEqual(other.Tiers, want) {
		t.Errorf("Tiers = %v, want %v (feature tier merged with the scenario's)", other.Tiers, want)
	}
	if len(other.Archetypes) != 0 {
		t.Errorf("Archetypes = %v, want none", other.Archetypes)
	}
	for _, step := range other.Steps {
		if step.Type == "Outcome" {
			t.Fatalf("scenario %q reports an Outcome step %q; the ratchet's no-Then detector would never fire",
				other.Name, step.Text)
		}
	}
}

// TestEnumerateEmitsRepositoryRelativePathsInOrder asserts the records are
// ordered by (feature, line) with repository-relative feature paths, which is
// what makes two runs byte-identical.
func TestEnumerateEmitsRepositoryRelativePathsInOrder(t *testing.T) {
	t.Parallel()

	doc := enumerate(t, testFeatureDir)

	const outlineFeature = "test/e2e/migration/cmd/scenarios/testdata/features/outline.feature"
	const tagsFeature = "test/e2e/migration/cmd/scenarios/testdata/features/tags.feature"
	wantOrder := []struct {
		feature string
		line    int
	}{
		{outlineFeature, 5},
		{tagsFeature, 6},
		{tagsFeature, 12},
	}
	if len(doc.Scenarios) != len(wantOrder) {
		t.Fatalf("enumerated %d records, want %d", len(doc.Scenarios), len(wantOrder))
	}
	for i, want := range wantOrder {
		if doc.Scenarios[i].Feature != want.feature || doc.Scenarios[i].Line != want.line {
			t.Errorf("record %d = %s:%d, want %s:%d",
				i, doc.Scenarios[i].Feature, doc.Scenarios[i].Line, want.feature, want.line)
		}
	}
}

// TestMarshalIsByteIdenticalAcrossRuns asserts the emitted JSON is stable, so
// the coverage ratchet diffs real drift rather than map-iteration order.
func TestMarshalIsByteIdenticalAcrossRuns(t *testing.T) {
	t.Parallel()

	first, err := Marshal(enumerate(t, testFeatureDir))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	second, err := Marshal(enumerate(t, testFeatureDir))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("two enumerations of the same tree produced different bytes")
	}
	if first[len(first)-1] != '\n' {
		t.Error("the document does not end with a newline")
	}

	// Every list is emitted, never omitted, so a reader never has to
	// distinguish an absent field from an empty one.
	var raw struct {
		Scenarios []map[string]json.RawMessage `json:"scenarios"`
	}
	if err := json.Unmarshal(first, &raw); err != nil {
		t.Fatalf("unmarshal the emitted document: %v", err)
	}
	for _, sc := range raw.Scenarios {
		for _, field := range []string{"stories", "tiers", "archetypes", "unknown_tags", "steps"} {
			if string(sc[field]) == "null" {
				t.Errorf("field %q is null; want an empty array", field)
			}
		}
	}
}

// TestEnumerateReportsAMissingFeatureDirectory asserts a bad --features path
// fails loudly instead of emitting an empty, silently-passing document.
func TestEnumerateReportsAMissingFeatureDirectory(t *testing.T) {
	t.Parallel()

	root, err := lib.RepoRoot()
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	if doc, err := Enumerate(root, "testdata/absent"); err == nil {
		t.Fatalf("Enumerate over a missing directory returned %d records and no error", len(doc.Scenarios))
	}
}

// TestEnumerateReadsTheHarnessFeatureTree asserts the default feature directory
// resolves to the committed features, and that the tier-0 schema-render
// scenario is projected with the tags and the assertion step the runner drives.
func TestEnumerateReadsTheHarnessFeatureTree(t *testing.T) {
	t.Parallel()

	root, err := lib.RepoRoot()
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	doc, err := Enumerate(root, lib.HarnessPath(root, defaultFeatureDir))
	if err != nil {
		t.Fatalf("Enumerate the harness features: %v", err)
	}
	if len(doc.Scenarios) == 0 {
		t.Fatal("the harness feature tree enumerated no scenarios")
	}

	sc := find(t, doc, "render the schema under a schema environment")
	if want := []string{"MIG-10"}; !reflect.DeepEqual(sc.Stories, want) {
		t.Errorf("Stories = %v, want %v", sc.Stories, want)
	}
	if want := []string{"tier0"}; !reflect.DeepEqual(sc.Tiers, want) {
		t.Errorf("Tiers = %v, want %v", sc.Tiers, want)
	}
	if len(sc.UnknownTags) != 0 {
		t.Errorf("UnknownTags = %v, want none", sc.UnknownTags)
	}
	outcomes := 0
	for _, step := range sc.Steps {
		if step.Type == "Outcome" {
			outcomes++
		}
	}
	if outcomes == 0 {
		t.Error("the schema-render scenario reports no Outcome step, so it would assert nothing")
	}
}

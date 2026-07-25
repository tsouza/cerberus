// Command scenarios enumerates the Layer-14 migration Gherkin features into
// the JSON the coverage ratchet consumes. It walks the feature files with
// godog's own parser, so exactly one Gherkin parser exists in the tree, and
// emits one record per Scenario node: which story, tier and archetypes its tags
// bind, which tags belong to no vocabulary, and each step's Gherkin keyword
// type.
//
// It carries no policy. Every verdict — a story with no scenario, a tier tag
// contradicting the story map, a Scenario with no Then — belongs to
// .github/scripts/migration-e2e.mjs, which reads this output. The enumerator is
// a faithful projection of what the feature files say.
//
// Usage:
//
//	scenarios [--features DIR] [--out PATH]
//
//	--features DIR  Gherkin feature directory (default: the harness feature
//	                directory under the repository root)
//	--out PATH      write JSON here (default: stdout)
//
// Exit: 0 when every feature parsed, 1 on a parse error or an unreadable path.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/cucumber/godog"
	messages "github.com/cucumber/messages/go/v21"

	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// SchemaVersion is stamped into every emitted document. Readers reject a
// version they do not understand rather than silently misreading a new shape.
const SchemaVersion = 1

// defaultFeatureDir is the harness feature directory, relative to the harness
// tree, used when --features is not given.
const defaultFeatureDir = "features"

// jsonIndent matches the repository's other machine-readable artifacts, so the
// output diffs cleanly in review.
const jsonIndent = "  "

// Tag vocabularies. A tag matching none of them is reported verbatim in
// UnknownTags, which is how a banned suppression tag (@wip, @skip, @manual) or
// a plain typo reaches the ratchet instead of being silently dropped.
var (
	storyTagRe     = regexp.MustCompile(`^@(MIG-\d{2})$`)
	tierTagRe      = regexp.MustCompile(`^@(tier[0-2])$`)
	archetypeTagRe = regexp.MustCompile(`^@archetype:([a-z0-9-]+)$`)
)

// Step is one Gherkin step: its keyword type (Context, Action, Outcome,
// Conjunction, Unknown) and its text. The ratchet derives "this scenario
// asserts something" from the presence of an Outcome step, and enforces the
// no-number / no-operator rules over the text.
type Step struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Scenario is one Scenario or Scenario Outline node. A Scenario Outline
// collapses to a single record: the Examples rows are data, not scenarios.
type Scenario struct {
	Feature     string   `json:"feature"`
	Line        int      `json:"line"`
	Keyword     string   `json:"keyword"`
	Name        string   `json:"name"`
	Stories     []string `json:"stories"`
	Tiers       []string `json:"tiers"`
	Archetypes  []string `json:"archetypes"`
	UnknownTags []string `json:"unknown_tags"`
	Steps       []Step   `json:"steps"`
}

// Document is the emitted JSON: a schema version plus every scenario, ordered
// by (feature, line) so two runs over the same tree are byte-identical.
type Document struct {
	SchemaVersion int        `json:"schema_version"`
	Scenarios     []Scenario `json:"scenarios"`
}

func main() {
	features := flag.String("features", "", "Gherkin feature directory (default: the harness feature directory)")
	out := flag.String("out", "", "write the JSON here (default: stdout)")
	flag.Parse()

	if err := run(*features, *out); err != nil {
		fmt.Fprintf(os.Stderr, "scenarios: %v\n", err)
		os.Exit(1)
	}
}

// run enumerates featureDir and writes the document to outPath (or stdout).
func run(featureDir, outPath string) error {
	root, err := lib.RepoRoot()
	if err != nil {
		return err
	}
	if featureDir == "" {
		featureDir = lib.HarnessPath(root, defaultFeatureDir)
	}

	doc, err := Enumerate(root, featureDir)
	if err != nil {
		return err
	}
	data, err := Marshal(doc)
	if err != nil {
		return err
	}
	if outPath == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	return os.WriteFile(outPath, data, outFileMode)
}

// outFileMode is the permission a written JSON document gets.
const outFileMode = 0o644

// Enumerate parses every feature under featureDir and projects it into a
// Document whose paths are relative to root.
func Enumerate(root, featureDir string) (Document, error) {
	suite := godog.TestSuite{Options: &godog.Options{Paths: []string{featureDir}}}
	features, err := suite.RetrieveFeatures()
	if err != nil {
		return Document{}, fmt.Errorf("parse features under %s: %w", featureDir, err)
	}

	doc := Document{SchemaVersion: SchemaVersion, Scenarios: []Scenario{}}
	for _, feature := range features {
		if feature == nil || feature.GherkinDocument == nil {
			return Document{}, errors.New("godog returned a feature with no Gherkin document")
		}
		rel, err := relativeURI(root, feature.Uri)
		if err != nil {
			return Document{}, err
		}
		doc.Scenarios = append(doc.Scenarios, scenariosOf(feature.GherkinDocument, rel)...)
	}
	sort.Slice(doc.Scenarios, func(i, j int) bool {
		if doc.Scenarios[i].Feature != doc.Scenarios[j].Feature {
			return doc.Scenarios[i].Feature < doc.Scenarios[j].Feature
		}
		return doc.Scenarios[i].Line < doc.Scenarios[j].Line
	})
	return doc, nil
}

// Marshal renders a document as indented JSON with a trailing newline. HTML
// escaping is off so a Scenario Outline placeholder stays readable as
// `<case>` in an artifact a reviewer reads.
func Marshal(doc Document) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", jsonIndent)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("marshal scenarios: %w", err)
	}
	return buf.Bytes(), nil
}

// scenariosOf projects one parsed feature into its Scenario records. Feature
// tags are merged into every scenario's tag set, since Gherkin inherits them.
func scenariosOf(doc *messages.GherkinDocument, feature string) []Scenario {
	if doc.Feature == nil {
		return nil
	}
	featureTags := tagNames(doc.Feature.Tags)

	out := make([]Scenario, 0, len(doc.Feature.Children))
	for _, child := range doc.Feature.Children {
		if child == nil || child.Scenario == nil {
			continue
		}
		sc := child.Scenario
		tags := append(append([]string{}, featureTags...), tagNames(sc.Tags)...)
		stories, tiers, archetypes, unknown := partitionTags(tags)

		steps := make([]Step, 0, len(sc.Steps))
		for _, st := range sc.Steps {
			if st == nil {
				continue
			}
			steps = append(steps, Step{Type: st.KeywordType.String(), Text: st.Text})
		}

		out = append(out, Scenario{
			Feature:     feature,
			Line:        lineOf(sc.Location),
			Keyword:     strings.TrimSpace(sc.Keyword),
			Name:        sc.Name,
			Stories:     stories,
			Tiers:       tiers,
			Archetypes:  archetypes,
			UnknownTags: unknown,
			Steps:       steps,
		})
	}
	return out
}

// partitionTags splits a tag list into the three vocabularies plus everything
// that matches none of them. Each list is sorted and deduplicated; a tag
// carrying no vocabulary is preserved verbatim, `@` included.
func partitionTags(tags []string) (stories, tiers, archetypes, unknown []string) {
	storySet := map[string]struct{}{}
	tierSet := map[string]struct{}{}
	archetypeSet := map[string]struct{}{}
	unknownSet := map[string]struct{}{}

	for _, tag := range tags {
		switch {
		case storyTagRe.MatchString(tag):
			storySet[storyTagRe.FindStringSubmatch(tag)[1]] = struct{}{}
		case tierTagRe.MatchString(tag):
			tierSet[tierTagRe.FindStringSubmatch(tag)[1]] = struct{}{}
		case archetypeTagRe.MatchString(tag):
			archetypeSet[archetypeTagRe.FindStringSubmatch(tag)[1]] = struct{}{}
		default:
			unknownSet[tag] = struct{}{}
		}
	}
	return sortedSet(storySet), sortedSet(tierSet), sortedSet(archetypeSet), sortedSet(unknownSet)
}

// sortedSet renders a set as a sorted slice, never nil, so the JSON always
// carries `[]` rather than `null`.
func sortedSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lineOf reads a Gherkin node's 1-indexed source line, tolerating the absent
// location a malformed document would carry.
func lineOf(loc *messages.Location) int {
	if loc == nil {
		return 0
	}
	return int(loc.Line)
}

// tagNames extracts the names of feature-level tags.
func tagNames(tags []*messages.Tag) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if tag != nil {
			out = append(out, tag.Name)
		}
	}
	return out
}

// relativeURI renders a parsed feature's URI as a repository-relative path with
// forward slashes. godog echoes back the path it walked, which is absolute or
// relative to the working directory depending on what --features was given.
func relativeURI(root, uri string) (string, error) {
	abs, err := filepath.Abs(uri)
	if err != nil {
		return "", fmt.Errorf("resolve feature path %s: %w", uri, err)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", fmt.Errorf("relate feature path %s to %s: %w", abs, root, err)
	}
	return filepath.ToSlash(rel), nil
}

package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A workflow that gates its real steps behind a positive `dorny/paths-filter`
// key ("run the suite only when these paths changed") is a required check
// that reports green without executing anything on every other PR. That is
// by design for a chart change — and a hole for the gate's OWN code: a PR that
// edits only `chart-render-assert.mjs` or `lib/helm-docs.mjs` got a green
// `chart-validate` that executed neither, because the filter named some of the
// scripts the steps run and not the others.
//
// The rule: every `node .github/scripts/<x>.mjs` a filter-gated step runs,
// together with every local module it imports (transitively — a helper under
// `lib/` is as much the gate's code as the entry script), must be matched by
// that filter key's globs. Derived from the workflow rather than listed, so
// the next script added behind the filter is covered or the test says which
// glob is missing.

// pathsFilterAction is the action whose `filters:` input this test parses.
const pathsFilterAction = "dorny/paths-filter@"

// filterGateRE matches a step condition of the shape
// `steps.<id>.outputs.<key> == 'true'`.
var filterGateRE = regexp.MustCompile(`steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)\s*==\s*'true'`)

// localImportRE matches one relative ESM import inside a script.
var localImportRE = regexp.MustCompile(`(?m)^import\s+(?:[^'"]+?\s+from\s+)?['"](\.{1,2}/[^'"]+)['"]`)

type pathsFilterStep struct {
	ID   string    `yaml:"id"`
	If   yaml.Node `yaml:"if"`
	Uses string    `yaml:"uses"`
	Run  string    `yaml:"run"`
	With struct {
		Filters string `yaml:"filters"`
	} `yaml:"with"`
}

type pathsFilterWorkflow struct {
	Jobs map[string]struct {
		Steps []pathsFilterStep `yaml:"steps"`
	} `yaml:"jobs"`
}

// globToRegexp turns a paths-filter glob into an anchored regexp: `**`
// matches across directory separators, `*` within one segment.
func globToRegexp(t *testing.T, glob string) *regexp.Regexp {
	t.Helper()
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case glob[i] == '*':
			b.WriteString("[^/]*")
		default:
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		t.Fatalf("glob %q: %v", glob, err)
	}
	return re
}

// scriptClosure returns `script` plus every `.github/scripts` module it
// imports, transitively, as repo-relative slash paths.
func scriptClosure(t *testing.T, script string) []string {
	t.Helper()
	seen := map[string]bool{}
	var walk func(string)
	walk = func(rel string) {
		if seen[rel] {
			return
		}
		seen[rel] = true
		src, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s (imported from a filter-gated step): %v", rel, err)
		}
		for _, m := range localImportRE.FindAllStringSubmatch(string(src), -1) {
			walk(filepath.ToSlash(filepath.Join(filepath.Dir(rel), m[1])))
		}
	}
	walk(script)
	out := make([]string, 0, len(seen))
	for rel := range seen {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

func TestPathsFilterCoversEveryScriptItGates(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowsDir, err)
	}
	gatedSteps := 0
	for _, entry := range entries {
		if entry.IsDir() || !isWorkflowYAML(entry.Name()) {
			continue
		}
		path := filepath.Join(workflowsDir, entry.Name())
		var workflow pathsFilterWorkflow
		if err := yaml.Unmarshal([]byte(readFileString(t, path)), &workflow); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for jobID, job := range workflow.Jobs {
			// filter step id -> key -> compiled globs
			filters := map[string]map[string][]*regexp.Regexp{}
			for _, step := range job.Steps {
				if !strings.HasPrefix(step.Uses, pathsFilterAction) || step.With.Filters == "" {
					continue
				}
				var keys map[string][]string
				if err := yaml.Unmarshal([]byte(step.With.Filters), &keys); err != nil {
					t.Fatalf("%s job %q step %q: parse filters: %v", path, jobID, step.ID, err)
				}
				filters[step.ID] = map[string][]*regexp.Regexp{}
				for key, globs := range keys {
					for _, glob := range globs {
						filters[step.ID][key] = append(filters[step.ID][key], globToRegexp(t, glob))
					}
				}
			}
			if len(filters) == 0 {
				continue
			}
			for _, step := range job.Steps {
				m := filterGateRE.FindStringSubmatch(ciLaneScalarValue(step.If))
				if m == nil {
					continue
				}
				globs, ok := filters[m[1]][m[2]]
				if !ok {
					continue // gated on something that is not a paths filter key of this job
				}
				for _, script := range mjsScriptsInvokedBy(step.Run) {
					gatedSteps++
					for _, rel := range scriptClosure(t, script) {
						matched := false
						for _, re := range globs {
							if re.MatchString(rel) {
								matched = true
								break
							}
						}
						if !matched {
							t.Errorf("%s job %q: step gated on `steps.%s.outputs.%s == 'true'` runs %s, which loads %s, "+
								"but that path is not in the %q filter. A PR changing only that file gets a green "+
								"check that never executed it — add the path to the filter (and to the lane's "+
								"package_globs in %s)",
								path, jobID, m[1], m[2], script, rel, m[2], ciLaneRegistryPath)
						}
					}
				}
			}
		}
	}
	if gatedSteps == 0 {
		t.Fatalf("found no filter-gated `node .github/scripts/*.mjs` step under %s; the rule would be vacuous",
			workflowsDir)
	}
}

func TestGlobToRegexp(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		glob, path string
		want       bool
	}{
		{"deploy/helm/**", "deploy/helm/cerberus/Chart.yaml", true},
		{"deploy/helm/**", "deploy/helmet", false},
		{".github/scripts/*.mjs", ".github/scripts/chart-publish.mjs", true},
		{".github/scripts/*.mjs", ".github/scripts/lib/helm-docs.mjs", false},
		{"**/*.md", "docs/a/b.md", true},
		{"**/*.md", "README.md", true},
		{".github/workflows/chart-ci.yml", ".github/workflows/chart-ci.yml", true},
		{".github/workflows/chart-ci.yml", ".github/workflows/chart-ci.yml.bak", false},
	} {
		if got := globToRegexp(t, test.glob).MatchString(test.path); got != test.want {
			t.Errorf("glob %q against %q = %v, want %v", test.glob, test.path, got, test.want)
		}
	}
}

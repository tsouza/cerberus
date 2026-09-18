package regression

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The docs-only short-circuit — "every changed file of this pull request is
// documentation, skip the heavy jobs" — is one decision made in six
// workflows, and what "documentation" means is declared once, in the lane
// registry's `impact_selection.known_nonimpact_globs`. Each workflow used to
// carry its own ten-line copy of that list inside a paths-filter step, and the
// copies had drifted from the registry (and would again). The pins below hold
// the single-copy shape:
//
//   - a workflow job that publishes a `docs_only` output gets it from
//     ./.github/actions/docs-only, never from an inline filter;
//   - no workflow carries a documentation negation pattern of its own;
//   - the action renders its filter from the registry — the negations it
//     would emit are exactly the registry globs (docs-only-filter.test.mjs
//     drives the script itself; this pins the action's wiring).

const (
	docsOnlyActionUses = "./.github/actions/docs-only"
	docsOnlyActionPath = repoRoot + "/.github/actions/docs-only/action.yml"
	docsOnlyScript     = "node .github/scripts/docs-only-filter.mjs"
)

// inlineDocsNegation is the tell-tale of a copied docs filter.
const inlineDocsNegation = "'!**/*.md'"

type docsOnlyWorkflow struct {
	Jobs map[string]struct {
		Outputs map[string]string `yaml:"outputs"`
		Steps   []struct {
			ID   string `yaml:"id"`
			Uses string `yaml:"uses"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func TestDocsOnlyVerdictComesFromTheSharedAction(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowsDir, err)
	}
	consumers := 0
	for _, entry := range entries {
		if entry.IsDir() || !isWorkflowYAML(entry.Name()) {
			continue
		}
		path := filepath.Join(workflowsDir, entry.Name())
		body := readFileString(t, path)
		if strings.Contains(body, inlineDocsNegation) {
			t.Errorf("%s carries an inline documentation filter (%s); the docs-only decision is made by %s "+
				"from the registry's known_nonimpact_globs, and a copy here drifts from it",
				path, inlineDocsNegation, docsOnlyActionUses)
		}
		var workflow docsOnlyWorkflow
		if err := yaml.Unmarshal([]byte(body), &workflow); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for jobID, job := range workflow.Jobs {
			expr, publishes := job.Outputs["docs_only"]
			if !publishes {
				continue
			}
			consumers++
			usesAction := ""
			for _, step := range job.Steps {
				if step.Uses == docsOnlyActionUses {
					usesAction = step.ID
				}
			}
			if usesAction == "" {
				t.Errorf("%s job %q publishes docs_only without using %s", path, jobID, docsOnlyActionUses)
				continue
			}
			want := "${{ steps." + usesAction + ".outputs.docs_only }}"
			if strings.TrimSpace(expr) != want {
				t.Errorf("%s job %q publishes docs_only as %q, not the action's own output %q",
					path, jobID, expr, want)
			}
		}
	}
	if consumers == 0 {
		t.Fatalf("no workflow job under %s publishes a docs_only output; the pin would be vacuous", workflowsDir)
	}
}

func TestDocsOnlyActionRendersItsFilterFromTheRegistry(t *testing.T) {
	t.Parallel()

	var action struct {
		Runs struct {
			Steps []struct {
				ID   string `yaml:"id"`
				Uses string `yaml:"uses"`
				Run  string `yaml:"run"`
				With struct {
					Filters   string `yaml:"filters"`
					Predicate string `yaml:"predicate-quantifier"`
				} `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal([]byte(readFileString(t, docsOnlyActionPath)), &action); err != nil {
		t.Fatalf("parse %s: %v", docsOnlyActionPath, err)
	}

	rendered := ""
	for _, step := range action.Runs.Steps {
		if strings.Contains(step.Run, docsOnlyScript) && step.ID != "" {
			rendered = step.ID
		}
		if strings.HasPrefix(step.Uses, pathsFilterAction) {
			if step.With.Predicate != "every" {
				t.Errorf("%s: the paths filter must use predicate-quantifier: every, or the `**` baseline "+
					"matches everything and the negations never fire (got %q)", docsOnlyActionPath, step.With.Predicate)
			}
			want := "${{ steps." + rendered + ".outputs.filters }}"
			if rendered == "" || strings.TrimSpace(step.With.Filters) != want {
				t.Errorf("%s: the paths filter's `filters` must be the output of the %s step (%q), got %q",
					docsOnlyActionPath, docsOnlyScript, want, step.With.Filters)
			}
		}
	}
	if rendered == "" {
		t.Fatalf("%s has no step running %s", docsOnlyActionPath, docsOnlyScript)
	}

	registry := readCILaneRegistryGlobs(t)
	if len(registry) == 0 {
		t.Fatalf("%s declares no known_nonimpact_globs", ciLaneRegistryPath)
	}
	for _, glob := range []string{"**/*.md", "docs/**", "README*"} {
		found := false
		for _, g := range registry {
			if g == glob {
				found = true
			}
		}
		if !found {
			t.Errorf("%s known_nonimpact_globs lacks %q; a change to that path alone would run every heavy lane",
				ciLaneRegistryPath, glob)
		}
	}
}

// readCILaneRegistryGlobs returns the registry's non-impact globs.
func readCILaneRegistryGlobs(t *testing.T) []string {
	t.Helper()
	var doc struct {
		ImpactSelection struct {
			KnownNonimpactGlobs []string `json:"known_nonimpact_globs"`
		} `json:"impact_selection"`
	}
	if err := json.Unmarshal([]byte(readFileString(t, ciLaneRegistryPath)), &doc); err != nil {
		t.Fatalf("parse %s: %v", ciLaneRegistryPath, err)
	}
	return doc.ImpactSelection.KnownNonimpactGlobs
}

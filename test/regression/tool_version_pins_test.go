package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The tool versions the Justfile declares are what a developer installs
// (`just install-tools`) and what the docs describe; CI installs its own copy
// of each tool by a literal in the workflow. Two literals with nothing
// between them drift: ci.yml said actionlint was "pinned in lock-step with
// Justfile's ACTIONLINT_VERSION", and nothing read either. `just` itself had
// no pin at all — forty `taiki-e/install-action` steps said `tool: just`, so
// CI ran whatever `just@latest` was that day while the Justfile that every
// lane executes was written against the version on the developer's machine.
//
// The pins below read the Justfile variables through `just --dump` and hold
// every workflow literal to them.

// toolVersionPins maps a Justfile variable to the regexp that captures the
// version wherever a workflow installs that tool.
var toolVersionPins = []struct {
	variable string
	site     *regexp.Regexp
}{
	// golangci/golangci-lint-action's `version:` input.
	{"GOLANGCI_LINT_VERSION", regexp.MustCompile(`golangci-lint-action@[^\n]*\n(?:[^\n]*\n){0,3}?\s*version:\s*(\S+)`)},
	// `go install github.com/rhysd/actionlint/cmd/actionlint@<version>`.
	{"ACTIONLINT_VERSION", regexp.MustCompile(`actionlint/cmd/actionlint@(\S+)`)},
	// taiki-e/install-action's `tool: just@<version>`.
	{"JUST_VERSION", regexp.MustCompile(`(?m)^\s*tool:\s*just(?:@(\S+))?\s*$`)},
}

func TestWorkflowToolVersionsMatchTheJustfile(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowsDir, err)
	}
	var sources []struct{ path, body string }
	for _, entry := range entries {
		if entry.IsDir() || !isWorkflowYAML(entry.Name()) {
			continue
		}
		path := filepath.Join(workflowsDir, entry.Name())
		sources = append(sources, struct{ path, body string }{path, readFileString(t, path)})
	}
	actionsDir := filepath.Join(repoRoot, ".github", "actions")
	actionEntries, err := os.ReadDir(actionsDir)
	if err != nil {
		t.Fatalf("read %s: %v", actionsDir, err)
	}
	for _, entry := range actionEntries {
		path := filepath.Join(actionsDir, entry.Name(), "action.yml")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		sources = append(sources, struct{ path, body string }{path, readFileString(t, path)})
	}

	for _, pin := range toolVersionPins {
		want := justVariable(t, pin.variable)
		sites := 0
		for _, src := range sources {
			for _, m := range pin.site.FindAllStringSubmatch(src.body, -1) {
				sites++
				got := m[1]
				if got == "" {
					t.Errorf("%s installs the tool behind %s with no version; CI then runs whatever is latest "+
						"that day while the Justfile expects %s. Pin it as `just@%s`",
						src.path, pin.variable, want, want)
					continue
				}
				if got != want {
					t.Errorf("%s pins the tool behind %s at %s, the Justfile declares %s; one of them is stale",
						src.path, pin.variable, got, want)
				}
			}
		}
		if sites == 0 {
			t.Errorf("no workflow installs the tool behind %s; the pin would be vacuous", pin.variable)
		}
	}
}

// TestJustVersionIsAReleaseNumber keeps JUST_VERSION a concrete release: a
// range or `latest` would make the pin above pass while pinning nothing.
func TestJustVersionIsAReleaseNumber(t *testing.T) {
	t.Parallel()
	v := justVariable(t, "JUST_VERSION")
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(v) {
		t.Fatalf("JUST_VERSION = %q, want a concrete major.minor.patch release", v)
	}
	if strings.HasPrefix(v, "v") {
		t.Fatalf("JUST_VERSION = %q carries a v prefix; install-action's `just@<version>` takes the bare number", v)
	}
}

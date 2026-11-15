package regression

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A `pull_request_target` run reads its workflow file from `github.sha`, the
// current tip of the pull request's base branch. A job that checks out
// `github.event.pull_request.base.sha` instead runs the scripts of the base
// recorded when the pull request's head last moved, which can be older than
// the workflow file: a script the workflow added since then is missing, and
// the job fails before it acts. Every checkout in such a workflow therefore
// names `github.sha` or leaves `ref` unset (which defaults to it).

// staleBaseRef is the checkout ref that resolves to the pull request's
// recorded base rather than the base branch's current tip.
const staleBaseRef = "github.event.pull_request.base.sha"

func TestPullRequestTargetCheckoutsUseTheWorkflowCommit(t *testing.T) {
	dir := filepath.Join(repoRoot, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var targetWorkflows, checkouts int
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !(strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
			continue
		}
		path := filepath.Join(dir, name)
		var workflow struct {
			On   any `yaml:"on"`
			Jobs map[string]struct {
				Steps []struct {
					Uses string         `yaml:"uses"`
					With map[string]any `yaml:"with"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal([]byte(readFileString(t, path)), &workflow); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		triggers, _ := workflow.On.(map[string]any)
		if _, ok := triggers["pull_request_target"]; !ok {
			continue
		}
		targetWorkflows++
		for jobName, job := range workflow.Jobs {
			for _, step := range job.Steps {
				if !strings.HasPrefix(step.Uses, "actions/checkout@") {
					continue
				}
				checkouts++
				if ref := fmt.Sprint(step.With["ref"]); strings.Contains(ref, staleBaseRef) {
					t.Errorf("%s: job %s checks out %q; a pull_request_target job must check out github.sha, "+
						"the commit its workflow file is read from", path, jobName, ref)
				}
			}
		}
	}
	// The scan must have reached the requeue workflow's checkouts, or a change
	// to the parsing above would pass vacuously.
	if targetWorkflows == 0 || checkouts == 0 {
		t.Fatalf("found %d pull_request_target workflows with %d checkouts; the scan no longer reaches %s",
			targetWorkflows, checkouts, requeueWorkflowPath)
	}
}

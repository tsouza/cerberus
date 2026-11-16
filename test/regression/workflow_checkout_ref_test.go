package regression

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// A job that checks out the base side of a pull request to run trusted scripts
// with a write token must check out the base branch's current tip:
//
//   - On `pull_request_target` the workflow file is read from `github.sha`,
//     the base branch's tip, so a checkout either leaves `ref` unset (which
//     defaults to that commit) or names `${{ github.sha }}`. Any other ref
//     runs scripts the workflow file was not written against, and a head ref
//     runs the pull request's own code with the write token.
//   - On any event, `github.event.pull_request.base.sha` is the base recorded
//     when the pull request's head last moved. It can predate a script the
//     workflow runs, so the job fails before it acts. On `pull_request`, where
//     `github.sha` is the merge commit holding the pull request's code, the
//     base branch's tip is `github.event.pull_request.base.ref`.

// staleBaseRef is the checkout ref that resolves to the pull request's
// recorded base rather than the base branch's current tip.
const staleBaseRef = "${{ github.event.pull_request.base.sha }}"

// workflowCommitRef is the only explicit checkout ref a pull_request_target
// job may name: the commit its workflow file is read from.
const workflowCommitRef = "${{ github.sha }}"

// requeueWorkflowCheckouts is the number of checkout steps in
// requeueWorkflowPath (a first attempt and a retry in each of its two jobs).
// The scan must reach all of them, or a parsing change would pass vacuously.
const requeueWorkflowCheckouts = 4

func TestWorkflowCheckoutsUseTheBaseBranchTip(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowsDir, err)
	}
	targetCheckouts := map[string]int{}
	for _, entry := range entries {
		if entry.IsDir() || !isWorkflowYAML(entry.Name()) {
			continue
		}
		path := filepath.Join(workflowsDir, entry.Name())
		var workflow struct {
			On   yaml.Node `yaml:"on"`
			Jobs map[string]struct {
				Steps []stallStep `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal([]byte(readFileString(t, path)), &workflow); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		target := onDeclares(workflow.On, "pull_request_target")
		for jobName, job := range workflow.Jobs {
			for _, step := range job.Steps {
				if !isCheckoutStep(step) {
					continue
				}
				rawRef, hasRef := step.With["ref"]
				ref := toString(rawRef)
				if hasRef && ref == staleBaseRef {
					t.Errorf("%s: job %s checks out %s, the base recorded when the pull request's head "+
						"last moved; check out the base branch's tip instead", path, jobName, ref)
				}
				if !target {
					continue
				}
				targetCheckouts[entry.Name()]++
				if hasRef && ref != workflowCommitRef {
					t.Errorf("%s: pull_request_target job %s checks out %q; leave ref unset or name %s, "+
						"the commit the workflow file is read from", path, jobName, ref, workflowCommitRef)
				}
			}
		}
	}
	requeue := filepath.Base(requeueWorkflowPath)
	if got := targetCheckouts[requeue]; got < requeueWorkflowCheckouts {
		t.Fatalf("the scan reached %d pull_request_target checkouts in %s, want at least %d; "+
			"it no longer reads that workflow's trigger or steps", got, requeueWorkflowPath, requeueWorkflowCheckouts)
	}
}

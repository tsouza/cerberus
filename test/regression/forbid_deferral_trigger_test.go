package regression

import (
	"strings"
	"testing"
)

// `forbid-deferral` reads three surfaces, and one of them — the pull request
// DESCRIPTION — is not part of the git history at all. That makes its trigger
// list load-bearing in a way no other gate's is.
//
// GitHub's default `pull_request:` trigger is opened + synchronize + reopened.
// `edited`, which is the event a description change raises, is NOT in it. The
// gate ran under that default for exactly one day and the consequence was
// immediate: its own failure message asks the author to correct the
// description, doing so raised no event, and the stale red stood over a body
// that no longer contained the violation. The only way out was an empty commit
// that re-ran the entire ~20-job suite in order to re-read one paragraph. As a
// REQUIRED check that is a deadlock, because a body-only violation has no code
// to push.
//
// The failure mode is SILENCE — the gate does not run, so there is nothing red
// to investigate and nothing in a log to read. A comment saying "keep `edited`"
// cannot fail. This test can.
//
// It also pins the reason the gate has its own workflow file: the fix could
// have been `types:` on ci.yml's `pull_request:`, which would re-run every job
// in that file on every title tweak. Keeping the gate out of ci.yml is what
// makes the cheap trigger affordable, so an accidental move back is a
// regression this asserts against too.
const (
	forbidDeferralWorkflowPath = "../../.github/workflows/forbid-deferral.yml"

	// The job name branch protection and release-gate-drift.yml both resolve by
	// exact text. Renaming the job leaves the required context "Expected"
	// forever.
	forbidDeferralJob = "forbid-deferral"

	// The event a description edit raises. Absent from GitHub's default set,
	// and the entire reason this workflow is separate.
	prEditedEvent = "edited"

	// The two steps that make the lane a gate rather than a checkout: the
	// scanner's own unit guard, then the scan.
	forbidDeferralUnitStep = "node --test .github/scripts/forbid-deferral.test.mjs"
	forbidDeferralScanStep = "node .github/scripts/forbid-deferral.mjs"
)

// TestForbidDeferralRunsOnDescriptionEdits pins the trigger, the job name, the
// two steps, and the absence of the escape hatch.
func TestForbidDeferralRunsOnDescriptionEdits(t *testing.T) {
	t.Parallel()

	workflow := readFileString(t, forbidDeferralWorkflowPath)

	triggers := topLevelSection(t, workflow, "on")
	if !strings.Contains(triggers, "pull_request:") {
		t.Fatalf("%s no longer triggers on pull_request, so the gate never inspects a PR at all. "+
			"Trigger block:\n%s", forbidDeferralWorkflowPath, triggers)
	}
	if !strings.Contains(triggers, prEditedEvent) {
		t.Errorf("%s dropped the %q event from its pull_request trigger. GitHub's default set is "+
			"opened + synchronize + reopened, so a description corrected exactly as this gate's remedy "+
			"instructs raises no event, posts no check-run, and the stale failure stands over a body that "+
			"no longer contains the violation — with no code to push, the check can never go green. "+
			"Trigger block:\n%s", forbidDeferralWorkflowPath, prEditedEvent, triggers)
	}
	if !strings.Contains(triggers, "push:") {
		t.Errorf("%s no longer triggers on push, so no check-run posts on the commit being shipped and "+
			"release.yml's preflight waits on a lane that never reports. Trigger block:\n%s",
			forbidDeferralWorkflowPath, triggers)
	}

	job := workflowJobBody(t, workflow, forbidDeferralJob)
	for _, want := range []string{forbidDeferralUnitStep, forbidDeferralScanStep} {
		if !strings.Contains(job, want) {
			t.Errorf("%s job %q no longer runs %q. The unit step is the anti-hollow-green pin and the "+
				"scan step is the gate; a lane missing either posts a green context over work it did not "+
				"do. Body:\n%s", forbidDeferralWorkflowPath, forbidDeferralJob, want, job)
		}
	}
	if strings.Contains(job, continueOnError) {
		t.Errorf("%s job %q sets %s — it keeps its name and its logs and loses its verdict, which is "+
			"strictly worse than not gating at all.",
			forbidDeferralWorkflowPath, forbidDeferralJob, continueOnError)
	}
}

// TestForbidDeferralIsNotInTheMainCIWorkflow pins the split. ci.yml triggers on
// a bare `pull_request:`; a job moved back into it inherits that default set
// and silently loses the `edited` event again. Widening ci.yml's trigger
// instead would re-run its whole suite on every title tweak.
func TestForbidDeferralIsNotInTheMainCIWorkflow(t *testing.T) {
	t.Parallel()

	ci := readFileString(t, ciWorkflowPath)
	if strings.Contains(ci, "\n  "+forbidDeferralJob+":\n") {
		t.Errorf("%s defines a %q job again. Its trigger is a bare `pull_request:`, whose default event "+
			"set excludes %q, so the gate would stop re-running when a description is corrected — the "+
			"exact deadlock the split workflow exists to prevent.",
			ciWorkflowPath, forbidDeferralJob, prEditedEvent)
	}
}

// topLevelSection returns a workflow's top-level mapping under `key:` —
// everything from the unindented `key:` line up to the next unindented,
// non-blank, non-comment line.
func topLevelSection(t *testing.T, workflow, key string) string {
	t.Helper()
	lines := strings.Split(workflow, "\n")
	start := -1
	for i, line := range lines {
		if line == key+":" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("workflow has no top-level %q section", key)
	}
	var out []string
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) == "" {
			out = append(out, line)
			continue
		}
		if !strings.HasPrefix(line, " ") {
			break
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

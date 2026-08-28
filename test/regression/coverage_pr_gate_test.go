package regression

import (
	"strings"
	"testing"
)

const (
	coverageWorkflowPath = "../../.github/workflows/coverage.yml"
	// tsouza/cerberus#2634 split coverage.yml's old single `coverage` job into
	// a leading `coverage-plan` job (the always-on structural checks below)
	// plus the parallel `coverage-default`/`coverage-chdb` lane jobs and a
	// `coverage` aggregator. The structural checks this test pins moved to
	// coverage-plan with them.
	coverageJobName  = "coverage-plan"
	coverageGateTest = "node --test .github/scripts/coverage-package-floor.test.mjs"
	coverageGateRun  = "node .github/scripts/coverage-package-floor.mjs"
	// Go setup reaches the upstream Action only through the local composite
	// (tsouza/cerberus#2676), which warms GOMODCACHE so no job can archive an
	// empty module cache. `assert-go-setup-hardened.mjs` is what keeps every
	// call site on that path; this constant just has to name the same one.
	coverageSetupGo    = "uses: ./.github/actions/setup-go"
	coverageHeavyGuard = "if: steps.run_heavy.outputs.run_heavy == 'true'"
)

// TestCoveragePRGateIsNotConditionedOnHeavyCoverage pins the ordinary-PR path
// that issue #2091 found was a green no-op. The structural test, scanner, and
// Go setup they need must all run outside RUN_HEAVY; measured coverage remains
// correctly heavy-only.
func TestCoveragePRGateIsNotConditionedOnHeavyCoverage(t *testing.T) {
	t.Parallel()

	workflow := readFileString(t, coverageWorkflowPath)
	job := workflowJobBody(t, workflow, coverageJobName)
	for _, command := range []string{coverageSetupGo, coverageGateTest, coverageGateRun} {
		step := workflowStepContaining(t, job, command)
		if strings.Contains(step, coverageHeavyGuard) {
			t.Errorf("%s job %q guards %q with RUN_HEAVY, so an ordinary PR is a green no-op again. Step:\n%s",
				coverageWorkflowPath, coverageJobName, command, step)
		}
		if strings.Contains(step, continueOnError) {
			t.Errorf("%s job %q lets %q continue on error, removing its gate verdict. Step:\n%s",
				coverageWorkflowPath, coverageJobName, command, step)
		}
	}
}

func workflowStepContaining(t *testing.T, job, needle string) string {
	t.Helper()
	lines := strings.Split(job, "\n")
	at := -1
	for i, line := range lines {
		if strings.Contains(line, needle) {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatalf("workflow job contains no step with %q. Body:\n%s", needle, job)
	}
	start := at
	for start > 0 && !strings.HasPrefix(lines[start], "      - ") {
		start--
	}
	end := at + 1
	for end < len(lines) && !strings.HasPrefix(lines[end], "      - ") {
		end++
	}
	return strings.Join(lines[start:end], "\n")
}

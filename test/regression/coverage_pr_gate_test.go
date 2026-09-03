package regression

import (
	"slices"
	"strings"
	"testing"
)

const (
	coverageWorkflowPath = "../../.github/workflows/coverage.yml"
	// tsouza/cerberus#2634 split coverage.yml's old single `coverage` job into
	// a leading `coverage-plan` job plus the parallel
	// `coverage-default`/`coverage-chdb` lane jobs and a `coverage`
	// aggregator; tsouza/cerberus#2987 then moved the enrollment SCAN out of
	// `coverage-plan` into its own `coverage-enrollment` job, leaving the gate
	// scripts' own `node --test` suites behind. Both jobs still have to run
	// outside RUN_HEAVY, which is what the test below pins.
	coveragePlanJobName       = "coverage-plan"
	coverageEnrollmentJobName = "coverage-enrollment"
	coverageAggregatorJobName = "coverage"
	coverageGateTest          = "node --test .github/scripts/coverage-package-floor.test.mjs"
	coverageGateRun           = "node .github/scripts/coverage-package-floor.mjs"
	// Go setup reaches the upstream Action only through the local composite
	// (tsouza/cerberus#2676), which warms GOMODCACHE so no job can archive an
	// empty module cache. `assert-go-setup-hardened.mjs` is what keeps every
	// call site on that path; this constant just has to name the same one.
	coverageSetupGo    = "uses: ./.github/actions/setup-go"
	coverageHeavyGuard = "if: steps.run_heavy.outputs.run_heavy == 'true'"
	// The aggregator step that turns coverage-enrollment's result into the
	// required `coverage` context's verdict, and the profile upload that must
	// already have happened by the time it fails.
	coverageEnrollmentVerdictStep = "Report the package floor enrollment verdict"
	coverageProfileUploadStep     = "Upload merged coverage profile"
	coverageEnrollmentResultExpr  = "needs.coverage-enrollment.result"
)

// coverageMeasuringJobs are the jobs that produce a coverage profile. None of
// them may depend on the enrollment scan — see
// TestCoverageEnrollmentDoesNotGateTheMeasuringLanes.
var coverageMeasuringJobs = []string{
	"coverage-plan",
	"coverage-default",
	"coverage-chdb",
	"coverage-chdb-ratchet",
}

// TestCoveragePRGateIsNotConditionedOnHeavyCoverage pins the ordinary-PR path
// that issue #2091 found was a green no-op. The structural scan, its own unit
// suite, and the Go setup they need must all run outside RUN_HEAVY; measured
// coverage remains correctly heavy-only.
func TestCoveragePRGateIsNotConditionedOnHeavyCoverage(t *testing.T) {
	t.Parallel()

	workflow := readFileString(t, coverageWorkflowPath)
	for _, tc := range []struct {
		job      string
		commands []string
	}{
		{job: coverageEnrollmentJobName, commands: []string{coverageSetupGo, coverageGateRun}},
		{job: coveragePlanJobName, commands: []string{coverageSetupGo, coverageGateTest}},
	} {
		job := workflowJobBody(t, workflow, tc.job)
		for _, command := range tc.commands {
			step := workflowStepContaining(t, job, command)
			if strings.Contains(step, coverageHeavyGuard) {
				t.Errorf("%s job %q guards %q with RUN_HEAVY, so an ordinary PR is a green no-op again. Step:\n%s",
					coverageWorkflowPath, tc.job, command, step)
			}
			if strings.Contains(step, continueOnError) {
				t.Errorf("%s job %q lets %q continue on error, removing its gate verdict. Step:\n%s",
					coverageWorkflowPath, tc.job, command, step)
			}
		}
	}
}

// TestCoverageEnrollmentDoesNotGateTheMeasuringLanes pins tsouza/cerberus#2987.
//
// The enrollment scan used to be a step inside `coverage-plan`, which every
// measuring job depends on with a plain `if:` — implicit `success()`. A
// statement-carrying package with no committed floor therefore failed
// coverage-plan, skipped all three lane jobs, and left CI unable to produce
// the merged profile `just update-coverage-floor` derives the ledger FROM: no
// floor, no lanes, no profile, no floor, on every trigger.
//
// The fix is a separation, not a relaxation, so this test pins both halves:
// no measuring job may depend on the enrollment scan, AND the required
// `coverage` context must still fail when that scan does — after the profile
// has been merged and uploaded, never instead of it.
func TestCoverageEnrollmentDoesNotGateTheMeasuringLanes(t *testing.T) {
	t.Parallel()

	workflow := readCILaneWorkflows(t)[".github/workflows/coverage.yml"]

	enrollment, ok := workflow.Jobs[coverageEnrollmentJobName]
	if !ok {
		t.Fatalf("%s has no %q job; the enrollment scan must be judged outside the jobs that measure",
			coverageWorkflowPath, coverageEnrollmentJobName)
	}
	if needs := ciLaneNeeds(t, coverageWorkflowPath, coverageEnrollmentJobName, enrollment.Needs); len(needs) > 0 {
		t.Errorf("%s job %q declares needs %v; a leaf job cannot be starved of a run by a failure elsewhere",
			coverageWorkflowPath, coverageEnrollmentJobName, needs)
	}

	for _, jobID := range coverageMeasuringJobs {
		job, ok := workflow.Jobs[jobID]
		if !ok {
			t.Fatalf("%s has no %q job", coverageWorkflowPath, jobID)
		}
		needs := ciLaneNeeds(t, coverageWorkflowPath, jobID, job.Needs)
		if slices.Contains(needs, coverageEnrollmentJobName) {
			t.Errorf("%s job %q depends on %q (needs %v), so a missing floor skips the very lane whose "+
				"profile is the only way to obtain one — tsouza/cerberus#2987 all over again",
				coverageWorkflowPath, jobID, coverageEnrollmentJobName, needs)
		}
	}

	aggregator, ok := workflow.Jobs[coverageAggregatorJobName]
	if !ok {
		t.Fatalf("%s has no %q job", coverageWorkflowPath, coverageAggregatorJobName)
	}
	aggregatorNeeds := ciLaneNeeds(t, coverageWorkflowPath, coverageAggregatorJobName, aggregator.Needs)
	if !slices.Contains(aggregatorNeeds, coverageEnrollmentJobName) {
		t.Errorf("%s job %q does not depend on %q (needs %v); %q is the required context, so a missing "+
			"floor would stop blocking the merge",
			coverageWorkflowPath, coverageAggregatorJobName, coverageEnrollmentJobName,
			aggregatorNeeds, coverageAggregatorJobName)
	}

	verdictAt := coverageStepIndex(t, aggregator.Steps, coverageEnrollmentVerdictStep)
	uploadAt := coverageStepIndex(t, aggregator.Steps, coverageProfileUploadStep)
	if verdictAt < uploadAt {
		t.Errorf("%s job %q runs %q (step %d) before %q (step %d); the profile an unfloored package's "+
			"author needs must be uploaded before the enrollment verdict fails the job",
			coverageWorkflowPath, coverageAggregatorJobName, coverageEnrollmentVerdictStep, verdictAt+1,
			coverageProfileUploadStep, uploadAt+1)
	}
	for _, step := range []int{verdictAt, uploadAt} {
		if condition := ciLaneScalarValue(aggregator.Steps[step].If); !strings.Contains(condition, "always()") {
			t.Errorf("%s job %q step %q has condition %q, which does not survive the failing floor gate above it",
				coverageWorkflowPath, coverageAggregatorJobName, aggregator.Steps[step].Name, condition)
		}
	}

	verdict := aggregator.Steps[verdictAt].Run
	if !strings.Contains(verdict, "exit 1") {
		t.Errorf("%s job %q step %q never exits non-zero, so a failed %q would report green on the "+
			"required context. Run:\n%s",
			coverageWorkflowPath, coverageAggregatorJobName, coverageEnrollmentVerdictStep,
			coverageEnrollmentJobName, verdict)
	}
	body := workflowJobBody(t, readFileString(t, coverageWorkflowPath), coverageAggregatorJobName)
	if !strings.Contains(body, coverageEnrollmentResultExpr) {
		t.Errorf("%s job %q never reads %s, so its verdict step cannot be observing the enrollment scan",
			coverageWorkflowPath, coverageAggregatorJobName, coverageEnrollmentResultExpr)
	}
}

// coverageStepIndex returns the position of the named step, failing when the
// step was renamed out from under the assertions that reference it.
func coverageStepIndex(t *testing.T, steps []ciLaneWorkflowStep, name string) int {
	t.Helper()
	for i, step := range steps {
		if step.Name == name {
			return i
		}
	}
	t.Fatalf("%s job %q has no step named %q", coverageWorkflowPath, coverageAggregatorJobName, name)
	return -1
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

package regression

import (
	"os"
	"strings"
	"testing"
)

// mutationWorkflowPath drives the gremlins mutation-testing lane.
const mutationWorkflowPath = "../../.github/workflows/mutation.yml"

// mutationRunnerPath owns the non-trivial argv and changed-line fallback that
// the workflow invokes.
const mutationRunnerPath = "../../.github/scripts/mutation-run.mjs"

// timeoutMaxFlag bounds a single mutant's test run regardless of how slow the
// mutated package's own tests are.
const timeoutMaxFlag = "--timeout-max"

// The runner must pass the flag AND bound its value by the declared ceiling.
// Pinned as independent substrings rather than as one adjacent phrase: the
// spelling of how the value reaches the argv has already changed twice — once
// when it was hoisted into a local to be reused by --timeout-coefficient, and
// again when cerberus issue #2903 made the budget MEASURED, so the argv now
// carries a derived number that the ceiling clamps rather than the ceiling
// itself. Neither is a refactor this pin has any business rejecting. What it
// must still catch is the flag disappearing, or the bounds it is clamped into
// no longer coming from the workflow's declared envelope.
//
// Both bounds are pinned because they fail in opposite directions and only one
// of them is this test's own subject. Losing the ceiling removes the OOM bound
// this test exists for. Losing the floor lets a mismeasured cycle hand out a
// budget SMALLER than the lane's analysed value, which is #2692's collapse.
const (
	mutationRunnerInvocation = "run: node .github/scripts/mutation-run.mjs"
	timeoutMaxArg            = "'--timeout-max',"
	timeoutMaxValue          = "'MUTANT_TIMEOUT_MAX'"
	timeoutMinValue          = "'MUTANT_TIMEOUT_MIN'"
	timeoutMaxDeclaration    = "MUTANT_TIMEOUT_MAX:"
	timeoutMinDeclaration    = "MUTANT_TIMEOUT_MIN:"
)

// gremlinsForkTag is the exact fork build this lane has been verified against.
// It is pinned whole rather than by family prefix because a tag name carries no
// ordering: "which tags are new enough" is not something a prefix can express,
// and every fork family so far has changed a verdict semantic this lane's
// numbers depend on. Bumping the fork therefore has to touch this line, which
// is the point -- the bump gets read against the list below rather than sliding
// through on a matching prefix.
//
// What each family in that history fixed, oldest first:
//
//   - before --timeout-max: gremlins rejects the argument outright, so the lane
//     fails on every leg. Loud, but still broken.
//   - the `timeout-max` family: accepts the bound and then charges compilation
//     to it (#2910). The deadline wrapped the whole `go test` child, so on a
//     package taking 13-16s to compile against a 15s budget, a mutant whose
//     test decides in 0.3s was recorded TIMED OUT having never run.
//   - the `run-leash` family: passes the bound to `go test -timeout`, which the
//     Go toolchain starts when the test BINARY starts, and reads the verdict
//     from the child's output rather than its exit status. `go test` reports a
//     failing test, a build failure and a test timeout all as exit 1, so taking
//     that at face value credits both of the latter as detections.
//   - the `unary-operators` family (#2930): stops mutating a prefix operator
//     against the infix meaning of its token. INVERT_BITWISE was rewriting
//     every `&chplan.Foo{...}` to `|chplan.Foo{...}`, which does not parse, and
//     the exit-status collapse above then booked each one KILLED. `Not viable:
//     0` on every leg was the tell: the status had never once been reported,
//     and three legs measured 52, 57 and 88 lost kills apiece once it was.
//   - the `viable-mutants` family (#2933): type-checks a candidate mutant's
//     whole package before emitting it, closing the 156 remaining mutants no
//     compiler accepts — string concatenation mutated to subtraction, a
//     constant mutated to zero and then divided by, a loop-control statement
//     moved somewhere it is not legal. It also runs the mutated tests with
//     `-vet=off`, which is the only entry in this list that MOVES a leg's
//     number rather than correcting its meaning: 78 INVERT_LOGICAL mutants
//     that `go vet` used to reject as "suspect and" — legal Go, and a real
//     change of behaviour — now reach a test binary and are adjudicated.
//     Reverting to an earlier tag puts them back outside the denominator,
//     which reads as a leg getting easier when it has only stopped asking.
//   - the `run-phase-timeout` family (#2944): reports WHICH of the two bounds
//     claimed a timed-out mutant. `go test -timeout` bounds the run and the
//     context deadline bounds compile+run, but both used to produce one status,
//     so a mutant that genuinely does not terminate was indistinguishable from a
//     compile that hung and from one the memory guard reaped. The fork now emits
//     RUN TIMED OUT when the test BINARY's own watchdog fired and printed so,
//     and .github/scripts/gremlins-threshold.mjs credits that and only that.
//     Reverting to an earlier tag makes every report carry the undifferentiated
//     status, and the gate FAILS on it rather than silently scoring the union:
//     the closed status set there refuses to score a report it cannot read.
const gremlinsForkTag = "github.com/tsouza/gremlins/cmd/gremlins@v0.6.0-cerberus-run-phase-timeout-consume"

// TestMutationLaneCapsPerMutantTimeout (#1294) pins the one bound that keeps the
// mutation lane from killing its own runner.
//
// gremlins derives a mutant's test timeout as `timeout-coefficient x the
// package's baseline test duration`. That scales the leash by how SLOW the
// package's tests are, which has nothing to do with how much damage a runaway
// mutant does in that time. Mutating a loop-advance statement (i++ -> i--)
// inside a scanner loop whose body appends per iteration yields a mutant that
// never terminates and allocates until the runner is out of memory; the OOM
// killer then reaps the runner agent and the job dies with no verdict at all.
//
// Measured over 91 heavy runs on 2026-07-26: 55 of 55 runner deaths were
// stalled on a lexer/scanner mutant, and the rate on the worst leg was 4 of the
// last 13 runs. --timeout-max removes the race by bounding exposure
// independently of the baseline.
//
// The pin matters because the failure it prevents does not look like a missing
// flag — it looks like flake. Without the bound the lane goes red roughly one
// push in three, on a different leg each time, and the tempting fix is to
// exclude whichever file the log names. That was tried twice and made it worse:
// excluding a file relocates its runaway mutants into whichever leg still owns
// it and burns real mutation coverage. So the flag and the fork tag that
// understands it are pinned together.
func TestMutationLaneCapsPerMutantTimeout(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(mutationWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", mutationWorkflowPath, err)
	}
	body := string(workflow)
	if !strings.Contains(body, mutationRunnerInvocation) {
		t.Errorf("%s does not invoke the pinned mutation runner %q", mutationWorkflowPath, mutationRunnerInvocation)
	}

	runner, err := os.ReadFile(mutationRunnerPath)
	if err != nil {
		t.Fatalf("read %s: %v", mutationRunnerPath, err)
	}
	runnerBody := string(runner)
	if !strings.Contains(runnerBody, timeoutMaxArg) || !strings.Contains(runnerBody, timeoutMaxValue) {
		t.Errorf("%s does not pass %s bounded by MUTANT_TIMEOUT_MAX to gremlins unleash. Without an "+
			"absolute ceiling a mutant that inverts a scanner loop-advance runs until the runner is out "+
			"of memory, and the job dies with no verdict — which reads as flake, not as a missing bound.",
			mutationRunnerPath, timeoutMaxFlag)
	}
	if !strings.Contains(runnerBody, timeoutMinValue) {
		t.Errorf("%s does not floor the measured per-mutant budget at MUTANT_TIMEOUT_MIN. The budget is "+
			"derived from a timed probe (#2903), and a probe that measures nothing must fall back to the "+
			"lane's analysed value rather than to whatever small number it happened to record — that is "+
			"#2692's budget collapse, where every mutant times out and the leg reports 0.00%%.",
			mutationRunnerPath)
	}
	for _, declaration := range []string{timeoutMinDeclaration, timeoutMaxDeclaration} {
		if !strings.Contains(body, declaration) {
			t.Errorf("%s does not declare %s for the gremlins unleash step. Both bounds are required env, "+
				"so a missing one fails every leg rather than running unbounded — loud, but still broken.",
				mutationWorkflowPath, declaration)
		}
	}

	if !strings.Contains(body, gremlinsForkTag) {
		t.Errorf("%s does not install %s. Fork tags older than the flag reject %s outright, so the lane "+
			"fails on every leg — loud, but still broken. Every tag after it is worse than loud: the "+
			"`timeout-max` family spends the bound on the compiler, so a mutant times out having never "+
			"run a line of test code (#2910); everything before `unary-operators` mutates a prefix "+
			"`&` as if it were bitwise AND, so mutants that cannot parse are booked as detections and "+
			"the leg is paid efficacy for the compiler's work (#2930); and everything before "+
			"`viable-mutants` emits 156 more mutants no compiler accepts and lets `go vet` withhold a "+
			"verdict from 78 that are legal Go, shrinking the denominator a leg is measured against "+
			"(#2933); and everything before `run-phase-timeout` reports one undifferentiated TIMED "+
			"OUT for both of the two bounds, so a mutant that genuinely does not terminate cannot be "+
			"told from a compile that hung or from one the memory guard reaped (#2944).",
			mutationWorkflowPath, gremlinsForkTag, timeoutMaxFlag)
	}
}

package regression

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A CI job and the `go test` it runs each carry their own clock, and only one
// of them is written down where a reviewer looks.
//
// `go test` defaults to aborting a test binary after 10 minutes. A job that
// declares `timeout-minutes: 20` therefore advertises twenty minutes of
// headroom the tests can never reach: at 10:00 the binary panics with
// "test timed out", the job fails at half its stated budget, and the log says
// nothing about where the missing ten minutes went. A job that declares no
// budget at all is the same bug with the sign flipped — it inherits GitHub's
// 360-minute default, so a genuinely hung test burns six hours of runner time
// before anything notices, except that it does not, because Go still stops it
// at 10.
//
// The mismatch is invisible in a diff. Raising a job's `timeout-minutes`
// because the lane got slower looks like it worked — the number went up — and
// changes nothing. #1847 was exactly that: `perf-guards` declared 20 minutes
// while `just perf-chdb` ran under the silent 10-minute default, with the lane
// already at 7m57s.
//
// So this file derives the pairing rather than restating it. It reads every
// workflow job, finds the `just` recipes that job runs, walks each recipe
// (dependencies included) for `go test` invocations, and requires that:
//
//   - a job running Go tests declares a budget at all;
//   - each `go test` in such a recipe declares its own `-timeout`; and
//   - that `-timeout` fits inside the smallest job budget that runs it, with
//     room for the abort to be written down.
//
// The last one is the point. Go's timeout is the useful one: it panics and
// dumps every goroutine's stack, which is the difference between "the perf
// lane is slow" and "TestCardinalityRatchet is blocked on chDB". The runner's
// timeout kills the process and uploads nothing. Ordering them so Go always
// fires first is what turns a red lane into a diagnosis.
const (
	// goDefaultTestTimeout is the timeout `go test` applies when the command
	// line does not. A job budget at or below it cannot be silently capped:
	// the runner is the shorter clock, which is visible in the job log.
	goDefaultTestTimeout = 10 * time.Minute

	// abortReportingMargin is the gap a recipe's `-timeout` must leave under
	// its job's budget. The panic-and-dump has to finish, and the job's
	// remaining steps have to upload it, before the runner's own timeout
	// takes the container away — so the two clocks are ordered, not merely
	// different.
	abortReportingMargin = 3 * time.Minute
)

var (
	// jobHeaderRe matches a workflow job key: exactly two spaces of indent,
	// a name, a colon, nothing else. Steps and `with:` keys sit deeper.
	jobHeaderRe = regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*$`)

	timeoutMinutesRe = regexp.MustCompile(`^\s+timeout-minutes:\s*(\S+)`)

	// justInvocationRe matches a `just <recipe>` call. The recipe name is
	// checked against the Justfile's own recipe set afterwards, so a `just`
	// appearing in English prose ("just the pushed tag") cannot masquerade
	// as an invocation.
	justInvocationRe = regexp.MustCompile(`\bjust\s+([a-z0-9][a-z0-9-]*)`)

	// justRecipeHeaderRe matches a Justfile recipe header at column 0. A
	// recipe may take parameters, so the name is followed by a space or the
	// colon that closes the header.
	justRecipeHeaderRe = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*)(\s+[^:]*)?:`)

	goTestRe = regexp.MustCompile(`\bgo test\b`)

	// goTestTimeoutRe captures the argument of a `-timeout` flag, which is
	// either a Go duration or a `{{VAR}}` reference to a Justfile variable.
	goTestTimeoutRe = regexp.MustCompile(`-timeout[= ]+(\S+)`)

	justVarRe = regexp.MustCompile(`^([A-Z0-9_]+)\s*:=\s*["']?([^"'\s#]+)`)
)

// workflowJob is one job's two relevant facts: how long the runner will wait,
// and which Justfile recipes it runs.
type workflowJob struct {
	workflow string
	name     string
	// budget is the declared `timeout-minutes`. Zero means undeclared,
	// which is a finding rather than a value.
	budget time.Duration
	// budgetIsExpression records a `timeout-minutes:` whose value is a
	// `${{ … }}` expression — the sharded lanes take theirs from the
	// matrix their generator script builds. The budget is declared, so the
	// lane is not running on GitHub's 360-minute default, but its number
	// is not in the file. Such a job still has to see a `-timeout`; only
	// the fits-inside comparison is unavailable.
	budgetIsExpression bool
	recipes            []string
}

// TestGoTestTimeoutFitsItsJobBudget is the whole gate. It fails with one line
// per offending (job, recipe, command) triple rather than at the first, so a
// sweep is one read of the output.
func TestGoTestTimeoutFitsItsJobBudget(t *testing.T) {
	t.Parallel()

	justfile := readFileString(t, justfilePath)
	recipeNames := justRecipeNames(justfile)
	justVars := justVariables(justfile)

	var problems []string
	for _, job := range workflowJobsRunningJust(t, recipeNames) {
		for _, recipe := range job.recipes {
			for _, cmd := range goTestCommands(t, justfile, recipe) {
				problems = append(problems,
					checkGoTestCommand(t, job, recipe, cmd, justVars)...)
			}
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Errorf("%d `go test` invocation(s) whose clock does not match the CI job budget that runs "+
			"them. `go test` stops a binary after %s unless told otherwise, and it is the abort that "+
			"dumps goroutine stacks — so it must always fire first, and the job's declared budget must "+
			"be the number a reader can trust:\n\n%s",
			len(problems), goDefaultTestTimeout, strings.Join(problems, "\n"))
	}
}

// checkGoTestCommand applies the three rules to one `go test` line.
func checkGoTestCommand(t *testing.T, job workflowJob, recipe, cmd string, justVars map[string]string) []string {
	t.Helper()
	where := fmt.Sprintf("%s job %q -> `just %s`", job.workflow, job.name, recipe)

	if job.budget == 0 && !job.budgetIsExpression {
		return []string{fmt.Sprintf("%s: job declares no `timeout-minutes`, so the runner waits out "+
			"GitHub's 360-minute default while `go test` stops at %s. Declare the budget the lane "+
			"actually needs.\n      %s", where, goDefaultTestTimeout, strings.TrimSpace(cmd))}
	}

	match := goTestTimeoutRe.FindStringSubmatch(cmd)
	if match == nil {
		if job.budget > 0 && job.budget <= goDefaultTestTimeout {
			// The runner is the shorter clock. Nothing is hidden: the job
			// log names the timeout that fired.
			return nil
		}
		budget := job.budget.String()
		if job.budgetIsExpression {
			budget = "matrix-supplied"
		}
		return []string{fmt.Sprintf("%s: job budget is %s but the command declares no `-timeout`, so it "+
			"silently runs under Go's %s default and the extra budget is unreachable.\n      %s",
			where, budget, goDefaultTestTimeout, strings.TrimSpace(cmd))}
	}
	if job.budgetIsExpression {
		// The number lives in the matrix generator, not here; a `-timeout`
		// is declared, which is all this file can check.
		return nil
	}

	declared, err := resolveGoTestTimeout(match[1], justVars)
	if err != nil {
		return []string{fmt.Sprintf("%s: cannot read the declared `-timeout` %q: %v.\n      %s",
			where, match[1], err, strings.TrimSpace(cmd))}
	}
	if declared+abortReportingMargin > job.budget {
		return []string{fmt.Sprintf("%s: `-timeout %s` leaves less than %s under the job's %s budget, so "+
			"the runner can kill the container mid-panic and the goroutine dump — the only artefact that "+
			"says WHERE it hung — never reaches the log.\n      %s",
			where, declared, abortReportingMargin, job.budget, strings.TrimSpace(cmd))}
	}
	return nil
}

// resolveGoTestTimeout parses a `-timeout` argument, following a `{{VAR}}`
// reference back to the Justfile variable it names.
func resolveGoTestTimeout(raw string, justVars map[string]string) (time.Duration, error) {
	if strings.HasPrefix(raw, "{{") && strings.HasSuffix(raw, "}}") {
		name := strings.TrimSpace(strings.Trim(raw, "{}"))
		value, ok := justVars[name]
		if !ok {
			return 0, fmt.Errorf("Justfile has no variable %q", name)
		}
		raw = value
	}
	return time.ParseDuration(raw)
}

// workflowJobsRunningJust returns every workflow job that invokes at least one
// of the named recipes, with the budget it declares.
func workflowJobsRunningJust(t *testing.T, recipeNames map[string]bool) []workflowJob {
	t.Helper()

	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowsDir, err)
	}

	var jobs []workflowJob
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		jobs = append(jobs, jobsInWorkflow(readFileString(t, filepath.Join(workflowsDir, name)), name, recipeNames)...)
	}
	return jobs
}

// jobsInWorkflow scans one workflow file. It is a line scanner rather than a
// YAML parse because the only structure it needs is "which job am I inside",
// which the two-space job header gives directly — and because a dependency-free
// scanner cannot disagree with the YAML the runner actually reads about
// anything subtler than that.
func jobsInWorkflow(workflow, filename string, recipeNames map[string]bool) []workflowJob {
	var (
		jobs    []workflowJob
		current *workflowJob
	)
	flush := func() {
		if current != nil && len(current.recipes) > 0 {
			jobs = append(jobs, *current)
		}
	}
	for _, line := range strings.Split(workflow, "\n") {
		if header := jobHeaderRe.FindStringSubmatch(line); header != nil {
			flush()
			current = &workflowJob{workflow: filename, name: header[1]}
			continue
		}
		if current == nil {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if minutes := timeoutMinutesRe.FindStringSubmatch(line); minutes != nil &&
			current.budget == 0 && !current.budgetIsExpression {
			if n, err := strconv.Atoi(minutes[1]); err == nil {
				current.budget = time.Duration(n) * time.Minute
			} else {
				current.budgetIsExpression = true
			}
		}
		for _, invocation := range justInvocationRe.FindAllStringSubmatch(line, -1) {
			recipe := invocation[1]
			if recipeNames[recipe] && !contains(current.recipes, recipe) {
				current.recipes = append(current.recipes, recipe)
			}
		}
	}
	flush()
	return jobs
}

// goTestCommands returns the `go test` lines a recipe runs, its dependencies
// included, with comment lines dropped.
func goTestCommands(t *testing.T, justfile, recipe string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(justRecipeBodyWithDeps(t, justfile, recipe), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !goTestRe.MatchString(trimmed) {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// justRecipeNames returns every recipe the Justfile declares, so a `just` in
// prose cannot be mistaken for an invocation.
func justRecipeNames(justfile string) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(justfile, "\n") {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "#") {
			continue
		}
		if header := justRecipeHeaderRe.FindStringSubmatch(line); header != nil {
			names[header[1]] = true
		}
	}
	return names
}

// justVariables returns the Justfile's top-level `NAME := value` assignments,
// which is how a timeout long enough to deserve a name is written down.
func justVariables(justfile string) map[string]string {
	vars := map[string]string{}
	for _, line := range strings.Split(justfile, "\n") {
		if assignment := justVarRe.FindStringSubmatch(line); assignment != nil {
			vars[assignment[1]] = assignment[2]
		}
	}
	return vars
}

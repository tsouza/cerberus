package regression

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// `ch-server-safety-setup` (strict-scan.yml, on an Ubicloud runner) died in its
// first step:
//
//	[command]/usr/bin/git -c protocol.version=2 fetch --no-tags --prune
//	    --no-recurse-submodules --depth=1 origin +<sha>:refs/remotes/pull/<n>/merge
//	##[error]The operation was canceled.
//
// The fetch printed nothing for five minutes until the job's own timeout
// cancelled it. actions/checkout wraps `git fetch` in a three-attempt retry,
// but a retry only fires on a failure, and a stalled fetch does not fail. Two
// layers turn a stall into a failure that is retried:
//
//   - GIT_HTTP_LOW_SPEED_LIMIT / GIT_HTTP_LOW_SPEED_TIME make git abort a
//     transfer that stays below the limit for that long once the request is
//     under way, which checkout's own retry absorbs. actions/checkout passes
//     the step environment, and so the workflow-level env, through to every
//     git process it starts. libcurl runs that check only while a transfer is
//     in progress (Curl_sendrecv); DNS, the TCP connect and the TLS handshake
//     are bounded by its connect timeout instead, which git does not set and
//     which defaults to five minutes (DEFAULT_CONNECT_TIMEOUT).
//   - Every checkout step in an Ubicloud job is therefore capped with a
//     step-level timeout-minutes and continue-on-error, and is followed by a
//     second checkout with the same inputs that runs only when the first one's
//     outcome is 'failure'. The runner fails a step that hits its timeout
//     (StepsRunner.RunStepAsync), and continue-on-error then records outcome
//     'failure' with conclusion 'success' (ExecutionContext.ApplyContinueOnError),
//     so the job goes on to the retry. The retry carries the same cap and no
//     continue-on-error: a second failure fails the job, within a bound.
//
// These tests pin the properties that make the protection real: every workflow
// that runs a job on an Ubicloud runner sets both variables to the values below,
// with no job- or step-level override; every checkout in such a job is a capped
// attempt followed by its retry; the cap holds one attempt's own retries, and
// both attempts fit inside the job's timeout with room for the job's own work;
// and git really does abort a stalled response under the variables, with curl's
// own low-speed error.

const (
	gitLowSpeedLimitEnv  = "GIT_HTTP_LOW_SPEED_LIMIT"
	gitLowSpeedTimeEnv   = "GIT_HTTP_LOW_SPEED_TIME"
	ubicloudRunnerPrefix = "ubicloud-"
	checkoutActionPrefix = "actions/checkout@"

	// The values every Ubicloud workflow sets: abort below 1000 B/s held for 30 s.
	wantLowSpeedLimitBytesPerSecond = 1000
	wantLowSpeedTimeSeconds         = 30

	// actions/checkout's retry helper: three attempts, sleeping a random
	// 10-20 seconds between them.
	checkoutFetchAttempts        = 3
	checkoutMaxRetrySleepSeconds = 20

	// checkoutHealthyFetchSeconds is the time a checkout that does not stall
	// may take. Healthy checkouts on the Ubicloud jobs finish in under ten
	// seconds; this leaves three times that for a slow day.
	checkoutHealthyFetchSeconds = 30

	// checkoutStepTimeoutMinutes is the step-level cap on every checkout step
	// in an Ubicloud job. It must hold one checkout whose transfer stalls on
	// every fetch attempt (each aborted by the low-speed check, with the retry
	// sleeps between them) plus a healthy fetch, so the cap never cuts short a
	// stall that checkout's own retry would absorb. The runner truncates a step
	// timeout to whole minutes, so it is an integer.
	checkoutStepTimeoutMinutes = 3

	// checkoutStepRuns is the number of checkout steps a job may run: the
	// capped attempt and its retry.
	checkoutStepRuns = 2

	// checkoutTimeoutShare is the share of a job's timeout that its checkout
	// steps, each run to its cap, may take; the rest is the job's own work.
	checkoutTimeoutShare = 0.5
)

// stallStep is the subset of a workflow step the stall guards read.
type stallStep struct {
	ID              string         `yaml:"id"`
	Name            string         `yaml:"name"`
	Uses            string         `yaml:"uses"`
	If              any            `yaml:"if"`
	With            map[string]any `yaml:"with"`
	TimeoutMinutes  any            `yaml:"timeout-minutes"`
	ContinueOnError any            `yaml:"continue-on-error"`
	Env             map[string]any `yaml:"env"`
}

type stallJob struct {
	RunsOn         any            `yaml:"runs-on"`
	TimeoutMinutes any            `yaml:"timeout-minutes"`
	Env            map[string]any `yaml:"env"`
	Steps          []stallStep    `yaml:"steps"`
}

type stallWorkflow struct {
	Env  map[string]any      `yaml:"env"`
	Jobs map[string]stallJob `yaml:"jobs"`
}

// readStallWorkflows parses every workflow file, keyed by its repository path
// (.github/workflows/<name>).
func readStallWorkflows(t *testing.T) map[string]stallWorkflow {
	t.Helper()
	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatal(err)
	}
	workflows := map[string]stallWorkflow{}
	for _, entry := range entries {
		if entry.IsDir() || !isWorkflowYAML(entry.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(workflowsDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var wf stallWorkflow
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		workflows[filepath.ToSlash(filepath.Join(".github/workflows", entry.Name()))] = wf
	}
	if len(workflows) == 0 {
		t.Fatalf("parsed no workflows under %s", workflowsDir)
	}
	return workflows
}

// runsOnUbicloud reports whether a `runs-on` value, in string or list form,
// names an Ubicloud runner.
func runsOnUbicloud(runsOn any) bool {
	labels := []any{runsOn}
	if list, ok := runsOn.([]any); ok {
		labels = list
	}
	for _, l := range labels {
		if s, _ := l.(string); strings.HasPrefix(s, ubicloudRunnerPrefix) {
			return true
		}
	}
	return false
}

// checkLowSpeedEnv fails unless env sets key to want; when optional, an absent
// key is accepted (a job or step that inherits the workflow value).
func checkLowSpeedEnv(t *testing.T, where string, env map[string]any, key string, want int, optional bool) {
	t.Helper()
	v, ok := env[key]
	if !ok && optional {
		return
	}
	if got := strings.TrimSpace(toString(v)); got != strconv.Itoa(want) {
		t.Errorf("%s: env %s = %q; want %q", where, key, got, strconv.Itoa(want))
	}
}

func TestUbicloudWorkflowsAbortStalledGitTransfers(t *testing.T) {
	ubicloudWorkflows := 0
	for name, wf := range readStallWorkflows(t) {
		isUbicloud := false
		for jobID, job := range wf.Jobs {
			if !runsOnUbicloud(job.RunsOn) {
				continue
			}
			isUbicloud = true
			where := name + ":" + jobID
			for _, key := range []struct {
				name string
				want int
			}{{gitLowSpeedLimitEnv, wantLowSpeedLimitBytesPerSecond}, {gitLowSpeedTimeEnv, wantLowSpeedTimeSeconds}} {
				checkLowSpeedEnv(t, where, job.Env, key.name, key.want, true)
				for i, s := range job.Steps {
					checkLowSpeedEnv(t, where+":step "+strconv.Itoa(i), s.Env, key.name, key.want, true)
				}
			}
		}
		if !isUbicloud {
			continue
		}
		ubicloudWorkflows++
		checkLowSpeedEnv(t, name, wf.Env, gitLowSpeedLimitEnv, wantLowSpeedLimitBytesPerSecond, false)
		checkLowSpeedEnv(t, name, wf.Env, gitLowSpeedTimeEnv, wantLowSpeedTimeSeconds, false)
	}
	if ubicloudWorkflows == 0 {
		t.Fatal("no workflow runs a job on an Ubicloud runner; the guard selects nothing")
	}
}

func isCheckoutStep(s stallStep) bool {
	return strings.HasPrefix(s.Uses, checkoutActionPrefix)
}

// workflowExpression strips the optional ${{ }} wrapper from an if value.
func workflowExpression(v any) string {
	s := strings.TrimSpace(toString(v))
	if inner, ok := strings.CutPrefix(s, "${{"); ok {
		if inner, ok = strings.CutSuffix(inner, "}}"); ok {
			s = strings.TrimSpace(inner)
		}
	}
	return s
}

// checkoutRetryCondition is the exact if a retry carries: it runs only when the
// attempt failed or hit its cap. A skipped attempt (outcome 'skipped') leaves
// the retry skipped too, and a cancelled job skips it through the implicit
// success() check.
func checkoutRetryCondition(attemptID string) string {
	return "steps." + attemptID + ".outcome == 'failure'"
}

// checkoutAttemptProblems returns why steps[i] is not a capped checkout attempt
// followed by its retry; none when it is. The attempt sets an id, the cap and
// continue-on-error: true. The step after it is the same checkout action with
// the same with: inputs, the same cap, no continue-on-error, and the if
// checkoutRetryCondition names.
func checkoutAttemptProblems(steps []stallStep, i int) []string {
	attempt := steps[i]
	var problems []string
	if attempt.ID == "" {
		problems = append(problems, "the attempt sets no id for its retry to read")
	}
	if attempt.ContinueOnError != true {
		problems = append(problems, fmt.Sprintf("the attempt's continue-on-error is %v; want true", attempt.ContinueOnError))
	}
	if attempt.TimeoutMinutes != checkoutStepTimeoutMinutes {
		problems = append(problems, fmt.Sprintf("the attempt's timeout-minutes is %v; want %d",
			attempt.TimeoutMinutes, checkoutStepTimeoutMinutes))
	}
	if i+1 >= len(steps) || !isCheckoutStep(steps[i+1]) {
		return append(problems, "no checkout retry follows the attempt")
	}
	retry := steps[i+1]
	if retry.Uses != attempt.Uses {
		problems = append(problems, fmt.Sprintf("the retry uses %q; the attempt uses %q", retry.Uses, attempt.Uses))
	}
	if !reflect.DeepEqual(retry.With, attempt.With) {
		problems = append(problems, fmt.Sprintf("the retry's with: %v differs from the attempt's %v", retry.With, attempt.With))
	}
	if retry.TimeoutMinutes != checkoutStepTimeoutMinutes {
		problems = append(problems, fmt.Sprintf("the retry's timeout-minutes is %v; want %d",
			retry.TimeoutMinutes, checkoutStepTimeoutMinutes))
	}
	if retry.ContinueOnError != nil && retry.ContinueOnError != false {
		problems = append(problems, fmt.Sprintf("the retry's continue-on-error is %v; a second failure must fail the job",
			retry.ContinueOnError))
	}
	if attempt.ID != "" {
		if got, want := workflowExpression(retry.If), checkoutRetryCondition(attempt.ID); got != want {
			problems = append(problems, fmt.Sprintf("the retry's if is %q; want %q", got, want))
		}
	}
	return problems
}

// retriedCheckoutAttempts indexes, by "<workflow path>#<job id>", the step
// indices that are capped checkout attempts with a conforming retry after them.
// Such a step's continue-on-error cannot report a failed checkout as success:
// the retry that follows it fails the job.
func retriedCheckoutAttempts(t *testing.T) map[string]map[int]bool {
	t.Helper()
	attempts := map[string]map[int]bool{}
	for path, wf := range readStallWorkflows(t) {
		for jobID, job := range wf.Jobs {
			for i, s := range job.Steps {
				if !isCheckoutStep(s) || len(checkoutAttemptProblems(job.Steps, i)) > 0 {
					continue
				}
				key := path + "#" + jobID
				if attempts[key] == nil {
					attempts[key] = map[int]bool{}
				}
				attempts[key][i] = true
			}
		}
	}
	return attempts
}

func TestUbicloudCheckoutsRetryAStalledCheckout(t *testing.T) {
	// One checkout attempt at its worst: every fetch attempt stalls mid-transfer
	// and is aborted by the low-speed check, with the longest sleep between
	// them, and then a healthy fetch.
	worstAttemptSeconds := checkoutFetchAttempts*wantLowSpeedTimeSeconds +
		(checkoutFetchAttempts-1)*checkoutMaxRetrySleepSeconds + checkoutHealthyFetchSeconds
	capSeconds := checkoutStepTimeoutMinutes * int(time.Minute/time.Second)
	if capSeconds < worstAttemptSeconds {
		t.Fatalf("the %d-minute checkout cap is shorter than one attempt at its worst (%ds): %d fetch attempts "+
			"aborting after %ds, %d sleeps of up to %ds, and a %ds healthy fetch",
			checkoutStepTimeoutMinutes, worstAttemptSeconds, checkoutFetchAttempts, wantLowSpeedTimeSeconds,
			checkoutFetchAttempts-1, checkoutMaxRetrySleepSeconds, checkoutHealthyFetchSeconds)
	}

	sites := 0
	for name, wf := range readStallWorkflows(t) {
		for jobID, job := range wf.Jobs {
			if !runsOnUbicloud(job.RunsOn) {
				continue
			}
			where := name + ":" + jobID
			jobSites := 0
			for i := 0; i < len(job.Steps); i++ {
				if !isCheckoutStep(job.Steps[i]) {
					continue
				}
				for _, p := range checkoutAttemptProblems(job.Steps, i) {
					t.Errorf("%s: checkout at step %d: %s", where, i, p)
				}
				jobSites++
				// Step past the retry, which is checked as part of its attempt.
				if i+1 < len(job.Steps) && isCheckoutStep(job.Steps[i+1]) {
					i++
				}
			}
			if jobSites == 0 {
				continue
			}
			sites += jobSites
			m, ok := job.TimeoutMinutes.(int)
			if !ok {
				t.Errorf("%s: timeout-minutes is %v; want an integer that the checkout budget can be checked against",
					where, job.TimeoutMinutes)
				continue
			}
			checkoutSeconds := jobSites * checkoutStepRuns * capSeconds
			budget := float64(m*int(time.Minute/time.Second)) * checkoutTimeoutShare
			if float64(checkoutSeconds) > budget {
				t.Errorf("%s: %d checkout step(s) capped at %d minutes take up to %ds; the job's %d-minute "+
					"timeout leaves %.0fs for checkout", where, jobSites*checkoutStepRuns,
					checkoutStepTimeoutMinutes, checkoutSeconds, m, budget)
			}
		}
	}
	if sites == 0 {
		t.Fatal("no Ubicloud job runs actions/checkout; the guard selects nothing")
	}
}

const (
	// stallProbeSeconds is the stall time the behavioural probe runs under:
	// the mechanism is the same at any value, and a short one keeps the test
	// fast.
	stallProbeSeconds = 2
	// stallProbeDeadline bounds the probe: a git that ignored the variables
	// would hang on the stalled server until this cancels it.
	stallProbeDeadline = 20 * time.Second
	// curlLowSpeedAbort is the error libcurl reports when the low-speed check
	// fires, which git passes through on stderr.
	curlLowSpeedAbort = "Operation too slow"
)

func TestGitAbortsAStalledFetchUnderTheLowSpeedEnv(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required by this guard: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	srv := &http.Server{
		ReadHeaderTimeout: stallProbeDeadline,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Accept the request and send nothing: a stalled response.
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		close(release)
		_ = srv.Close()
	})

	repo := t.TempDir()
	if out, err := exec.Command(gitPath, "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), stallProbeDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitPath, "-C", repo,
		"fetch", "--depth=1", "http://"+ln.Addr().String()+"/repo.git", "HEAD")
	cmd.Env = append(
		os.Environ(),
		gitLowSpeedLimitEnv+"="+strconv.Itoa(wantLowSpeedLimitBytesPerSecond),
		gitLowSpeedTimeEnv+"="+strconv.Itoa(stallProbeSeconds),
		"GIT_TERMINAL_PROMPT=0",
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("git fetch against a stalled server ran until the %s deadline: the low-speed env did not abort it",
			stallProbeDeadline)
	}
	if err == nil {
		t.Fatal("git fetch against a server that sends nothing succeeded")
	}
	if !strings.Contains(stderr.String(), curlLowSpeedAbort) {
		t.Fatalf("git fetch failed without curl's low-speed abort (%q); it failed for another reason: %s",
			curlLowSpeedAbort, stderr.String())
	}
}

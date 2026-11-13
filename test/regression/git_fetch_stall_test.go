package regression

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// `ch-server-safety-setup` (strict-scan.yml, on an Ubicloud runner) died twice
// in its first step:
//
//	[command]/usr/bin/git -c protocol.version=2 fetch --no-tags --prune
//	    --no-recurse-submodules --depth=1 origin +<sha>:refs/remotes/pull/<n>/merge
//	##[error]The operation was canceled.
//
// The fetch printed nothing for five minutes until the job's own timeout
// cancelled it. actions/checkout wraps `git fetch` in a three-attempt retry,
// but a retry only fires on a failure: git has no stall timeout of its own,
// so a stalled transfer never fails and the retry never runs.
// GIT_HTTP_LOW_SPEED_LIMIT / GIT_HTTP_LOW_SPEED_TIME make git abort a transfer
// that stays below the limit for that long once the request is under way, which
// turns that stall into a failure the retry absorbs. libcurl applies the check
// only after the connection is up: a stalled TCP connect or TLS handshake is not
// covered (see #4067). actions/checkout passes the step environment, and so the
// workflow-level env, through to every git process it starts.
//
// These tests pin the properties that make the protection real: every workflow
// that runs a job on an Ubicloud runner sets both variables to the values below,
// with no job- or step-level override; checkout's retries fit inside the
// shortest such job with room for the job's own work; and git really does abort
// a stalled response under them, with curl's own low-speed error.

const (
	gitLowSpeedLimitEnv  = "GIT_HTTP_LOW_SPEED_LIMIT"
	gitLowSpeedTimeEnv   = "GIT_HTTP_LOW_SPEED_TIME"
	ubicloudRunnerPrefix = "ubicloud-"

	// The values every Ubicloud workflow sets: abort below 1000 B/s held for 30 s.
	wantLowSpeedLimitBytesPerSecond = 1000
	wantLowSpeedTimeSeconds         = 30

	// actions/checkout's retry helper: three attempts, sleeping a random
	// 10-20 seconds between them.
	checkoutFetchAttempts        = 3
	checkoutMaxRetrySleepSeconds = 20

	// checkoutTimeoutShare is the share of a job's timeout a fully retried
	// checkout may take; the rest is the job's own work.
	checkoutTimeoutShare = 0.5
)

type stallStep struct {
	Env map[string]any `yaml:"env"`
}

type stallWorkflow struct {
	Env  map[string]any `yaml:"env"`
	Jobs map[string]struct {
		RunsOn         any            `yaml:"runs-on"`
		TimeoutMinutes any            `yaml:"timeout-minutes"`
		Env            map[string]any `yaml:"env"`
		Steps          []stallStep    `yaml:"steps"`
	} `yaml:"jobs"`
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
	worstCheckout := checkoutFetchAttempts*wantLowSpeedTimeSeconds +
		(checkoutFetchAttempts-1)*checkoutMaxRetrySleepSeconds

	paths, err := filepath.Glob("../../.github/workflows/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	ubicloudWorkflows := 0
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var wf stallWorkflow
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		name := filepath.Base(p)
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
			if m, ok := job.TimeoutMinutes.(int); ok {
				budget := float64(m*int(time.Minute/time.Second)) * checkoutTimeoutShare
				if float64(worstCheckout) > budget {
					t.Errorf("%s: %d fetch attempts aborting after %ds each, plus retry sleeps, take %ds; "+
						"the job's %d-minute timeout leaves %.0fs for checkout",
						where, checkoutFetchAttempts, wantLowSpeedTimeSeconds, worstCheckout, m, budget)
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

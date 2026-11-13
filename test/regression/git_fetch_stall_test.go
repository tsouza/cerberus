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
// that stays below the limit for that long, which turns the stall into a
// failure the retry absorbs. actions/checkout passes the step environment,
// and so the workflow-level env, through to every git process it starts.
//
// These tests pin the three properties that make the protection real: every
// workflow that runs a job on an Ubicloud runner sets both variables, the
// abort leaves room for checkout's retries inside the shortest such job, and
// git really does abort a stalled fetch under them.

const (
	gitLowSpeedLimitEnv  = "GIT_HTTP_LOW_SPEED_LIMIT"
	gitLowSpeedTimeEnv   = "GIT_HTTP_LOW_SPEED_TIME"
	ubicloudRunnerPrefix = "ubicloud-"

	// actions/checkout's retry helper: three attempts, sleeping a random
	// 10-20 seconds between them.
	checkoutFetchAttempts        = 3
	checkoutMaxRetrySleepSeconds = 20
)

type stallWorkflow struct {
	Env  map[string]any `yaml:"env"`
	Jobs map[string]struct {
		RunsOn         any `yaml:"runs-on"`
		TimeoutMinutes any `yaml:"timeout-minutes"`
	} `yaml:"jobs"`
}

func positiveEnvInt(t *testing.T, wf string, env map[string]any, key string) int {
	t.Helper()
	raw := strings.TrimSpace(toString(env[key]))
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		t.Errorf("%s: workflow-level env %s = %q; want a positive integer", wf, key, raw)
		return 0
	}
	return n
}

func TestUbicloudWorkflowsAbortStalledGitTransfers(t *testing.T) {
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
		isUbicloud, shortestTimeout := false, 0
		for _, job := range wf.Jobs {
			if runsOn, _ := job.RunsOn.(string); !strings.HasPrefix(runsOn, ubicloudRunnerPrefix) {
				continue
			}
			isUbicloud = true
			if m, ok := job.TimeoutMinutes.(int); ok && (shortestTimeout == 0 || m < shortestTimeout) {
				shortestTimeout = m
			}
		}
		if !isUbicloud {
			continue
		}
		ubicloudWorkflows++
		positiveEnvInt(t, name, wf.Env, gitLowSpeedLimitEnv)
		stall := positiveEnvInt(t, name, wf.Env, gitLowSpeedTimeEnv)
		if shortestTimeout == 0 || stall == 0 {
			continue
		}
		worstCheckout := checkoutFetchAttempts*stall + (checkoutFetchAttempts-1)*checkoutMaxRetrySleepSeconds
		if budget := shortestTimeout * int(time.Minute/time.Second); worstCheckout >= budget {
			t.Errorf("%s: %d fetch attempts aborting after %ds each, plus retry sleeps, take %ds; "+
				"the shortest Ubicloud job's timeout is %ds, so checkout's retries cannot run",
				name, checkoutFetchAttempts, stall, worstCheckout, budget)
		}
	}
	if ubicloudWorkflows == 0 {
		t.Fatal("no workflow runs a job on an Ubicloud runner; the guard selects nothing")
	}
}

// stallProbeSeconds is the stall time the behavioural probe runs under: the
// mechanism is the same at any value, and a short one keeps the test fast.
const (
	stallProbeSeconds = 2
	// stallProbeLimitBytesPerSecond is the workflows' own limit: a server
	// that sends nothing stays below any positive one.
	stallProbeLimitBytesPerSecond = 1000
	// stallProbeDeadline bounds the probe: a git that ignored the variables
	// would hang on the stalled server until this cancels it.
	stallProbeDeadline = 20 * time.Second
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
			// Accept the request and send nothing: the shape of the stall.
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
	run := func(ctx context.Context, env []string, args ...string) error {
		cmd := exec.CommandContext(ctx, gitPath, args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), env...)
		return cmd.Run()
	}
	if err := run(context.Background(), nil, "init", "-q"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), stallProbeDeadline)
	defer cancel()
	start := time.Now()
	err = run(ctx, []string{
		gitLowSpeedLimitEnv + "=" + strconv.Itoa(stallProbeLimitBytesPerSecond),
		gitLowSpeedTimeEnv + "=" + strconv.Itoa(stallProbeSeconds),
		"GIT_TERMINAL_PROMPT=0",
	}, "fetch", "--depth=1", "http://"+ln.Addr().String()+"/repo.git", "HEAD")
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("git fetch against a stalled server ran until the %s deadline: the low-speed env did not abort it",
			stallProbeDeadline)
	}
	if err == nil {
		t.Fatal("git fetch against a server that sends nothing succeeded")
	}
	if elapsed := time.Since(start); elapsed < stallProbeSeconds*time.Second {
		t.Fatalf("git fetch failed after %s, before the %ds stall window: it failed for another reason", elapsed, stallProbeSeconds)
	}
}

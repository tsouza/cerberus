package regression

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A `compatibility/tempo` run died here, before a single image had been built:
//
//	#1 [internal] booting buildkit
//	#1 pulling image moby/buildkit:buildx-stable-1
//	#1 ERROR: Error response from daemon: Head ".../moby/buildkit/manifests/
//	   buildx-stable-1": read: connection reset by peer
//
// `docker/setup-buildx-action` boots the docker-container driver by pulling
// `moby/buildkit:<tag>` single-attempt, inside its own bootstrap — a step no
// `run:`-level retry can reach. buildx does fall back to a locally present copy
// when that pull fails, so putting the image in the daemon first (with retry)
// is what removes the failure mode.
//
// These tests pin the three properties that make the protection real: every
// image-building job goes through the composite that owns the retry, the image
// the composite warms is the image the builder boots from, and the retry loop
// actually loops.

const (
	buildxComposite      = "./.github/actions/setup-buildx"
	buildxCompositePath  = "../../.github/actions/setup-buildx/action.yml"
	buildxUpstreamAction = "docker/setup-buildx-action"
	buildxPullScript     = "pull-buildkit-image.mjs"
)

// workflowStep is the subset of a workflow / composite-action step these guards
// read. `with` and `env` are `any`-valued because workflow YAML mixes strings,
// booleans and integers in both.
type workflowStep struct {
	Name string         `yaml:"name"`
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	With map[string]any `yaml:"with"`
	Env  map[string]any `yaml:"env"`
}

type workflowDoc struct {
	Jobs map[string]struct {
		Steps []workflowStep `yaml:"steps"`
	} `yaml:"jobs"`
}

// buildxStepEnv returns the step's env value for key as a string ("" when absent).
func buildxStepEnv(s workflowStep, key string) string {
	v, ok := s.Env[key]
	if !ok {
		return ""
	}
	return strings.TrimSpace(toString(v))
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	default:
		return ""
	}
}

// buildxWorkflowSteps returns `<workflow>:<job>` -> steps for every workflow under
// .github/workflows.
func buildxWorkflowSteps(t *testing.T) map[string][]workflowStep {
	t.Helper()

	const workflowDir = "../../.github/workflows"
	entries, err := os.ReadDir(workflowDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowDir, err)
	}

	out := map[string][]workflowStep{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := filepath.Ext(e.Name()); ext != ".yml" && ext != ".yaml" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(workflowDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var doc workflowDoc
		if err := yaml.Unmarshal(src, &doc); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for job, spec := range doc.Jobs {
			out[e.Name()+":"+job] = spec.Steps
		}
	}
	if len(out) == 0 {
		t.Fatal("no workflow parsed out of .github/workflows — the guard is scanning for a shape that no longer exists")
	}
	return out
}

// TestWorkflowsSetUpBuildxThroughTheRetryingComposite pins that no workflow
// calls `docker/setup-buildx-action` itself. Called directly, its bootstrap
// pull is single-attempt and unretryable from the workflow; the composite is
// the one place that owns the retry, so a sixth call site added tomorrow gets
// the protection instead of quietly reintroducing the flake.
func TestWorkflowsSetUpBuildxThroughTheRetryingComposite(t *testing.T) {
	t.Parallel()

	viaComposite := 0
	for where, steps := range buildxWorkflowSteps(t) {
		for _, s := range steps {
			switch {
			case strings.HasPrefix(s.Uses, buildxUpstreamAction+"@"):
				t.Errorf("%s uses %s directly. Its BuildKit bootstrap pulls %s single-attempt and one reset "+
					"connection to Docker Hub fails the job before a single image is built. Go through `uses: %s`, "+
					"which acquires the image with retry first.", where, s.Uses, "moby/buildkit:<tag>", buildxComposite)
			case s.Uses == buildxComposite:
				viaComposite++
			}
		}
	}

	if viaComposite == 0 {
		t.Fatalf("no workflow uses %s — the guard is scanning for a shape that no longer exists", buildxComposite)
	}
}

// TestBuildxCompositeAcquiresTheImageItBoots pins that the image the composite
// pre-pulls is the image the builder is told to boot from, and that the
// pre-pull runs first. Either half alone is hollow: warming one tag while
// buildx boots another protects nothing, and warming the right tag afterwards
// is too late.
func TestBuildxCompositeAcquiresTheImageItBoots(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(buildxCompositePath)
	if err != nil {
		t.Fatalf("read %s: %v", buildxCompositePath, err)
	}

	var action struct {
		Inputs map[string]struct {
			Default string `yaml:"default"`
		} `yaml:"inputs"`
		Runs struct {
			Using string         `yaml:"using"`
			Steps []workflowStep `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(src, &action); err != nil {
		t.Fatalf("parse %s: %v", buildxCompositePath, err)
	}

	const imageInput = "buildkit-image"
	const imageRef = "${{ inputs." + imageInput + " }}"

	in, ok := action.Inputs[imageInput]
	if !ok {
		t.Fatalf("%s declares no %q input — the guard is scanning for a shape that no longer exists",
			buildxCompositePath, imageInput)
	}
	if !strings.HasPrefix(in.Default, "moby/buildkit:") {
		t.Errorf("%s input %q defaults to %q; the docker-container driver boots from a moby/buildkit tag, and "+
			"the default is what every call site inherits.", buildxCompositePath, imageInput, in.Default)
	}

	prePull, setup := -1, -1
	for i, s := range action.Runs.Steps {
		switch {
		case strings.Contains(s.Run, buildxPullScript):
			prePull = i
			if got := buildxStepEnv(s, "BUILDKIT_IMAGE"); got != imageRef {
				t.Errorf("the pre-pull step passes BUILDKIT_IMAGE=%q, not %q. It must warm the very ref the "+
					"builder boots from, or the retry protects an image nothing uses.", got, imageRef)
			}
			if got := buildxStepEnv(s, "BUILDKIT_PULL_BACKOFF_SECONDS"); got != "" {
				t.Errorf("the pre-pull step overrides BUILDKIT_PULL_BACKOFF_SECONDS=%q. CI keeps the real "+
					"backoff; only the regression test drives it to zero.", got)
			}
		case strings.HasPrefix(s.Uses, buildxUpstreamAction+"@"):
			setup = i
			if got := toString(s.With["driver-opts"]); got != "image="+imageRef {
				t.Errorf("%s is invoked with driver-opts %q, not %q. Without the pin, buildx boots whatever "+
					"image its built-in default names, which the pre-pull may not have warmed.",
					buildxUpstreamAction, got, "image="+imageRef)
			}
		}
	}

	if prePull < 0 {
		t.Fatalf("%s never runs %s — the retry is gone", buildxCompositePath, buildxPullScript)
	}
	if setup < 0 {
		t.Fatalf("%s never runs %s — the guard is scanning for a shape that no longer exists",
			buildxCompositePath, buildxUpstreamAction)
	}
	if prePull > setup {
		t.Errorf("%s runs the pre-pull (step %d) AFTER %s (step %d); the bootstrap has already pulled and failed "+
			"by then.", buildxCompositePath, prePull, buildxUpstreamAction, setup)
	}
}

// TestBuildkitImagePullRetriesAndFallsBack drives the module against a stub
// `docker` on PATH, so the retry loop and the local-image fallback are asserted
// behaviourally rather than by reading the source for a plausible-looking loop.
func TestBuildkitImagePullRetriesAndFallsBack(t *testing.T) {
	t.Parallel()

	// The module's own retry budget for the TRANSPORT class. A pull that never
	// succeeds against a daemon that holds no local copy must be attempted
	// exactly this many times: fewer means the flake gets through, more means
	// an unreachable registry stalls the job.
	const pullAttempts = 5

	// The two ways the bootstrap pull fails. A transport fault earns the full
	// budget; a quota refusal earns exactly one attempt, because the window it
	// is counted over outlasts any backoff this loop can spend and every extra
	// attempt deepens the deficit (issue #1561).
	const (
		transportFault = "connection reset by peer"
		rateLimited    = "toomanyrequests: You have reached your unauthenticated pull rate limit."
		oneAttempt     = 1
	)

	cases := []struct {
		name string
		// pullSucceedsOn is the 1-based attempt at which the stub's `docker
		// pull` starts succeeding; never when larger than pullAttempts.
		pullSucceedsOn int
		// imageInDaemon makes the stub's `docker image inspect` succeed,
		// i.e. the runner already holds the image.
		imageInDaemon bool
		// failureText is what the stub's failing pull writes to stderr; it is
		// what the module classifies on.
		failureText  string
		wantExitCode int
		wantPulls    int
	}{
		{
			name:           "retries a failing pull until the budget is spent",
			pullSucceedsOn: pullAttempts + 1,
			imageInDaemon:  false,
			failureText:    transportFault,
			wantExitCode:   1,
			wantPulls:      pullAttempts,
		},
		{
			name:           "a transient failure is ridden out",
			pullSucceedsOn: 2,
			imageInDaemon:  false,
			failureText:    transportFault,
			wantExitCode:   0,
			wantPulls:      2,
		},
		{
			name:           "an image already in the daemon satisfies the postcondition",
			pullSucceedsOn: pullAttempts + 1,
			imageInDaemon:  true,
			failureText:    transportFault,
			wantExitCode:   0,
			wantPulls:      1,
		},
		{
			name: "a rate-limited pull is attempted once, not five times",
			// It would have succeeded on the second attempt. It still fails:
			// what is under test is that the loop stops spending a quota it
			// has just been told is exhausted.
			pullSucceedsOn: 2,
			imageInDaemon:  false,
			failureText:    rateLimited,
			wantExitCode:   1,
			wantPulls:      oneAttempt,
		},
		{
			name: "a locally present image still passes when the registry rate-limits",
			// The fallback is asked BEFORE the quota question, because a copy
			// in the daemon is the artefact wanted no matter why the pull
			// failed — buildx boots from exactly that copy.
			pullSucceedsOn: pullAttempts + 1,
			imageInDaemon:  true,
			failureText:    rateLimited,
			wantExitCode:   0,
			wantPulls:      1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			callLog := filepath.Join(dir, "pulls")
			inspectStatus := 1
			if tc.imageInDaemon {
				inspectStatus = 0
			}
			writeStubDocker(t, dir, callLog, tc.pullSucceedsOn, inspectStatus, tc.failureText)

			cmd := exec.Command("node", "../../.github/scripts/"+buildxPullScript)
			cmd.Env = append(
				os.Environ(),
				"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"BUILDKIT_IMAGE=moby/buildkit:buildx-stable-1",
				// Zero backoff: the loop under test is the retry count, not
				// the wall clock it burns.
				"BUILDKIT_PULL_BACKOFF_SECONDS=0",
			)
			out, err := cmd.CombinedOutput()

			exitCode := 0
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("run %s: %v (node must be on PATH — every .github/scripts module is Node ESM)",
						buildxPullScript, err)
				}
				exitCode = ee.ExitCode()
			}
			if exitCode != tc.wantExitCode {
				t.Errorf("exit code = %d, want %d\noutput:\n%s", exitCode, tc.wantExitCode, out)
			}

			logged, err := os.ReadFile(callLog)
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("read stub call log: %v", err)
			}
			pulls := len(strings.Fields(string(logged)))
			if pulls != tc.wantPulls {
				t.Errorf("`docker pull` ran %d time(s), want %d\noutput:\n%s", pulls, tc.wantPulls, out)
			}
		})
	}
}

// writeStubDocker drops a `docker` onto dir that records every `pull` and
// answers `image inspect` with inspectStatus.
func writeStubDocker(t *testing.T, dir, callLog string, pullSucceedsOn, inspectStatus int, failureText string) {
	t.Helper()

	script := strings.Join([]string{
		"#!/bin/sh",
		"case \"$1\" in",
		"  pull)",
		"    echo pull >> " + shellQuote(callLog),
		"    attempts=$(wc -l < " + shellQuote(callLog) + " | tr -d ' ')",
		"    [ \"$attempts\" -ge " + strconv.Itoa(pullSucceedsOn) + " ] && exit 0",
		"    echo " + shellQuote("stub: "+failureText) + " >&2",
		"    exit 1",
		"    ;;",
		"  image)",
		"    exit " + strconv.Itoa(inspectStatus),
		"    ;;",
		"esac",
		"echo \"stub: unexpected docker $*\" >&2",
		"exit 2",
		"",
	}, "\n")

	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub docker: %v", err)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestBuildxJobsAuthenticateBeforeBootstrapping pins that a job holding Docker
// Hub credentials establishes them BEFORE the builder is set up. The bootstrap
// pull is a Docker Hub pull like any other: run before the login it would be
// anonymous, sharing the runner-IP rate limit that the login exists to escape.
func TestBuildxJobsAuthenticateBeforeBootstrapping(t *testing.T) {
	t.Parallel()

	const loginAction = "docker/login-action"

	checked := 0
	for where, steps := range buildxWorkflowSteps(t) {
		buildx := -1
		for i, s := range steps {
			if s.Uses == buildxComposite {
				buildx = i
				break
			}
		}
		if buildx < 0 {
			continue
		}

		var late []string
		for _, s := range steps[buildx+1:] {
			if strings.HasPrefix(s.Uses, loginAction+"@") {
				late = append(late, s.Name)
			}
		}
		checked++
		if len(late) > 0 {
			sort.Strings(late)
			t.Errorf("%s logs in (%s) AFTER setting up buildx. The BuildKit bootstrap image is pulled during "+
				"setup, so it would be fetched anonymously against the shared runner-IP rate limit. Move the "+
				"login step ahead of `uses: %s`.", where, strings.Join(late, ", "), buildxComposite)
		}
	}

	if checked == 0 {
		t.Fatalf("no job uses %s — the guard is scanning for a shape that no longer exists", buildxComposite)
	}
}

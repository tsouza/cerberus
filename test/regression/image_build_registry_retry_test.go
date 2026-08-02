package regression

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Two lanes went red on main @ d939e299 with one signature — `e2e` run
// 30695961421 (`dashboard-shard`, step "Bring up k3d stack") and
// `migration-e2e` run 30695961427 (`migration-tier1`):
//
//	#5 ERROR: unexpected status from HEAD request to
//	   https://registry-1.docker.io/v2/library/golang/manifests/1.26:
//	   429 Too Many Requests
//
// `golang:1.26` is the `FROM` of Dockerfile.local — a base image BuildKit
// resolves from Docker Hub while running the build, not an image the host
// daemon pulls. `_pull-retry` / `pull-buildkit-image.mjs` already cover
// host-side pulls and the BuildKit bootstrap image; nothing covered the build's
// own `FROM` resolution, which every cerberus-image-building lane performs
// (e2e / dashboard, migration-e2e, compose-smoke, the three compatibility
// harnesses — two of them release gates).
//
// A host-side pre-pull cannot close it uniformly: the built-in `docker` driver
// resolves `FROM` from the daemon's image store, but the `docker-container`
// driver `.github/actions/setup-buildx` installs runs BuildKit in a container
// with its own content store and always goes to the registry. So the retry
// wraps the build command itself, and these guards pin that every
// build-invoking command goes through it and that it retries the transient
// class ONLY.
//
// The rate limit is NOT in that class, and issue #1561 is what that cost. A
// wrapper that retried `toomanyrequests` five times turned one refused pull
// into five refused pulls on a counter that only time decrements — across ~8
// open PRs and every image-pulling lane, the retry budget was itself holding
// the quota window open. The window is hours; the backoff is ~100 seconds; no
// number of attempts can outlast it. So a quota refusal fails on the FIRST
// attempt, and the cases below pin both halves: the transport class still
// retries, and the rate-limit class does not — even when the same output also
// names a transport fault, which BuildKit's `unexpected status from HEAD
// request … 429` always does.

const (
	buildRetryWrapper     = "build-with-registry-retry.mjs"
	buildRetryWrapperPath = "../../.github/scripts/" + buildRetryWrapper

	// The wrapper's own attempt budget for the TRANSPORT class (lib/registry.mjs
	// `registryAttempts`). Fewer lets the flake through; more parks the job on a
	// network path that is genuinely broken for this runner. A rate-limit
	// refusal is not on this budget at all — it gets one attempt.
	buildRetryAttempts = 5
	rateLimitAttempts  = 1

	// Floor for the class scan. The wrapped set is currently 13 (5 Justfile,
	// 4 compatibility harness, 2 e2e.yml, 2 bench); the floor guards against a
	// refactor that leaves the guard scanning for a shape nothing matches, so
	// it sits just below the real count rather than tracking it exactly.
	minWrappedBuildSites = 10
)

// A command that makes a builder resolve base images: `docker build`,
// `docker buildx build`, and any `docker compose … up|build` (compose builds
// every service with a `build:` stanza whose image is absent).
var (
	dockerBuildCommand   = regexp.MustCompile(`\bdocker\s+(?:buildx\s+)?build\b`)
	dockerComposeCommand = regexp.MustCompile(`\bdocker\s+compose\b.*\b(?:up|build)\b`)
)

// prosePrefixes are the line shapes that mention a docker command without
// running one: comments (shell / Justfile / YAML / JS), workflow step names,
// and echoed progress lines.
var prosePrefixes = []string{"#", "@#", "//", "- name:", "name:", "echo", "@echo", "@printf", "printf"}

func isProse(line string) bool {
	trimmed := strings.TrimSpace(line)
	for _, p := range prosePrefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}

// logicalLines joins backslash-continued lines, so a command split across a
// `\`-continuation (every multi-`-f` compose invocation in the Justfile) is
// judged as the single command the shell actually runs.
func logicalLines(src string) []string {
	var out []string
	var buf strings.Builder
	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if strings.HasSuffix(line, `\`) {
			buf.WriteString(strings.TrimSuffix(line, `\`))
			buf.WriteString(" ")
			continue
		}
		buf.WriteString(line)
		out = append(out, buf.String())
		buf.Reset()
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

// buildScanFiles walks the repo for the file kinds that can invoke a builder:
// the Justfile, every shell script, and every workflow / composite-action YAML.
func buildScanFiles(t *testing.T) []string {
	t.Helper()

	const repoRoot = "../.."
	// Vendored upstream trees and per-agent worktrees are not cerberus's
	// build entry points; scanning them would pin someone else's shapes.
	skipDirs := map[string]bool{".git": true, ".claude": true, "node_modules": true, "upstream": true, "vendor": true}

	var files []string
	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		ext := filepath.Ext(name)
		switch {
		case name == "Justfile":
		case ext == ".sh":
		case (ext == ".yml" || ext == ".yaml") && strings.Contains(filepath.ToSlash(path), "/.github/"):
		default:
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}
	if len(files) == 0 {
		t.Fatal("no Justfile / shell script / workflow found — the guard is scanning for a shape that no longer exists")
	}
	return files
}

// TestImageBuildingCommandsGoThroughTheRetryWrapper pins the class fix: every
// command that makes a builder resolve a base image runs through the wrapper.
// A new lane that shells out to `docker build` directly reintroduces exactly
// the Docker Hub 429 that took `e2e` + `migration-e2e` down, so the guard is on
// the command shape rather than on the two call sites that happened to fail.
func TestImageBuildingCommandsGoThroughTheRetryWrapper(t *testing.T) {
	t.Parallel()

	wrapped := 0
	for _, file := range buildScanFiles(t) {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, line := range logicalLines(string(src)) {
			if isProse(line) {
				continue
			}
			if !dockerBuildCommand.MatchString(line) && !dockerComposeCommand.MatchString(line) {
				continue
			}
			if strings.Contains(line, buildRetryWrapper) {
				wrapped++
				continue
			}
			t.Errorf("%s invokes a builder without the retry wrapper:\n    %s\n"+
				"Base images are resolved from Docker Hub during the build, and a 429 there fails the lane "+
				"outright. Run it through `node .github/scripts/%s <command>`, which retries registry/network "+
				"faults only.", file, strings.TrimSpace(line), buildRetryWrapper)
		}
	}

	if wrapped < minWrappedBuildSites {
		t.Errorf("only %d build invocation(s) go through %s, want at least %d — the guard is scanning for a "+
			"shape that no longer exists", wrapped, buildRetryWrapper, minWrappedBuildSites)
	}
}

// TestCiScriptModulesAcquireImagesThroughTheSharedPolicy pins the same class fix
// one layer down, for the `.mjs` modules that drive docker themselves.
//
// `docker run` and `docker create` pull a missing image implicitly, and that
// implicit pull is single-attempt: a Docker Hub blip or a quota refusal surfaced
// as "failed to start reference container" — a registry fault wearing the
// costume of a broken gate (issue #1562). Going through `pullImageWithRetry`
// gives every such site the same budget for the transport class, the same
// immediate stop on the quota class, and the same words for which one it was.
func TestCiScriptModulesAcquireImagesThroughTheSharedPolicy(t *testing.T) {
	t.Parallel()

	const (
		scriptsDir = "../../.github/scripts"
		// The module that IMPLEMENTS the policy: it is where the raw `docker
		// pull` legitimately lives.
		policyModule = "registry.mjs"
		policyCall   = "pullImageWithRetry"
	)

	// `capture('docker', ['pull'|'run'|'create', …])` — the three subcommands
	// that fetch from a registry (the latter two implicitly, when the image is
	// absent). Both quote styles, because either is valid ESM.
	fetchingDockerCall := regexp.MustCompile(`\(\s*['"]docker['"]\s*,\s*\[\s*['"](?:pull|run|create)['"]`)

	entries, err := os.ReadDir(scriptsDir)
	if err != nil {
		t.Fatalf("read %s: %v", scriptsDir, err)
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".mjs" {
			files = append(files, filepath.Join(scriptsDir, e.Name()))
		}
	}
	libEntries, err := os.ReadDir(filepath.Join(scriptsDir, "lib"))
	if err != nil {
		t.Fatalf("read %s/lib: %v", scriptsDir, err)
	}
	for _, e := range libEntries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".mjs" && e.Name() != policyModule {
			files = append(files, filepath.Join(scriptsDir, "lib", e.Name()))
		}
	}

	sites := 0
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		// Matched against the whole file, not line by line: the argv array of a
		// `docker run` spans several lines in every module that has one, and a
		// per-line scan silently matched none of them.
		text := string(src)
		for _, loc := range fetchingDockerCall.FindAllStringIndex(text, -1) {
			call := strings.TrimSpace(strings.SplitN(text[loc[0]:loc[1]], "\n", 2)[0])
			if isProse(lineContaining(text, loc[0])) {
				continue
			}
			sites++
			if !strings.Contains(text, policyCall) {
				t.Errorf("%s fetches from a registry without the shared policy:\n    %s\n"+
					"Acquire the image with `%s(ref, …)` from ./lib/registry.mjs first, so a transport fault "+
					"retries and a rate-limit refusal fails on the first attempt instead of spending four more "+
					"pulls out of an exhausted quota.", file, call, policyCall)
			}
		}
	}

	if sites == 0 {
		t.Fatalf("no .mjs module under %s runs `docker pull|run|create` — the guard is scanning for a shape "+
			"that no longer exists", scriptsDir)
	}
}

// lineContaining returns the source line that offset falls on.
func lineContaining(src string, offset int) string {
	start := strings.LastIndexByte(src[:offset], '\n') + 1
	end := strings.IndexByte(src[offset:], '\n')
	if end < 0 {
		return src[start:]
	}
	return src[start : offset+end]
}

// TestBuildRetryWrapperRetriesOnlyTransientRegistryFailures drives the module
// against a stub `docker` on PATH. The three halves are equally load-bearing: a
// wrapper that does not retry a reset connection fixes nothing, one that
// retries a genuine build failure is a `continue-on-error` in disguise — it
// would turn a broken build into five broken builds and a slower red — and one
// that retries a rate-limit refusal spends four more pulls out of the exhausted
// quota that is failing this job and every concurrent one.
func TestBuildRetryWrapperRetriesOnlyTransientRegistryFailures(t *testing.T) {
	t.Parallel()

	const (
		// The two forms Docker Hub's refusal arrives in. The first is also a
		// transport signature (`unexpected status from HEAD request`), so it
		// doubles as the precedence case: rate limit wins, no retry.
		hubRateLimit = "#5 ERROR: unexpected status from HEAD request to " +
			"https://registry-1.docker.io/v2/library/golang/manifests/1.26: 429 Too Many Requests"
		hubRateLimitProse = "toomanyrequests: You have reached your unauthenticated pull rate limit."

		resetConnection     = `#4 ERROR: failed to do request: Head "https://registry-1.docker.io/v2/": read: connection reset by peer`
		compileError        = "internal/api/prom/handler.go:12:2: undefined: notAThing"
		compileExitStatus   = 2
		neverSucceeds       = buildRetryAttempts + 1
		succeedsImmediately = 1
		// Late enough that a retrying wrapper WOULD have succeeded: the
		// rate-limit cases fail anyway, which is the behaviour under test.
		wouldSucceedOnRetry = 3
	)

	cases := []struct {
		name string
		// succeedsOn is the 1-based attempt at which the stub starts exiting 0.
		succeedsOn   int
		failureText  string
		failureExit  int
		wantExitCode int
		wantCalls    int
		// wantReport is a phrase the failure report must carry. The wording is
		// part of the fix, not decoration: the report this replaced said "the
		// registry is not answering for this runner", and a whole afternoon of
		// red lanes was diagnosed as a Docker Hub outage on the strength of it.
		wantReport string
	}{
		{
			name:         "a transport fault is ridden out",
			succeedsOn:   wouldSucceedOnRetry,
			failureText:  resetConnection,
			failureExit:  1,
			wantExitCode: 0,
			wantCalls:    wouldSucceedOnRetry,
		},
		{
			name:         "a registry that never answers spends the budget and fails the job",
			succeedsOn:   neverSucceeds,
			failureText:  resetConnection,
			failureExit:  1,
			wantExitCode: 1,
			wantCalls:    buildRetryAttempts,
			wantReport:   "transport fault",
		},
		{
			name:         "a Docker Hub 429 fails on the first attempt, even though a retry would have worked",
			succeedsOn:   wouldSucceedOnRetry,
			failureText:  hubRateLimit,
			failureExit:  1,
			wantExitCode: 1,
			wantCalls:    rateLimitAttempts,
			wantReport:   "QUOTA, not an outage",
		},
		{
			name:         "the prose form of the same refusal is not retried either",
			succeedsOn:   wouldSucceedOnRetry,
			failureText:  hubRateLimitProse,
			failureExit:  1,
			wantExitCode: 1,
			wantCalls:    rateLimitAttempts,
			wantReport:   "retrying cannot clear it",
		},
		{
			name:         "a genuine build failure is not retried and keeps its exit status",
			succeedsOn:   neverSucceeds,
			failureText:  compileError,
			failureExit:  compileExitStatus,
			wantExitCode: compileExitStatus,
			wantCalls:    1,
		},
		{
			name:         "a build that works runs exactly once",
			succeedsOn:   succeedsImmediately,
			failureText:  hubRateLimit,
			failureExit:  1,
			wantExitCode: 0,
			wantCalls:    1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			callLog := filepath.Join(dir, "calls")
			writeStubDockerBuild(t, dir, callLog, tc.succeedsOn, tc.failureText, tc.failureExit)

			cmd := exec.Command("node", buildRetryWrapperPath,
				"docker", "build", "-f", "Dockerfile.local", "-t", "cerberus:e2e", ".")
			cmd.Env = append(
				os.Environ(),
				"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				// Zero backoff: what is under test is the retry decision, not
				// the wall clock it burns.
				"IMAGE_BUILD_RETRY_BACKOFF_SECONDS=0",
			)
			out, err := cmd.CombinedOutput()

			exitCode := 0
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("run %s: %v (node and bash must be on PATH)", buildRetryWrapper, err)
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
			calls := len(strings.Fields(string(logged)))
			if calls != tc.wantCalls {
				t.Errorf("the build ran %d time(s), want %d\noutput:\n%s", calls, tc.wantCalls, out)
			}
			if tc.wantReport != "" && !strings.Contains(string(out), tc.wantReport) {
				t.Errorf("the failure report never says %q, so it does not name why the job failed\noutput:\n%s",
					tc.wantReport, out)
			}
		})
	}
}

// writeStubDockerBuild drops a `docker` onto dir that records every BUILD call
// and fails with failureText/failureExit until the succeedsOn-th one. Any other
// subcommand is refused without being recorded.
func writeStubDockerBuild(t *testing.T, dir, callLog string, succeedsOn int, failureText string, failureExit int) {
	t.Helper()

	script := strings.Join([]string{
		"#!/bin/sh",
		// The stub stands in for the BUILD, so only a build counts as a call.
		// The wrapper also probes the mirror for its base-image refs
		// (`docker buildx imagetools inspect`), and a probe that failed the
		// count would make the retry assertions below read one attempt short.
		// Refusing it is the honest answer for a runner holding no mirror
		// credentials, and it exercises the wrapper's fall-back-to-upstream
		// path; the mirror-hit path is pinned by the sibling test.
		`[ "$1" = build ] || exit 1`,
		"echo call >> " + shellQuote(callLog),
		"attempts=$(wc -l < " + shellQuote(callLog) + " | tr -d ' ')",
		"[ \"$attempts\" -ge " + strconv.Itoa(succeedsOn) + " ] && exit 0",
		"echo " + shellQuote(failureText) + " >&2",
		"exit " + strconv.Itoa(failureExit),
		"",
	}, "\n")

	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub docker: %v", err)
	}
}

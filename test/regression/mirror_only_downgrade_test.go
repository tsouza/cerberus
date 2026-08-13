package regression

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The GHCR mirror's whole claim is that cerberus's CI does not depend on Docker
// Hub answering. Until issue #1933 that claim was never measured, and it was
// false: `compatibility/promql-surface` went red on run 31148765992 without
// issuing a single query, because the Docker Hub login exhausted its retries on
// `context deadline exceeded` and every login is a hard gate. Each image the job
// wanted was sitting in the mirror, which nothing had contacted.
//
// Two mechanisms fix that and both are asserted here, on the real scripts:
//
//   - `registry-login.mjs` turns a spent TRANSPORT-fault budget on Docker Hub
//     into MIRROR-ONLY mode for the job, exported through $GITHUB_ENV.
//   - `pullImageWithRetry` (lib/registry.mjs) in that mode serves from the
//     mirror or fails LOUDLY naming the image. It never reaches for Docker Hub
//     anonymously, which is the silent degradation the login gate exists to
//     prevent and the reason `continue-on-error: true` on the login step would
//     have been a regression rather than a fix.
//
// The tests drive the two halves in sequence through a stub `docker` on PATH,
// passing the flag between them exactly as the runner does: the login step's
// $GITHUB_ENV file is parsed and fed to the pull step's environment. That is
// what makes the variable name load-bearing here rather than restated — a
// rename on one side and not the other fails this test.

const (
	registryLoginModule = "../../.github/scripts/registry-login.mjs"
	pullImagesModule    = "../../.github/scripts/pull-images.mjs"

	// The output the daemon produces when it cannot reach Docker Hub at all —
	// the verbatim line from run 31148765992. It is on `lib/registry.mjs`'s
	// transient list and off its rate-limit list, so it is the input that
	// produces the `transient` verdict the downgrade keys on. Using the real
	// text rather than a synthetic one keeps this test coupled to the
	// classifier's actual behaviour.
	handshakeTimeout = `Error response from daemon: Get "https://registry-1.docker.io/v2/": context deadline exceeded`

	// A Docker Hub ref (no registry host) that the mirror inventory does not
	// hold, so `mirroredRef` returns null for it and mirror-only mode has
	// nothing to serve. Deliberately not a real image: the point is the
	// decision, and a name nobody can mirror cannot drift into the inventory.
	unmirroredRef = "cerberus-test/not-in-the-mirror:1"

	// A ref on a registry Docker Hub does not meter, which the inventory also
	// does not hold — and deliberately never will, because mirroring a registry
	// that is not rate-limited would add a hop and a staleness window for no
	// gain (see lib/mirror.mjs). `mirroredRef` returns null for it exactly as it
	// does for `unmirroredRef`, which is why keying mirror-only mode off that
	// null is wrong: the two refs need OPPOSITE answers.
	//
	// The real thing, not a placeholder: `E2E_EXTERNAL_IMAGES` in the Justfile
	// names it, so the k3d lanes acquire it on every run.
	unmeteredRef = "ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v0.116.0"
)

// dockerCall is one line of the stub's log: the verb and what it was aimed at.
type dockerCall struct{ verb, target string }

// writeStubDockerHubOutage drops a `docker` onto dir that behaves exactly as a
// runner would during a Docker Hub outage with a healthy GHCR:
//
//	login <anything>     fails with the /v2/ handshake timeout, always.
//	pull <mirror ns>/... succeeds — the mirror is up, which is the whole premise.
//	pull <other ghcr>    succeeds, recorded as `direct`. A ref on a registry
//	                     Docker Hub does not meter is reached over a path this
//	                     outage cannot touch, so it must still be acquired.
//	pull <docker hub>    fails the same way, and is recorded as `hub`. Recording
//	                     the attempt is the point: the property under test is
//	                     that the attempt is never made, and an assertion about
//	                     something not happening is worthless unless the harness
//	                     would have seen it happen.
//	tag / image          succeed / report nothing local.
func writeStubDockerHubOutage(t *testing.T, dir, callLog string) {
	t.Helper()

	script := strings.Join([]string{
		"#!/bin/sh",
		"log=" + shellQuote(callLog),
		"case \"$1\" in",
		"  login)",
		// The password arrives on stdin (`--password-stdin`), and a real
		// `docker login` reads it before answering. Draining it is not cosmetic:
		// a stub that exits first makes the parent's write race the child's
		// death, and the EPIPE that sometimes results is classified `fatal`
		// rather than `transient` — so the test intermittently measured a
		// completely different code path from the one it names.
		"    cat > /dev/null",
		"    echo " + shellQuote("login") + " >> \"$log\"",
		"    echo " + shellQuote(handshakeTimeout) + " >&2",
		"    exit 1",
		"    ;;",
		"  pull)",
		"    case \"$2\" in",
		"      " + mirrorNamespace(t) + "/*)",
		"        echo \"mirror $2\" >> \"$log\"",
		"        exit 0",
		"        ;;",
		"      " + mirrorRegistryPrefix + "*)",
		"        echo \"direct $2\" >> \"$log\"",
		"        exit 0",
		"        ;;",
		"    esac",
		"    echo \"hub $2\" >> \"$log\"",
		"    echo " + shellQuote(handshakeTimeout) + " >&2",
		"    exit 1",
		"    ;;",
		"  tag)",
		"    echo \"tag $3\" >> \"$log\"",
		"    exit 0",
		"    ;;",
		"  image)",
		// Nothing is cached locally, so an acquisition can only come from a
		// registry — otherwise `acceptLocalCopy` callers would pass for the
		// wrong reason.
		"    exit 1",
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

// readStubCalls parses the stub's log. A missing file means no call was made,
// which is a legitimate answer rather than an error.
func readStubCalls(t *testing.T, callLog string) []dockerCall {
	t.Helper()

	buf, err := os.ReadFile(callLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read stub call log: %v", err)
	}
	var calls []dockerCall
	for _, line := range strings.Split(strings.TrimSpace(string(buf)), "\n") {
		if line == "" {
			continue
		}
		verb, target, _ := strings.Cut(line, " ")
		calls = append(calls, dockerCall{verb: verb, target: target})
	}
	return calls
}

func callsWithVerb(calls []dockerCall, verb string) []string {
	var out []string
	for _, c := range calls {
		if c.verb == verb {
			out = append(out, c.target)
		}
	}
	sort.Strings(out)
	return out
}

// runNode runs one of the repository's scripts against the stub PATH and returns
// its exit status and combined output.
func runNode(t *testing.T, module, stubDir string, env []string, args ...string) (int, string) {
	t.Helper()

	cmd := exec.Command("node", append([]string{module}, args...)...)
	cmd.Env = append(
		os.Environ(),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		// Zero backoff: the retry budget is what is under test, not the wait.
		"REGISTRY_LOGIN_BACKOFF_STEP_SECONDS=0",
		"IMAGE_PULL_BACKOFF_SECONDS=0",
	)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	status := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v\noutput:\n%s", module, err, out)
		}
		status = exit.ExitCode()
	}
	return status, string(out)
}

// loginUnderOutage runs the Docker Hub login against the stub and returns its
// status, its output, and whatever it exported through $GITHUB_ENV — the
// runner's own channel, so the exported flag reaches the next step exactly as it
// would in a job.
func loginUnderOutage(t *testing.T, stubDir string, env ...string) (int, string, []string) {
	t.Helper()

	githubEnv := filepath.Join(t.TempDir(), "github_env")
	if err := os.WriteFile(githubEnv, nil, 0o600); err != nil {
		t.Fatalf("create GITHUB_ENV file: %v", err)
	}
	env = append(env, "GITHUB_ENV="+githubEnv, "USERNAME=cerberus-test", "PASSWORD=not-a-real-secret")
	status, out := runNode(t, registryLoginModule, stubDir, env)

	buf, err := os.ReadFile(githubEnv)
	if err != nil {
		t.Fatalf("read GITHUB_ENV file: %v", err)
	}
	var exported []string
	for _, line := range strings.Split(strings.TrimSpace(string(buf)), "\n") {
		if line != "" {
			exported = append(exported, line)
		}
	}
	return status, out, exported
}

// mirrorNamespaceLiteral isolates `mirrorRegistry`'s default in lib/mirror.mjs.
var mirrorNamespaceLiteral = regexp.MustCompile(`\|\|\s*'(ghcr\.io/[^']+)'`)

// mirrorNamespace is the GHCR namespace mirrored copies live under, read from
// the module rather than restated here. The stub has to tell a pull of a
// MIRRORED copy from a pull of some other ghcr.io image, and a hardcoded
// namespace would silently stop distinguishing them if the mirror ever moved —
// leaving the test green while measuring nothing.
func mirrorNamespace(t *testing.T) string {
	t.Helper()

	src, err := os.ReadFile(mirrorModule)
	if err != nil {
		t.Fatalf("read %s: %v", mirrorModule, err)
	}
	m := mirrorNamespaceLiteral.FindStringSubmatch(string(src))
	if m == nil {
		t.Fatalf("%s no longer states a default `mirrorRegistry` — the stub cannot tell a mirrored pull "+
			"from any other ghcr.io pull", mirrorModule)
	}
	return m[1]
}

// anyMirroredImage is one ref the mirror actually holds, read from the inventory
// rather than named here so a bump cannot leave this test pinned to a ref the
// mirror no longer carries.
func anyMirroredImage(t *testing.T) string {
	t.Helper()

	var refs []string
	for ref := range mirrorInventory(t) {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs[0]
}

// TestMirrorOnlyJobCompletesWithDockerHubUnreachable is the property the mirror
// was built for and nothing measured until now: with Docker Hub unreachable, a
// job whose images are all mirrored runs to completion.
//
// Both steps are the real scripts and the flag travels between them through
// $GITHUB_ENV, so this fails if either half stops honouring the mode OR if the
// two halves stop agreeing on how it is spelled.
//
// What DISCRIMINATES here is the login half: without the downgrade it exits 1
// and the job is over, which is issue #1933 exactly. The pull half is the
// unchanged-behaviour leg on purpose — an inventoried image is served by
// `acquireFromMirror` before the mode is ever consulted, so it must look
// identical in both modes, and the assertions below say so rather than claiming
// to exercise the mode. The pull-side behaviour that only mirror-only mode
// produces is pinned by the two tests that follow.
func TestMirrorOnlyJobCompletesWithDockerHubUnreachable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls")
	writeStubDockerHubOutage(t, dir, callLog)

	status, out, exported := loginUnderOutage(t, dir)
	if status != 0 {
		t.Fatalf("the Docker Hub login failed the job (exit %d) instead of downgrading it to mirror-only.\n"+
			"An unreachable Docker Hub is exactly what the GHCR mirror exists to survive.\noutput:\n%s", status, out)
	}
	if len(exported) != 1 || !strings.HasPrefix(exported[0], "CERBERUS_REGISTRY_MIRROR_ONLY=") {
		t.Fatalf("the login exported %v through $GITHUB_ENV, want the mirror-only flag. Without it every "+
			"later step in the job carries on as if Docker Hub were reachable.\noutput:\n%s", exported, out)
	}

	image := anyMirroredImage(t)
	status, out = runNode(t, pullImagesModule, dir, exported, image)
	if status != 0 {
		t.Fatalf("acquiring the mirrored image %q failed (exit %d) although the mirror served it.\noutput:\n%s",
			image, status, out)
	}

	calls := readStubCalls(t, callLog)
	if got := callsWithVerb(calls, "tag"); len(got) != 1 || got[0] != image {
		t.Errorf("the mirrored copy was tagged as %v, want exactly [%s] — the caller's postcondition is the "+
			"image present under its UPSTREAM name", got, image)
	}
	if got := callsWithVerb(calls, "hub"); len(got) != 0 {
		t.Errorf("the job reached for Docker Hub (%v) while in mirror-only mode. That pull is anonymous — it "+
			"is the silent degradation the mirror and the login gate both exist to remove.", got)
	}
}

// TestMirrorOnlyFailsLoudlyRatherThanPullingAnonymously is the other half of the
// same property, and the one that keeps it from being a tolerance: an image the
// mirror cannot serve fails the job NAMING ITSELF, rather than quietly falling
// through to the shared anonymous Docker Hub quota.
func TestMirrorOnlyFailsLoudlyRatherThanPullingAnonymously(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls")
	writeStubDockerHubOutage(t, dir, callLog)

	// The downgrade is this test's PREMISE, so it is asserted rather than
	// assumed: a login that failed instead would leave `exported` empty, the
	// pull would run in ordinary mode, and the test would go red for a reason
	// that has nothing to do with what it is named after.
	loginStatus, loginOut, exported := loginUnderOutage(t, dir)
	if loginStatus != 0 || len(exported) != 1 {
		t.Fatalf("the login exited %d exporting %v, so this test never reached mirror-only mode.\noutput:\n%s",
			loginStatus, exported, loginOut)
	}

	status, out := runNode(t, pullImagesModule, dir, exported, unmirroredRef)
	if status == 0 {
		t.Fatalf("acquiring %q succeeded although the mirror does not hold it and Docker Hub is unreachable.\n"+
			"output:\n%s", unmirroredRef, out)
	}
	if !strings.Contains(out, unmirroredRef) {
		t.Errorf("the failure never named %q, so a reader cannot tell which image to mirror.\noutput:\n%s",
			unmirroredRef, out)
	}
	if got := callsWithVerb(readStubCalls(t, callLog), "hub"); len(got) != 0 {
		t.Errorf("mirror-only mode reached for Docker Hub anyway (%v) — the fallback must be REMOVED in this "+
			"mode, not merely deprioritised.", got)
	}
}

// TestMirrorOnlyStillAcquiresImagesDockerHubNeverServed is the boundary that
// keeps the mode proportionate to the outage that caused it.
//
// Mirror-only mode exists because DOCKER HUB is unreachable. A ref naming any
// other registry is fetched over a path the outage does not touch, on
// credentials the job already holds, so it must be acquired exactly as always.
// Refusing it would take down the lanes this mode is meant to save: the k3d e2e
// jobs acquire `telemetrygen` from ghcr.io on every run, and the migration tiers
// extract their binary from a `ghcr.io/tsouza/cerberus` release image.
//
// The bug this pins is a one-token one. `mirroredRef` returns null both for "the
// mirror has no copy of this" and for "this never needed one", so keying the
// refusal off that null reads the second as the first and fails an acquisition
// that was never in danger — with a message prescribing a fix (add it to
// `mirroredImages`) that cannot work for such a ref.
func TestMirrorOnlyStillAcquiresImagesDockerHubNeverServed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls")
	writeStubDockerHubOutage(t, dir, callLog)

	loginStatus, loginOut, exported := loginUnderOutage(t, dir)
	if loginStatus != 0 || len(exported) != 1 {
		t.Fatalf("the login exited %d exporting %v, so this test never reached mirror-only mode.\noutput:\n%s",
			loginStatus, exported, loginOut)
	}

	status, out := runNode(t, pullImagesModule, dir, exported, unmeteredRef)
	if status != 0 {
		t.Fatalf("mirror-only mode refused %q (exit %d), a ref Docker Hub does not serve and its outage cannot "+
			"reach. The mode must constrain Docker Hub acquisitions, not every acquisition.\noutput:\n%s",
			unmeteredRef, status, out)
	}
	if got := callsWithVerb(readStubCalls(t, callLog), "direct"); len(got) != 1 || got[0] != unmeteredRef {
		t.Errorf("the unmetered ref was acquired as %v, want exactly [%s] — it must be pulled from its own "+
			"registry rather than looked for in the mirror", got, unmeteredRef)
	}
}

// TestWithoutTheDowngradeTheSameImageIsPulledFromDockerHub is the non-vacuity
// control for both tests above. Their central assertion is that something did
// NOT happen, which passes just as well against a stub nothing ever calls or a
// module that crashed on startup. Here the ONLY difference is the absence of the
// mirror-only flag, and the upstream pull the other tests forbid duly appears.
func TestWithoutTheDowngradeTheSameImageIsPulledFromDockerHub(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls")
	writeStubDockerHubOutage(t, dir, callLog)

	status, out := runNode(t, pullImagesModule, dir, nil, unmirroredRef)
	if status == 0 {
		t.Fatalf("the unmirrored image was acquired although Docker Hub is unreachable.\noutput:\n%s", out)
	}
	if got := callsWithVerb(readStubCalls(t, callLog), "hub"); len(got) == 0 {
		t.Fatal("no Docker Hub pull was attempted even with mirror-only mode OFF — the stub is not observing " +
			"the fallback at all, so the assertions that it does NOT happen prove nothing")
	}
}

// TestOnlyDockerHubDowngrades pins the two boundaries that keep the downgrade
// from becoming a general "carry on regardless".
//
// A ghcr.io login that cannot reach ghcr.io has nothing to fall back to —
// mirror-only mode there would mean acquiring everything from the registry that
// just failed. And a step that publishes to Docker Hub (`release.yml`) or reads
// its inventory from it (`mirror-images.yml`) says `ON_TRANSPORT_FAULT: fail`,
// because for those two Docker Hub is the work rather than a source a mirror can
// stand in for.
func TestOnlyDockerHubDowngrades(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		env  []string
	}{
		{name: "a login to the mirror itself", env: []string{"REGISTRY=ghcr.io"}},
		{name: "a step that opted out", env: []string{"ON_TRANSPORT_FAULT=fail"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			writeStubDockerHubOutage(t, dir, filepath.Join(dir, "calls"))

			status, out, exported := loginUnderOutage(t, dir, tc.env...)
			if status == 0 {
				t.Errorf("the login exited 0 for %s, so the job carries on without a registry no mirror can "+
					"substitute for.\noutput:\n%s", tc.name, out)
			}
			if len(exported) != 0 {
				t.Errorf("the login exported %v for %s; nothing may be exported when there is no downgrade",
					exported, tc.name)
			}
		})
	}
}

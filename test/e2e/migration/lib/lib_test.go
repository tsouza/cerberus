package lib

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoRootFindsTheCerberusModule asserts the walk stops at the directory
// that actually holds the cerberus go.mod — the root every fixture path is
// resolved against — rather than at the first go.mod it meets.
func TestRepoRootFindsTheCerberusModule(t *testing.T) {
	t.Parallel()

	root, err := RepoRoot()
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod at discovered root %s: %v", root, err)
	}
	if !strings.Contains(string(mod), modulePath) {
		t.Fatalf("go.mod at %s does not declare %q", root, modulePath)
	}
	// This package lives inside the harness tree, so the discovered root must
	// be the harness tree's parent chain — a root that merely exists is not
	// enough.
	if _, err := os.Stat(HarnessPath(root, "lib", "repo.go")); err != nil {
		t.Fatalf("discovered root %s does not contain the harness lib package: %v", root, err)
	}
}

// TestRepoRootSkipsANestedModule asserts the walk does not stop at a nested
// module's go.mod. The repository carries one (the AGPL-quarantined oracle
// tree), so a presence-only check would return the wrong root.
func TestRepoRootSkipsANestedModule(t *testing.T) {
	t.Parallel()

	outer := t.TempDir()
	if err := os.WriteFile(filepath.Join(outer, "go.mod"), []byte(modulePath+"\n"), goldenFileMode); err != nil {
		t.Fatalf("write outer go.mod: %v", err)
	}
	nested := filepath.Join(outer, "nested")
	if err := os.MkdirAll(nested, goldenDirMode); err != nil {
		t.Fatalf("create nested dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "go.mod"), []byte("module example.com/nested\n"), goldenFileMode); err != nil {
		t.Fatalf("write nested go.mod: %v", err)
	}

	got, err := repoRootFrom(nested)
	if err != nil {
		t.Fatalf("repoRootFrom(%s): %v", nested, err)
	}
	if got != outer {
		t.Fatalf("repoRootFrom(%s) = %s, want the outer cerberus module root %s", nested, got, outer)
	}
}

// TestRepoRootReportsAMissingModule asserts the walk fails loudly at the
// filesystem root instead of returning an empty path a caller would join onto.
func TestRepoRootReportsAMissingModule(t *testing.T) {
	t.Parallel()

	got, err := repoRootFrom(t.TempDir())
	if err == nil {
		t.Fatalf("repoRootFrom on a tree with no cerberus go.mod returned %q, want an error", got)
	}
	if got != "" {
		t.Fatalf("repoRootFrom returned root %q alongside an error, want the empty string", got)
	}
}

// TestRunCapturesBothStreamsAndTheExitCode asserts a non-zero exit is reported
// as data with both streams intact. Scenarios assert specific exit codes, so a
// non-zero exit must not surface as a Go error.
func TestRunCapturesBothStreamsAndTheExitCode(t *testing.T) {
	t.Parallel()

	const wantExit = 7
	res, err := Run(RunSpec{
		Bin:  "/bin/sh",
		Args: []string{"-c", "printf on-stdout; printf on-stderr 1>&2; exit 7"},
		Env:  OfflineEnv(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != wantExit {
		t.Fatalf("ExitCode = %d, want %d", res.ExitCode, wantExit)
	}
	if string(res.Stdout) != "on-stdout" {
		t.Fatalf("Stdout = %q, want %q", res.Stdout, "on-stdout")
	}
	if string(res.Stderr) != "on-stderr" {
		t.Fatalf("Stderr = %q, want %q", res.Stderr, "on-stderr")
	}
}

// TestRunReportsAMissingBinary asserts a failure to start is an error rather
// than a zero-valued Result a caller could mistake for a clean run.
func TestRunReportsAMissingBinary(t *testing.T) {
	t.Parallel()

	res, err := Run(RunSpec{Bin: filepath.Join(t.TempDir(), "absent"), Env: OfflineEnv()})
	if err == nil {
		t.Fatalf("Run on a missing binary returned exit %d and no error", res.ExitCode)
	}
}

// TestRunRefusesDocker asserts the seam that REPLACES the environment refuses
// the one binary whose correctness depends on inheriting it: docker resolves
// which compose project — and so which checkout's containers — an invocation
// addresses from COMPOSE_PROJECT_SUFFIX, so a docker child started through Run
// would pause or tear down whichever checkout owns the unsuffixed project.
func TestRunRefusesDocker(t *testing.T) {
	t.Parallel()

	for _, bin := range []string{DockerBin, filepath.Join("/usr/bin", DockerBin)} {
		res, err := Run(RunSpec{Bin: bin, Args: []string{"compose", "version"}, Env: OfflineEnv()})
		if err == nil {
			t.Fatalf("Run(%q) returned exit %d and no error; it must refuse docker", bin, res.ExitCode)
		}
		if !strings.Contains(err.Error(), "Compose") {
			t.Fatalf("Run(%q) refusal %q does not name Compose as the seam to use instead", bin, err)
		}
	}
}

// TestOfflineEnvBlackholesTheNetworkAndLetsACaseWin asserts the offline
// environment closes every proxy bypass and that a case's own CERBERUS_*
// setting overrides the harness default.
func TestOfflineEnvBlackholesTheNetworkAndLetsACaseWin(t *testing.T) {
	t.Parallel()

	env := OfflineEnv("CERBERUS_CH_ADDR=ch.example:9000")
	seen := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("environment entry %q is not key=value", kv)
		}
		seen[k] = v // a later entry wins, exactly as exec applies the slice
	}

	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if seen[k] != blackholeProxy {
			t.Errorf("%s = %q, want the blackholed proxy %q", k, seen[k], blackholeProxy)
		}
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		if seen[k] != "" {
			t.Errorf("%s = %q, want empty so loopback cannot bypass the blackhole", k, seen[k])
		}
	}
	if got := seen["CERBERUS_CH_ADDR"]; got != "ch.example:9000" {
		t.Errorf("CERBERUS_CH_ADDR = %q, want the caller's override to win", got)
	}
}

// TestAssertGoldenMatchesAndReportsTheDifferingLine asserts a byte-identical
// artifact passes and a one-line divergence names that line.
func TestAssertGoldenMatchesAndReportsTheDifferingLine(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "golden.sql")
	golden := "CREATE TABLE a;\nCREATE TABLE b;\n"
	if err := os.WriteFile(path, []byte(golden), goldenFileMode); err != nil {
		t.Fatalf("write golden: %v", err)
	}

	if err := AssertGolden(path, []byte(golden)); err != nil {
		t.Fatalf("AssertGolden on an identical artifact: %v", err)
	}

	err := AssertGolden(path, []byte("CREATE TABLE a;\nCREATE TABLE c;\n"))
	if err == nil {
		t.Fatal("AssertGolden on a divergent artifact returned nil")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error does not name the differing line: %v", err)
	}
	if !strings.Contains(err.Error(), "CREATE TABLE c;") {
		t.Errorf("error does not quote the observed line: %v", err)
	}
}

// TestAssertGoldenReportsAMissingGolden asserts an absent golden fails instead
// of silently passing — the failure mode that would let a scenario assert
// nothing at all.
func TestAssertGoldenReportsAMissingGolden(t *testing.T) {
	t.Parallel()

	err := AssertGolden(filepath.Join(t.TempDir(), "absent.sql"), []byte("anything"))
	if err == nil {
		t.Fatal("AssertGolden against a missing golden returned nil")
	}
}

// TestAssertGoldenRegeneratesLocallyOnly asserts the update mode writes the
// observed artifact when run by a developer, and refuses under CI.
func TestAssertGoldenRegeneratesLocallyOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "golden.sql")
	t.Setenv(updateGoldensEnv, "1")
	t.Setenv(ciEnv, "")

	if err := AssertGolden(path, []byte("CREATE TABLE a;\n")); err != nil {
		t.Fatalf("AssertGolden in update mode: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read regenerated golden: %v", err)
	}
	if string(got) != "CREATE TABLE a;\n" {
		t.Fatalf("regenerated golden = %q, want the observed artifact", got)
	}

	t.Setenv(ciEnv, "true")
	err = AssertGolden(path, []byte("CREATE TABLE b;\n"))
	if err == nil {
		t.Fatal("AssertGolden regenerated a golden under CI")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden after the refused regeneration: %v", err)
	}
	if string(after) != "CREATE TABLE a;\n" {
		t.Fatalf("golden was rewritten under CI: %q", after)
	}
}

// writeVersionStub writes an executable that answers `--version` with the given
// stamp — the smallest stand-in for a cerberus build whose provenance the
// harness has to decide about.
func writeVersionStub(t *testing.T, dir, version string) string {
	t.Helper()
	path := filepath.Join(dir, "cerberus")
	script := "#!/bin/sh\necho " + version + "\n"
	if err := os.WriteFile(path, []byte(script), goldenDirMode); err != nil {
		t.Fatalf("write version stub: %v", err)
	}
	return path
}

// TestBuildCerberusHonoursAPrebuiltBinary asserts CERBERUS_BIN short-circuits
// the compile, and that a path naming a directory is rejected rather than
// handed on as an unexecutable binary.
func TestBuildCerberusHonoursAPrebuiltBinary(t *testing.T) {
	dir := t.TempDir()
	prebuilt := writeVersionStub(t, dir, SourceBuildVersion)

	t.Setenv(cerberusBinEnv, prebuilt)
	got, err := BuildCerberus(t.TempDir(), dir)
	if err != nil {
		t.Fatalf("BuildCerberus with %s set: %v", cerberusBinEnv, err)
	}
	if got != prebuilt {
		t.Fatalf("BuildCerberus = %s, want the prebuilt binary %s", got, prebuilt)
	}

	t.Setenv(cerberusBinEnv, dir)
	if _, err := BuildCerberus(t.TempDir(), dir); err == nil {
		t.Fatalf("BuildCerberus accepted a directory as %s", cerberusBinEnv)
	}
}

// TestBuildCerberusProvesWhichBinaryItGot is the provenance half. A stat that
// only proves "a file exists here" is satisfied by a stale artifact, a
// wrong-GOARCH build, or a release lane that silently fell back to compiling
// from source — every one of which would run the whole scenario set against a
// cerberus the run is not supposed to be testing, and report green.
func TestBuildCerberusProvesWhichBinaryItGot(t *testing.T) {
	const releaseVersion = "1.11.2"

	t.Run("source_run_expects_the_source_stamp", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(ExpectVersionEnv, "")
		t.Setenv(cerberusBinEnv, writeVersionStub(t, dir, SourceBuildVersion))
		if _, err := BuildCerberus(t.TempDir(), dir); err != nil {
			t.Fatalf("a from-source run rejected the %q stamp it is supposed to expect: %v",
				SourceBuildVersion, err)
		}
	})

	t.Run("release_run_rejects_a_source_build", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(ExpectVersionEnv, releaseVersion)
		t.Setenv(cerberusBinEnv, writeVersionStub(t, dir, SourceBuildVersion))
		_, err := BuildCerberus(t.TempDir(), dir)
		if err == nil {
			t.Fatalf("a run pinned to %s accepted a binary reporting %q; a release lane that fell "+
				"back to a source build would report green", releaseVersion, SourceBuildVersion)
		}
		for _, want := range []string{SourceBuildVersion, releaseVersion} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the mismatch error does not quote %q, so the log would not name what was "+
					"actually run: %v", want, err)
			}
		}
	})

	t.Run("release_run_accepts_the_pinned_stamp", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(ExpectVersionEnv, releaseVersion)
		t.Setenv(cerberusBinEnv, writeVersionStub(t, dir, releaseVersion))
		if _, err := BuildCerberus(t.TempDir(), dir); err != nil {
			t.Fatalf("a run pinned to %s rejected a binary reporting exactly that: %v", releaseVersion, err)
		}
	})

	t.Run("a_silent_binary_is_a_failure_not_a_pass", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "cerberus")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), goldenDirMode); err != nil {
			t.Fatalf("write silent stub: %v", err)
		}
		t.Setenv(ExpectVersionEnv, "")
		t.Setenv(cerberusBinEnv, path)
		if _, err := BuildCerberus(t.TempDir(), dir); err == nil {
			t.Fatalf("BuildCerberus accepted a binary that printed no version at all")
		}
	})
}

// TestExpectedVersionsSplitCLIFromServer pins the one asymmetry the provenance
// helpers encode: on a from-source run the CLI (`go build`, no ldflags) and the
// compose server (Dockerfile.local, `-X main.Version=e2e`) are DIFFERENT builds,
// so each is held to its own stamp; on a released-artifact run they are the same
// bytes and both are held to the run-wide one. Collapsing the two would make one
// of the two probes assert against a value that can never hold.
func TestExpectedVersionsSplitCLIFromServer(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		t.Setenv(ExpectVersionEnv, "")
		if got := ExpectedCLIVersion(); got != SourceBuildVersion {
			t.Fatalf("ExpectedCLIVersion() = %q, want %q", got, SourceBuildVersion)
		}
		if got := ExpectedServerVersion(); got != LocalImageVersion {
			t.Fatalf("ExpectedServerVersion() = %q, want %q", got, LocalImageVersion)
		}
		if SourceBuildVersion == LocalImageVersion {
			t.Fatalf("the CLI and the server stamps are both %q, so neither probe can tell a "+
				"from-source run from an image run", SourceBuildVersion)
		}
	})

	t.Run("pinned", func(t *testing.T) {
		const releaseVersion = "1.11.2"
		t.Setenv(ExpectVersionEnv, releaseVersion)
		if got := ExpectedCLIVersion(); got != releaseVersion {
			t.Fatalf("ExpectedCLIVersion() = %q, want %q", got, releaseVersion)
		}
		if got := ExpectedServerVersion(); got != releaseVersion {
			t.Fatalf("ExpectedServerVersion() = %q, want %q", got, releaseVersion)
		}
	})
}

// TestRequireOfflineAcceptsOnlyAClosedEnvironment proves the check a scenario's
// "contacts no network endpoint" assertion rests on actually bites. Every way an
// environment could leave a route open — a proxy variable unset, one pointed
// somewhere reachable, a bypass list exempting hosts — must fail it; only the
// environment OfflineEnv builds may pass.
func TestRequireOfflineAcceptsOnlyAClosedEnvironment(t *testing.T) {
	if err := RequireOffline(OfflineEnv()); err != nil {
		t.Fatalf("the harness's own offline environment was rejected: %v", err)
	}

	drop := func(name string) []string {
		var out []string
		for _, kv := range OfflineEnv() {
			if k, _, _ := strings.Cut(kv, "="); k != name {
				out = append(out, kv)
			}
		}
		return out
	}
	for _, name := range blackholedProxyVars {
		t.Run("unset "+name, func(t *testing.T) {
			if err := RequireOffline(drop(name)); err == nil {
				t.Fatalf("an environment with %s unset was accepted as offline", name)
			}
		})
	}
	// An ABSENT bypass list exempts nothing, so it is offline; only a populated
	// one reopens a route.
	for _, name := range bypassVars {
		t.Run("absent "+name, func(t *testing.T) {
			if err := RequireOffline(drop(name)); err != nil {
				t.Fatalf("an environment with no %s bypass list was rejected: %v", name, err)
			}
		})
	}
	for _, leak := range []string{
		"HTTP_PROXY=http://proxy.internal:3128",
		"https_proxy=http://proxy.internal:3128",
		"NO_PROXY=localhost,127.0.0.1",
		"no_proxy=*",
	} {
		t.Run(leak, func(t *testing.T) {
			if err := RequireOffline(OfflineEnv(leak)); err == nil {
				t.Fatalf("an environment carrying %q was accepted as offline", leak)
			}
		})
	}
}

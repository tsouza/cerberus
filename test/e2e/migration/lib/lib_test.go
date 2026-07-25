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

// TestBuildCerberusHonoursAPrebuiltBinary asserts CERBERUS_BIN short-circuits
// the compile, and that a path naming a directory is rejected rather than
// handed on as an unexecutable binary.
func TestBuildCerberusHonoursAPrebuiltBinary(t *testing.T) {
	dir := t.TempDir()
	prebuilt := filepath.Join(dir, "cerberus")
	if err := os.WriteFile(prebuilt, []byte("#!/bin/sh\n"), goldenDirMode); err != nil {
		t.Fatalf("write prebuilt binary: %v", err)
	}

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

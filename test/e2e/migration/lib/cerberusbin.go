package lib

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cerberusBinEnv names a prebuilt cerberus binary the scenarios should drive.
// The migration workflow resolves the binary once in its own step — from this
// tree on a PR/dispatch run, out of the released image on a release run (see
// .github/scripts/migration-artifact.mjs) — and hands the path in, so a tier
// job does not recompile it per scenario run.
const cerberusBinEnv = "CERBERUS_BIN"

// cerberusPkg is the main package the harness compiles when no prebuilt binary
// is supplied.
const cerberusPkg = "./cmd/cerberus"

// binName is the filename the freshly-built binary gets inside dir.
const binName = "cerberus"

// versionFlag is the argument that makes cerberus print its bare build version
// and nothing else (cmd/cerberus/cli.go's printVersion).
const versionFlag = "--version"

// BuildCerberus returns the path of the cerberus binary the scenarios exec.
// CERBERUS_BIN short-circuits the build when a caller already produced one;
// otherwise the binary is compiled from source into dir, which the caller owns
// (a per-run temporary directory).
//
// Either way the resolved binary is then PROVED to be the one this run is
// supposed to be testing: it must answer `--version` with exactly
// ExpectedCLIVersion(). A stale artifact, a wrong-GOARCH build or an unrelated
// executable all satisfy a stat-and-not-a-directory check, and a release run
// that quietly fell back to a source build would report `dev` where the release
// version was expected — so the probe runs unconditionally, on every path.
func BuildCerberus(root, dir string) (string, error) {
	bin, err := resolveCerberusBin(root, dir)
	if err != nil {
		return "", err
	}
	if err := assertCerberusVersion(bin); err != nil {
		return "", err
	}
	return bin, nil
}

// resolveCerberusBin produces the binary path, from the environment or from a
// compile, without asserting anything about what that binary is.
func resolveCerberusBin(root, dir string) (string, error) {
	if bin := os.Getenv(cerberusBinEnv); bin != "" {
		info, err := os.Stat(bin) //nolint:gosec // the path is the harness operator's own build output
		if err != nil {
			return "", fmt.Errorf("migration harness: %s=%s: %w", cerberusBinEnv, bin, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("migration harness: %s=%s is a directory, not a binary", cerberusBinEnv, bin)
		}
		return bin, nil
	}

	out := filepath.Join(dir, binName)
	res, err := Run(RunSpec{
		Bin:  "go",
		Args: []string{"build", "-o", out, cerberusPkg},
		Dir:  root,
		Env:  os.Environ(),
	})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("migration harness: build %s: exit %d: %s",
			cerberusPkg, res.ExitCode, string(res.Stderr))
	}
	return out, nil
}

// assertCerberusVersion holds the resolved binary to the version this run
// expects. `--version` prints the bare version and nothing else, so anything
// but a single non-empty line is itself a failure.
func assertCerberusVersion(bin string) error {
	res, err := Run(RunSpec{Bin: bin, Args: []string{versionFlag}, Env: os.Environ()})
	if err != nil {
		return fmt.Errorf("migration harness: %s %s: %w", bin, versionFlag, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("migration harness: %s %s: exit %d: %s",
			bin, versionFlag, res.ExitCode, string(res.Stderr))
	}

	var lines []string
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) != 1 {
		return fmt.Errorf("migration harness: %s %s printed %d non-empty line(s), want exactly one bare version: %q",
			bin, versionFlag, len(lines), string(res.Stdout))
	}
	if want := ExpectedCLIVersion(); lines[0] != want {
		return fmt.Errorf("migration harness: %s reports version %q, but this run drives %q "+
			"(%s=%q, %s=%q). The scenarios would have exercised a different build than the run claims to test",
			bin, lines[0], want, ExpectVersionEnv, sharedExpectedVersion(), ImageEnv, ResolvedImage())
	}
	return nil
}

package lib

import (
	"fmt"
	"os"
	"path/filepath"
)

// cerberusBinEnv names a prebuilt cerberus binary the scenarios should drive.
// The migration workflow builds the binary once in its own step and hands the
// path in, so a matrix leg does not recompile it per scenario run.
const cerberusBinEnv = "CERBERUS_BIN"

// cerberusPkg is the main package the harness compiles when no prebuilt binary
// is supplied.
const cerberusPkg = "./cmd/cerberus"

// binName is the filename the freshly-built binary gets inside dir.
const binName = "cerberus"

// BuildCerberus returns the path of the cerberus binary the scenarios exec.
// CERBERUS_BIN short-circuits the build when a caller already produced one;
// otherwise the binary is compiled from source into dir, which the caller owns
// (a per-run temporary directory).
func BuildCerberus(root, dir string) (string, error) {
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

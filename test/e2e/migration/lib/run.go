package lib

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
)

// RunSpec is one child-process invocation: which binary, which arguments,
// which working directory, and the complete environment it runs under. Env
// replaces the parent environment rather than extending it, so a scenario's
// result cannot depend on a CERBERUS_* variable the developer happens to have
// exported.
type RunSpec struct {
	Bin  string
	Args []string
	Dir  string
	Env  []string
}

// Result is a finished child process: its captured streams, its exit code,
// and the complete environment it actually ran under. Env is spec.Env echoed
// back rather than re-derived, so a caller proving an offline property can
// assert against what the child process was really given instead of
// reconstructing a fresh value that would agree with itself by construction.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Env      []string
}

// Run executes spec and returns its captured output and exit code. A non-zero
// exit is reported in Result.ExitCode with a nil error; only a failure to start
// or to wait on the process (a missing binary, a permission error) is an error.
func Run(spec RunSpec) (Result, error) {
	cmd := exec.Command(spec.Bin, spec.Args...) //nolint:gosec // the binary and args are harness-authored, not user input
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Env: spec.Env}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	default:
		return res, fmt.Errorf(
			"migration harness: run %s %v: %w", filepath.Base(spec.Bin), spec.Args, err,
		)
	}
}

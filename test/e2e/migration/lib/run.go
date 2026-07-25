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

// Result is a finished child process: its captured streams and its exit code.
// The exit code is data, not an error — several scenarios assert a specific
// non-zero code (the gate's documented no-go status), which they could not do
// if a non-zero exit were reported as a failure.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
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
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}

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

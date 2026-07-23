package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestRootNoArgsReachesServerRunE pins the non-negotiable backward-compat
// contract: a bare `cerberus` invocation (no subcommand) must reach the server
// RunE — helm/compose/Docker ENTRYPOINT depend on this — and must NOT degrade to
// a help/usage dump.
func TestRootNoArgsReachesServerRunE(t *testing.T) {
	called := false
	root := newRootCmd(func() error { called = true; return nil })
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{})
	if err := root.Execute(); err != nil {
		t.Fatalf("bare `cerberus` execute: %v", err)
	}
	if !called {
		t.Fatal("bare `cerberus` must reach the server RunE, not print help")
	}
	if strings.Contains(out.String(), "Usage:") || strings.Contains(errBuf.String(), "Usage:") {
		t.Errorf("bare `cerberus` must not dump usage; stdout=%q stderr=%q", out.String(), errBuf.String())
	}
}

// TestServeSubcommandReachesServerRunE pins that `cerberus serve` is identical to
// the bare invocation: it reaches the server RunE.
func TestServeSubcommandReachesServerRunE(t *testing.T) {
	called := false
	root := newRootCmd(func() error { called = true; return nil })
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"serve"})
	if err := root.Execute(); err != nil {
		t.Fatalf("`cerberus serve` execute: %v", err)
	}
	if !called {
		t.Fatal("`cerberus serve` must reach the server RunE")
	}
}

// TestVersionSubcommandDumpsBareVersion pins that the `version` subcommand emits
// the bare Version string (the same output as the --version fast-path) and never
// starts the server.
func TestVersionSubcommandDumpsBareVersion(t *testing.T) {
	root := newRootCmd(func() error {
		t.Fatal("server RunE must not run for `version`")
		return nil
	})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("`cerberus version` execute: %v", err)
	}
	if got := strings.TrimRight(out.String(), "\n"); got != Version {
		t.Errorf("`version` subcommand = %q, want the bare version %q", got, Version)
	}
}

// TestPrintVersionBare pins that printVersion writes exactly the bare version and
// a trailing newline — the shared renderer the --version fast-path relies on for
// the distroless healthcheck.
func TestPrintVersionBare(t *testing.T) {
	var b bytes.Buffer
	printVersion(&b)
	if b.String() != Version+"\n" {
		t.Errorf("printVersion = %q, want %q", b.String(), Version+"\n")
	}
}

// TestExitCodeForError pins the typed-error → exit-code mapping main() applies
// after Execute: a parity-gate failure exits 2, a cutover-gate no-go exits 3, and
// any other error exits 1. Wrapped errors are discriminated via errors.As.
func TestExitCodeForError(t *testing.T) {
	if c := exitCodeForError(verifyFailedError{}); c != verifyExitFail {
		t.Errorf("verifyFailedError exit = %d, want %d", c, verifyExitFail)
	}
	if c := exitCodeForError(gateFailedError{}); c != gateExitFail {
		t.Errorf("gateFailedError exit = %d, want %d", c, gateExitFail)
	}
	if c := exitCodeForError(errors.New("some tool error")); c != 1 {
		t.Errorf("generic error exit = %d, want 1", c)
	}
}

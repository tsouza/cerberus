package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/config"
)

// TestRun_NoFlagsIsError pins that invoking the tool with nothing to do reports
// an error (and prints usage) rather than silently succeeding.
func TestRun_NoFlagsIsError(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(nil, &out, &errOut); err == nil {
		t.Fatal("run with no flags should error")
	}
	if out.Len() != 0 {
		t.Errorf("no schema should be written to stdout on error, got: %q", out.String())
	}
}

// TestRun_UnknownFlagIsError pins that an unknown flag surfaces the flag
// package's parse error instead of proceeding.
func TestRun_UnknownFlagIsError(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"--nope"}, &out, &errOut); err == nil {
		t.Fatal("run with an unknown flag should error")
	}
}

// TestStringListFlag pins that --rules accumulates both repeated flags and
// comma-separated values, trimming blanks.
func TestStringListFlag(t *testing.T) {
	var l stringList
	if err := l.Set("a.yml, b.yml"); err != nil {
		t.Fatal(err)
	}
	if err := l.Set(" c.yml "); err != nil {
		t.Fatal(err)
	}
	got := strings.Join([]string(l), "|")
	if got != "a.yml|b.yml|c.yml" {
		t.Errorf("stringList = %q, want a.yml|b.yml|c.yml", got)
	}
}

// TestRunExplainEndToEnd runs the explain mode offline over a temp rules file:
// a valid recording rule must produce emitted SQL, and a deliberately-broken
// expr must be marked UNSUPPORTED — the build keeps going past the bad one.
func TestRunExplainEndToEnd(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "rules.yml")
	const rules = `
groups:
  - name: probe
    rules:
      - record: job:up
        expr: up
      - alert: BrokenExpr
        expr: "!!! not valid promql"
`
	if err := os.WriteFile(file, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := run([]string{"--rules", file}, &out, &errOut); err != nil {
		t.Fatalf("run --rules: %v (stderr: %s)", err, errOut.String())
	}
	got := out.String()

	if !strings.Contains(got, "SELECT") {
		t.Errorf("explain report should contain the emitted SQL for `up`, got:\n%s", got)
	}
	if !strings.Contains(got, "UNSUPPORTED") {
		t.Errorf("explain report should mark the broken expr UNSUPPORTED, got:\n%s", got)
	}
	if !strings.Contains(got, "cardinality is NOT knowable offline") {
		t.Errorf("explain report should carry the offline-cardinality honesty note, got:\n%s", got)
	}
}

// TestWriteSchema pins the render path end to end (offline): a config with a
// database + table overrides produces pipeable DDL that creates the database
// first and the overridden tables after, each statement ';'-terminated.
func TestWriteSchema(t *testing.T) {
	cfg := config.Config{
		ClickHouse: chclient.Config{Database: "otel"},
	}

	var out bytes.Buffer
	if err := writeSchema(&out, cfg); err != nil {
		t.Fatalf("writeSchema: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "CREATE DATABASE IF NOT EXISTS otel") {
		t.Errorf("expected CREATE DATABASE for the configured database, got:\n%s", got)
	}
	if !strings.Contains(got, "CREATE TABLE IF NOT EXISTS") {
		t.Errorf("expected CREATE TABLE statements, got:\n%s", got)
	}
	// The database must be created before any table references it.
	if db, tbl := strings.Index(got, "CREATE DATABASE"), strings.Index(got, "CREATE TABLE"); db > tbl {
		t.Errorf("CREATE DATABASE must precede CREATE TABLE (db@%d, table@%d)", db, tbl)
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), ";") {
		t.Errorf("rendered schema must be ';'-terminated for clickhouse-client, got tail: %q",
			got[max(0, len(got)-40):])
	}
}

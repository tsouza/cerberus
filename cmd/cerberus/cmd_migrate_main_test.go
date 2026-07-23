package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/migrate"
)

// TestRun_NoFlagsIsError pins that invoking the tool with nothing to do reports
// an error (and prints usage) rather than silently succeeding.
func TestRun_NoFlagsIsError(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := runMigrate(nil, &out, &errOut); err == nil {
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
	if err := runMigrate([]string{"--nope"}, &out, &errOut); err == nil {
		t.Fatal("run with an unknown flag should error")
	}
}

// TestRunHelpExitsCleanToStdout pins that -h/--help prints usage to stdout and
// returns no error (exit 0), with no spurious "flag: help requested" error line
// on stderr.
func TestRunHelpExitsCleanToStdout(t *testing.T) {
	for _, flagArg := range []string{"-h", "--help"} {
		var out, errOut bytes.Buffer
		if err := runMigrate([]string{flagArg}, &out, &errOut); err != nil {
			t.Fatalf("run %s should exit cleanly, got error: %v", flagArg, err)
		}
		if out.Len() == 0 {
			t.Errorf("run %s should print usage to stdout", flagArg)
		}
		if errOut.Len() != 0 {
			t.Errorf("run %s should write nothing to stderr, got: %q", flagArg, errOut.String())
		}
	}
}

// TestSubcommandHelpExitsClean pins that every subcommand's -h likewise exits 0
// with usage on stdout and nothing on stderr — the fix applies to every flagset.
func TestSubcommandHelpExitsClean(t *testing.T) {
	for _, sc := range []string{"harvest", "explain", "classify", "rulegraph", "verify", "inventory", "gate"} {
		var out, errOut bytes.Buffer
		if err := runMigrate([]string{sc, "-h"}, &out, &errOut); err != nil {
			t.Errorf("run %s -h should exit cleanly, got: %v", sc, err)
		}
		if out.Len() == 0 {
			t.Errorf("run %s -h should print usage to stdout", sc)
		}
		if errOut.Len() != 0 {
			t.Errorf("run %s -h should write nothing to stderr, got: %q", sc, errOut.String())
		}
	}
}

// TestRunUnknownSubcommand pins that a mistyped subcommand is a clear error (not a
// silent fall-through to the root flags that prints "nothing to do").
func TestRunUnknownSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMigrate([]string{"verifyy"}, &out, &errOut)
	if err == nil {
		t.Fatal("an unknown subcommand should error")
	}
	if !strings.Contains(err.Error(), "unknown command") || !strings.Contains(err.Error(), "verifyy") {
		t.Errorf("error should name the unknown subcommand, got: %v", err)
	}
	if strings.Contains(err.Error(), "nothing to do") {
		t.Errorf("unknown subcommand must not fall through to the root 'nothing to do', got: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("unknown subcommand should write nothing to stdout, got: %q", out.String())
	}
}

// TestRootUsageListsSubcommands pins that the root usage names every subcommand so
// an operator can discover them.
func TestRootUsageListsSubcommands(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"-h"}, &out, &errOut); err != nil {
		t.Fatalf("run -h: %v", err)
	}
	usage := out.String()
	for _, name := range []string{"schema", "harvest", "explain", "classify", "rulegraph", "verify", "inventory", "gate"} {
		if !strings.Contains(usage, name) {
			t.Errorf("root usage should list subcommand %q, got:\n%s", name, usage)
		}
	}
}

// TestCorpusCommandsPrintUsageOnMissingInput pins that the corpus subcommands
// (harvest / explain / classify), like rulegraph, print flag usage on stderr
// when no corpus-input flag is supplied — a consistent usage-error UX rather than
// a bare error line.
func TestCorpusCommandsPrintUsageOnMissingInput(t *testing.T) {
	for _, sc := range []string{"harvest", "explain", "classify"} {
		var out, errOut bytes.Buffer
		if err := runMigrate([]string{sc}, &out, &errOut); err == nil {
			t.Errorf("%s with no inputs should error", sc)
		}
		if !strings.Contains(errOut.String(), "Usage:") || !strings.Contains(errOut.String(), "migrate "+sc) {
			t.Errorf("%s with no inputs should print usage on stderr, got: %q", sc, errOut.String())
		}
	}
}

// TestNormalizeList pins that the --rules normalizer reproduces the legacy
// stringList semantics on top of cobra's StringSlice accumulation: each element
// is trimmed and blanks are dropped, so a repeatable + comma-separated flag
// (which cobra splits into raw elements) accumulates a clean list.
func TestNormalizeList(t *testing.T) {
	got := strings.Join(normalizeList([]string{"a.yml", " b.yml", " c.yml ", ""}), "|")
	if got != "a.yml|b.yml|c.yml" {
		t.Errorf("normalizeList = %q, want a.yml|b.yml|c.yml", got)
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
	if err := runMigrate([]string{"explain", "--rules", file}, &out, &errOut); err != nil {
		t.Fatalf("explain --rules: %v (stderr: %s)", err, errOut.String())
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

// TestHarvestThenExplainCorpus drives the composed flow end to end: harvest a
// corpus from a rules file plus a dashboard (with a Prometheus panel, a nested
// row, and a Loki panel harvested as LogQL) to a file, then explain that corpus
// file. The corpus is deterministic and the explain reads it back; the LogQL
// query is carried into the corpus but reported as not-previewed-offline.
func TestHarvestThenExplainCorpus(t *testing.T) {
	dir := t.TempDir()

	rulesFile := filepath.Join(dir, "rules.yml")
	const rules = `
groups:
  - name: cpu
    rules:
      - record: job:up
        expr: up
`
	if err := os.WriteFile(rulesFile, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}

	dashDir := filepath.Join(dir, "dash")
	if err := os.MkdirAll(dashDir, 0o750); err != nil {
		t.Fatal(err)
	}
	const dashboard = `{
  "panels": [
    {"id": 1, "title": "reqs", "datasource": {"type": "prometheus"},
     "targets": [{"refId": "A", "expr": "sum(rate(http_requests_total[5m]))"}]},
    {"id": 2, "title": "logs", "datasource": {"type": "loki"},
     "targets": [{"refId": "A", "expr": "{app=\"x\"}"}]},
    {"id": 3, "title": "row", "type": "row", "panels": [
      {"id": 4, "title": "nested", "datasource": {"type": "prometheus"},
       "targets": [{"refId": "A", "expr": "node_load1"}]}
    ]}
  ]
}`
	if err := os.WriteFile(filepath.Join(dashDir, "board.json"), []byte(dashboard), 0o600); err != nil {
		t.Fatal(err)
	}

	corpusFile := filepath.Join(dir, "corpus.json")

	// harvest → corpus.json
	var hOut, hErr bytes.Buffer
	if err := runMigrate([]string{"harvest", "--rules", rulesFile, "--dashboards", dashDir, "--out", corpusFile}, &hOut, &hErr); err != nil {
		t.Fatalf("harvest: %v (stderr: %s)", err, hErr.String())
	}

	data, err := os.ReadFile(corpusFile) //nolint:gosec // test-controlled temp path.
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var corpus migrate.Corpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatalf("corpus is not valid JSON: %v\n%s", err, data)
	}
	if corpus.Version != migrate.CorpusVersion {
		t.Errorf("corpus version = %d, want %d", corpus.Version, migrate.CorpusVersion)
	}
	// 1 rule + 2 prometheus panel targets (top-level + nested row) + 1 Loki panel
	// = 4 queries; nothing dropped now that the corpus is three-headed.
	if len(corpus.Queries) != 4 {
		t.Fatalf("corpus queries = %d, want 4: %+v", len(corpus.Queries), corpus.Queries)
	}
	if len(corpus.Skipped) != 0 {
		t.Fatalf("expected 0 skips (Loki panel harvested as LogQL), got %+v", corpus.Skipped)
	}
	langs := map[string]int{}
	for _, q := range corpus.Queries {
		if q.Lang == "" {
			t.Errorf("query %q carries no lang tag", q.Expr)
		}
		langs[q.Lang]++
	}
	if langs[migrate.LangPromQL] != 3 || langs[migrate.LangLogQL] != 1 {
		t.Errorf("unexpected lang distribution: %+v", langs)
	}

	// Harvest is deterministic: a second harvest to a fresh file is byte-identical.
	corpusFile2 := filepath.Join(dir, "corpus2.json")
	var h2Out, h2Err bytes.Buffer
	if err := runMigrate([]string{"harvest", "--rules", rulesFile, "--dashboards", dashDir, "--out", corpusFile2}, &h2Out, &h2Err); err != nil {
		t.Fatalf("harvest (2): %v (stderr: %s)", err, h2Err.String())
	}
	data2, err := os.ReadFile(corpusFile2) //nolint:gosec // test-controlled temp path.
	if err != nil {
		t.Fatalf("read corpus2: %v", err)
	}
	if !bytes.Equal(data, data2) {
		t.Errorf("harvest is not deterministic:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", data, data2)
	}

	// explain --corpus reads the harvested corpus back and dry-runs it.
	var eOut, eErr bytes.Buffer
	if err := runMigrate([]string{"explain", "--corpus", corpusFile}, &eOut, &eErr); err != nil {
		t.Fatalf("explain --corpus: %v (stderr: %s)", err, eErr.String())
	}
	report := eOut.String()
	if !strings.Contains(report, "SELECT") {
		t.Errorf("explain report should contain emitted SQL, got:\n%s", report)
	}
	if !strings.Contains(report, "node_load1") {
		t.Errorf("explain report should include the nested-row panel query, got:\n%s", report)
	}
	// The LogQL query is carried into the corpus (4 queries, 0 skips) but the
	// offline SQL preview covers PromQL only, so it is reported UNSUPPORTED with an
	// honest reason rather than parsed as PromQL.
	if !strings.Contains(report, "4 queries, 0 skipped") {
		t.Errorf("explain report should report 4 queries and 0 skips, got:\n%s", report)
	}
	if !strings.Contains(report, "offline SQL preview covers PromQL only") {
		t.Errorf("explain report should flag the LogQL query as not previewed offline, got:\n%s", report)
	}
}

// TestExplainToOutFile pins that `explain --out <file>` writes the report to the
// named file (checked, via os.WriteFile) rather than only to stdout: the file
// carries the emitted SQL and nothing is written to stdout on the file path.
func TestExplainToOutFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "rules.yml")
	const rules = `
groups:
  - name: probe
    rules:
      - record: job:up
        expr: up
`
	if err := os.WriteFile(file, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	reportFile := filepath.Join(dir, "report.txt")

	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"explain", "--rules", file, "--out", reportFile}, &out, &errOut); err != nil {
		t.Fatalf("explain --out: %v (stderr: %s)", err, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("explain --out should not write the report to stdout, got: %q", out.String())
	}

	data, err := os.ReadFile(reportFile) //nolint:gosec // test-controlled temp path.
	if err != nil {
		t.Fatalf("read report file: %v", err)
	}
	report := string(data)
	if !strings.Contains(report, "SELECT") {
		t.Errorf("report file should contain the emitted SQL, got:\n%s", report)
	}
	if !strings.Contains(report, "cardinality is NOT knowable offline") {
		t.Errorf("report file should carry the offline-cardinality honesty note, got:\n%s", report)
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

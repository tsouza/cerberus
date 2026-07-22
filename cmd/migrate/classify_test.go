package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/migrate"
)

// TestClassifyEndToEnd runs `migrate classify` over a small rule file through the
// REAL offline pipeline (parse -> lower -> emit via engine.DryRunSQL, no
// ClickHouse). One clean query buckets as supported, one syntactically broken
// query buckets as unsupported with its construct named, and the honesty header
// warns that "supported" is translation, not result parity.
func TestClassifyEndToEnd(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "rules.yml")
	// `up` lowers + emits cleanly; `up{` is a real PromQL parse error, so the
	// engine rejects it and it must land in the unsupported bucket, not vanish.
	const rules = `
groups:
  - name: mix
    rules:
      - record: job:up
        expr: up
      - alert: Broken
        expr: "up{"
`
	if err := os.WriteFile(file, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := run([]string{"classify", "--rules", file}, &out, &errOut); err != nil {
		t.Fatalf("classify: %v (stderr: %s)", err, errOut.String())
	}
	report := out.String()

	// Two rules -> two queries: `up` supported, `up{` unsupported.
	for _, want := range []string{
		"2 queries: 1 supported (0 risky), 1 unsupported; 0 skipped",
		"job:up",
		"only `migrate verify` proves parity",
		// The unsupported query must NAME its offending construct (the engine
		// error), under the unsupported bucket — never silently dropped.
		"construct:",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("classify report missing %q\n---\n%s", want, report)
		}
	}
}

// TestClassifyJSONEndToEnd pins the --json path over the real pipeline: the
// ledger unmarshals, the counts split 1 supported / 1 unsupported, and the
// unsupported entry carries a non-empty construct.
func TestClassifyJSONEndToEnd(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "rules.yml")
	const rules = `
groups:
  - name: mix
    rules:
      - record: job:up
        expr: up
      - alert: Broken
        expr: "up{"
`
	if err := os.WriteFile(file, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := run([]string{"classify", "--rules", file, "--json"}, &out, &errOut); err != nil {
		t.Fatalf("classify --json: %v (stderr: %s)", err, errOut.String())
	}

	var cl migrate.Classification
	if err := json.Unmarshal(out.Bytes(), &cl); err != nil {
		t.Fatalf("unmarshal classify JSON: %v\n%s", err, out.String())
	}
	if cl.Counts.Total != 2 || cl.Counts.Supported != 1 || cl.Counts.Unsupported != 1 {
		t.Fatalf("counts = %+v, want total 2 / supported 1 / unsupported 1", cl.Counts)
	}
	var sawNamedConstruct bool
	for _, q := range cl.Queries {
		if q.Bucket == migrate.BucketUnsupported {
			if q.Construct == "" {
				t.Errorf("unsupported query %q has an empty construct", q.Expr)
			} else {
				sawNamedConstruct = true
			}
		}
	}
	if !sawNamedConstruct {
		t.Error("expected at least one unsupported query with a named construct")
	}
}

// TestClassifyToOutFile pins that `classify --out <file>` writes the ledger to
// the named file (checked write) and nothing to stdout.
func TestClassifyToOutFile(t *testing.T) {
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
	ledger := filepath.Join(dir, "classify.txt")

	var out, errOut bytes.Buffer
	if err := run([]string{"classify", "--rules", file, "--out", ledger}, &out, &errOut); err != nil {
		t.Fatalf("classify --out: %v (stderr: %s)", err, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("classify --out should not write to stdout, got: %q", out.String())
	}
	data, err := os.ReadFile(ledger) //nolint:gosec // test-controlled temp path.
	if err != nil {
		t.Fatalf("read ledger file: %v", err)
	}
	if !strings.Contains(string(data), "1 queries: 1 supported") {
		t.Errorf("ledger file should carry the counts line, got:\n%s", string(data))
	}
}

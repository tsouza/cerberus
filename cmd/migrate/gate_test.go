package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/migrateverify"
)

// writeJSONFile renders one block's JSON (via its real WriteJSON writer) to a
// file in dir, so the gate CLI reads exactly what the sibling commands emit.
func writeJSONFile(t *testing.T, dir, name string, wj func(io.Writer) error) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if err := wj(f); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
	return p
}

func cleanVerifyFile(t *testing.T, dir string) string {
	rep := migrateverify.Report{Summary: migrateverify.Summary{Total: 1, Match: 1}}
	return writeJSONFile(t, dir, "verify.json", rep.WriteJSON)
}

func cleanClassifyFile(t *testing.T, dir string) string {
	cl := migrate.Classification{Counts: migrate.BucketCounts{Total: 1, Supported: 1}}
	return writeJSONFile(t, dir, "classify.json", cl.WriteJSON)
}

func orphanRuleGraphFile(t *testing.T, dir string) string {
	g := migrate.RuleGraph{
		Counts:   migrate.RuleGraphCounts{Recorded: 1, Orphan: 1},
		Recorded: []migrate.RecordedNode{{Name: "job:x", Source: "r", Status: migrate.StatusOrphan}},
	}
	return writeJSONFile(t, dir, "rulegraph.json", g.WriteJSON)
}

// TestRunGate_PassAllClean: clean required artifacts → runGate returns nil (the
// gate passes) and prints an overall PASS.
func TestRunGate_PassAllClean(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := runGate([]string{
		"--verify", cleanVerifyFile(t, dir),
		"--classify", cleanClassifyFile(t, dir),
		"--rulegraph", orphanRuleGraphFile(t, dir),
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runGate returned error on clean artifacts: %v", err)
	}
	if !strings.Contains(stdout.String(), "OVERALL: PASS") {
		t.Errorf("want OVERALL: PASS in output, got:\n%s", stdout.String())
	}
}

// TestRunGate_FailsOnConsumedSeries: a consumed recorded series → runGate returns
// a gateFailedError (which main maps to a non-zero exit) and prints OVERALL: FAIL.
func TestRunGate_FailsOnConsumedSeries(t *testing.T) {
	dir := t.TempDir()
	g := migrate.RuleGraph{
		Counts:    migrate.RuleGraphCounts{Recorded: 1, Consumed: 1, Consumers: 1},
		Recorded:  []migrate.RecordedNode{{Name: "job:x", Source: "r", Status: migrate.StatusConsumed, Consumers: []string{"dash.json"}}},
		Consumers: []migrate.ConsumerNode{{Expr: "job:x", Source: "dash.json", Kind: "dashboard", References: []string{"job:x"}}},
	}
	var stdout, stderr bytes.Buffer
	err := runGate([]string{
		"--verify", cleanVerifyFile(t, dir),
		"--classify", cleanClassifyFile(t, dir),
		"--rulegraph", writeJSONFile(t, dir, "rulegraph.json", g.WriteJSON),
	}, &stdout, &stderr)

	var gate gateFailedError
	if !errors.As(err, &gate) {
		t.Fatalf("want gateFailedError, got %v", err)
	}
	if !strings.Contains(stdout.String(), "OVERALL: FAIL") {
		t.Errorf("want OVERALL: FAIL in output, got:\n%s", stdout.String())
	}
}

// TestRunGate_FailsOnMissingRequired: omitting a required artifact blocks the
// gate — the CLI must not pass by omission.
func TestRunGate_FailsOnMissingRequired(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	// rulegraph omitted (required) → BLOCK.
	err := runGate([]string{
		"--verify", cleanVerifyFile(t, dir),
		"--classify", cleanClassifyFile(t, dir),
	}, &stdout, &stderr)

	var gate gateFailedError
	if !errors.As(err, &gate) {
		t.Fatalf("want gateFailedError on missing required artifact, got %v", err)
	}
	if !strings.Contains(stdout.String(), "missing artifacts") {
		t.Errorf("want a missing-artifacts note in output, got:\n%s", stdout.String())
	}
}

// TestRunGate_JSONOutput: --json emits a machine-readable decision that parses
// and reflects the failing verdict.
func TestRunGate_JSONOutput(t *testing.T) {
	dir := t.TempDir()
	rep := migrateverify.Report{Summary: migrateverify.Summary{Total: 1, Diverge: 1}}
	var stdout, stderr bytes.Buffer
	err := runGate([]string{
		"--json",
		"--verify", writeJSONFile(t, dir, "verify.json", rep.WriteJSON),
		"--classify", cleanClassifyFile(t, dir),
		"--rulegraph", orphanRuleGraphFile(t, dir),
	}, &stdout, &stderr)

	var gate gateFailedError
	if !errors.As(err, &gate) {
		t.Fatalf("want gateFailedError, got %v", err)
	}
	var dec struct {
		Pass    bool `json:"pass"`
		Overall string
		Stages  []struct {
			Stage    string
			Verdict  string
			Blocking bool
		} `json:"stages"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &dec); err != nil {
		t.Fatalf("gate --json output does not parse: %v\n%s", err, stdout.String())
	}
	if dec.Pass || dec.Overall != "FAIL" {
		t.Errorf("want pass=false overall=FAIL, got %+v", dec)
	}
	var found bool
	for _, s := range dec.Stages {
		if s.Stage == "verify" {
			found = true
			if s.Verdict != "FAIL" || !s.Blocking {
				t.Errorf("verify stage: want FAIL+blocking, got %+v", s)
			}
		}
	}
	if !found {
		t.Errorf("verify stage missing from JSON output")
	}
}

// TestRunGate_OutFile: --out writes the decision to a file rather than stdout.
func TestRunGate_OutFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "decision.txt")
	var stdout, stderr bytes.Buffer
	err := runGate([]string{
		"--verify", cleanVerifyFile(t, dir),
		"--classify", cleanClassifyFile(t, dir),
		"--rulegraph", orphanRuleGraphFile(t, dir),
		"--out", out,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runGate --out: %v", err)
	}
	data, err := os.ReadFile(out) //nolint:gosec // test-controlled path.
	if err != nil {
		t.Fatalf("read out file: %v", err)
	}
	if !strings.Contains(string(data), "OVERALL: PASS") {
		t.Errorf("out file should contain the decision, got:\n%s", data)
	}
}

// TestRunGate_UnparseableArtifactIsError: a supplied-but-corrupt artifact is a
// hard error, not a gate FAIL and not a silent skip.
func TestRunGate_UnparseableArtifactIsError(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "verify.json")
	if err := os.WriteFile(bad, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := runGate([]string{"--verify", bad, "--classify", cleanClassifyFile(t, dir)}, &stdout, &stderr)
	if err == nil {
		t.Fatal("want a hard error on a corrupt artifact")
	}
	var gate gateFailedError
	if errors.As(err, &gate) {
		t.Fatal("a corrupt artifact must be a tool error, not a gate FAIL verdict")
	}
}

// TestRun_GateDispatch: the top-level dispatcher routes `gate` to runGate.
func TestRun_GateDispatch(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"gate",
		"--verify", cleanVerifyFile(t, dir),
		"--classify", cleanClassifyFile(t, dir),
		"--rulegraph", orphanRuleGraphFile(t, dir),
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run gate dispatch: %v", err)
	}
	if !strings.Contains(stdout.String(), "OVERALL: PASS") {
		t.Errorf("want PASS via dispatcher, got:\n%s", stdout.String())
	}
}

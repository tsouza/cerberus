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

// writeRuleGraphCorpus writes a minimal harvested corpus.json for the rulegraph tests: a
// single panel query that references the recorded series, so the graph has a real
// dashboard consumer to link. It uses migrate.BuildCorpus so the version stamp
// and shape match what `migrate harvest` produces.
func writeRuleGraphCorpus(t *testing.T, path string, queries ...migrate.HarvestedQuery) {
	t.Helper()
	c := migrate.BuildCorpus(queries, nil)
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("marshal corpus: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRuleGraphEndToEnd runs `migrate rulegraph` over a rule file (one recording
// rule consumed by a dashboard query, one orphan recording rule) plus a corpus
// through the REAL name extractor, asserting the consumed/orphan classification,
// the edge back to the dashboard consumer, and the honesty header.
func TestRuleGraphEndToEnd(t *testing.T) {
	dir := t.TempDir()
	ruleFile := filepath.Join(dir, "rules.yml")
	const rules = `
groups:
  - name: api
    rules:
      - record: job:http_requests:rate5m
        expr: rate(http_requests_total[5m])
      - record: job:errors:rate5m
        expr: rate(http_errors_total[5m])
`
	if err := os.WriteFile(ruleFile, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	corpusFile := filepath.Join(dir, "corpus.json")
	writeRuleGraphCorpus(t, corpusFile, migrate.HarvestedQuery{
		Expr:   `sum(job:http_requests:rate5m{env="prod"})`,
		Source: "dash:overview/reqs",
		Kind:   migrate.KindPanel,
	})

	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"rulegraph", "--rules", ruleFile, "--corpus", corpusFile}, &out, &errOut); err != nil {
		t.Fatalf("rulegraph: %v (stderr: %s)", err, errOut.String())
	}
	report := out.String()

	for _, want := range []string{
		"2 recorded series: 1 consumed, 1 orphan; 1 consumers; 0 skipped",
		"NAME-LEVEL dependency approximation",
		// the consumed series names its consumer edge
		"job:http_requests:rate5m",
		"<- dash:overview/reqs",
		// the orphan series is listed under orphans
		"job:errors:rate5m",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("rulegraph report missing %q\n---\n%s", want, report)
		}
	}
}

// TestRuleGraphJSONEndToEnd pins the --json path: the graph unmarshals, the
// counts split 1 consumed / 1 orphan, and the consumed node carries its
// consumer edge.
func TestRuleGraphJSONEndToEnd(t *testing.T) {
	dir := t.TempDir()
	ruleFile := filepath.Join(dir, "rules.yml")
	const rules = `
groups:
  - name: api
    rules:
      - record: job:http_requests:rate5m
        expr: rate(http_requests_total[5m])
      - record: job:errors:rate5m
        expr: rate(http_errors_total[5m])
`
	if err := os.WriteFile(ruleFile, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	corpusFile := filepath.Join(dir, "corpus.json")
	writeRuleGraphCorpus(t, corpusFile, migrate.HarvestedQuery{
		Expr:   "job:http_requests:rate5m",
		Source: "dash:overview/reqs",
		Kind:   migrate.KindPanel,
	})

	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"rulegraph", "--rules", ruleFile, "--corpus", corpusFile, "--json"}, &out, &errOut); err != nil {
		t.Fatalf("rulegraph --json: %v (stderr: %s)", err, errOut.String())
	}

	var g migrate.RuleGraph
	if err := json.Unmarshal(out.Bytes(), &g); err != nil {
		t.Fatalf("unmarshal rulegraph JSON: %v\n%s", err, out.String())
	}
	if g.Counts.Recorded != 2 || g.Counts.Consumed != 1 || g.Counts.Orphan != 1 {
		t.Fatalf("counts = %+v, want recorded 2 / consumed 1 / orphan 1", g.Counts)
	}
	var sawEdge bool
	for _, n := range g.Recorded {
		if n.Name == "job:http_requests:rate5m" {
			if n.Status != migrate.StatusConsumed {
				t.Errorf("job:http_requests:rate5m status = %q, want consumed", n.Status)
			}
			for _, c := range n.Consumers {
				if c == "dash:overview/reqs" {
					sawEdge = true
				}
			}
		}
	}
	if !sawEdge {
		t.Error("expected consumed node to carry an edge back to dash:overview/reqs")
	}
}

// TestRuleGraphNoInputs pins that rulegraph refuses to run with neither --rules
// nor --corpus rather than emitting an empty graph.
func TestRuleGraphNoInputs(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"rulegraph"}, &out, &errOut); err == nil {
		t.Fatal("rulegraph with no inputs should error")
	}
}

// TestRuleGraphToOutFile pins that `rulegraph --out <file>` writes to the named
// file and nothing to stdout.
func TestRuleGraphToOutFile(t *testing.T) {
	dir := t.TempDir()
	ruleFile := filepath.Join(dir, "rules.yml")
	const rules = `
groups:
  - name: g
    rules:
      - record: job:up
        expr: up
`
	if err := os.WriteFile(ruleFile, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	graphFile := filepath.Join(dir, "graph.txt")

	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"rulegraph", "--rules", ruleFile, "--out", graphFile}, &out, &errOut); err != nil {
		t.Fatalf("rulegraph --out: %v (stderr: %s)", err, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("rulegraph --out should not write to stdout, got: %q", out.String())
	}
	data, err := os.ReadFile(graphFile) //nolint:gosec // test-controlled temp path.
	if err != nil {
		t.Fatalf("read graph file: %v", err)
	}
	if !strings.Contains(string(data), "1 recorded series") {
		t.Errorf("graph file should carry the counts line, got:\n%s", string(data))
	}
}

// TestRuleGraphLegacyInvocationUnaffectedWithoutLokiRules pins that the
// --loki-rules flag's mere existence on the command changes nothing: the
// output for an invocation that never passes it is byte-identical to the
// output before the flag was added. This is what makes MIG-04's committed
// golden safe without regeneration.
func TestRuleGraphLegacyInvocationUnaffectedWithoutLokiRules(t *testing.T) {
	dir := t.TempDir()
	ruleFile := filepath.Join(dir, "rules.yml")
	const rules = `
groups:
  - name: api
    rules:
      - record: job:http_requests:rate5m
        expr: rate(http_requests_total[5m])
      - record: job:errors:rate5m
        expr: rate(http_errors_total[5m])
`
	if err := os.WriteFile(ruleFile, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	corpusFile := filepath.Join(dir, "corpus.json")
	writeRuleGraphCorpus(t, corpusFile, migrate.HarvestedQuery{
		Expr:   `sum(job:http_requests:rate5m{env="prod"})`,
		Source: "dash:overview/reqs",
		Kind:   migrate.KindPanel,
	})

	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"rulegraph", "--rules", ruleFile, "--corpus", corpusFile, "--json"}, &out, &errOut); err != nil {
		t.Fatalf("rulegraph --json: %v (stderr: %s)", err, errOut.String())
	}

	var withoutFlag, withEmptyFlag bytes.Buffer
	if err := runMigrate([]string{"rulegraph", "--rules", ruleFile, "--corpus", corpusFile, "--json"}, &withoutFlag, &errOut); err != nil {
		t.Fatalf("rulegraph --json (rerun): %v (stderr: %s)", err, errOut.String())
	}
	if err := runMigrate([]string{"rulegraph", "--rules", ruleFile, "--loki-rules", "", "--corpus", corpusFile, "--json"}, &withEmptyFlag, &errOut); err != nil {
		t.Fatalf("rulegraph --json --loki-rules '': %v (stderr: %s)", err, errOut.String())
	}

	if out.String() != withoutFlag.String() {
		t.Fatalf("rulegraph output is not even reproducible across two runs with identical flags:\n%s\n---\n%s", out.String(), withoutFlag.String())
	}
	if out.String() != withEmptyFlag.String() {
		t.Fatalf("rulegraph output changed by the mere presence of an empty --loki-rules flag:\n%s\n---\n%s", out.String(), withEmptyFlag.String())
	}
}

// TestRuleGraphLokiRulesHarvestsRecordedSeries pins the CLI wiring: a
// --loki-rules file's record: output is folded into the graph's recorded
// series, tagged with the loki-rule: source prefix, and linked by a corpus
// consumer exactly like a Prometheus-recorded series.
func TestRuleGraphLokiRulesHarvestsRecordedSeries(t *testing.T) {
	dir := t.TempDir()
	lokiRuleFile := filepath.Join(dir, "loki-rules.yml")
	const lokiRules = `
groups:
  - name: g
    rules:
      - record: job:error_lines:rate5m
        expr: sum(rate({app="api"} |= "ERROR" [5m]))
      - alert: HighErrorLines
        expr: '{app="api"} |= "ERROR"'
`
	if err := os.WriteFile(lokiRuleFile, []byte(lokiRules), 0o600); err != nil {
		t.Fatal(err)
	}
	corpusFile := filepath.Join(dir, "corpus.json")
	writeRuleGraphCorpus(t, corpusFile, migrate.HarvestedQuery{
		Expr:   `sum(job:error_lines:rate5m{env="prod"})`,
		Source: "dash:overview/errlines",
		Kind:   migrate.KindPanel,
	})

	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"rulegraph", "--loki-rules", lokiRuleFile, "--corpus", corpusFile, "--json"}, &out, &errOut); err != nil {
		t.Fatalf("rulegraph --loki-rules: %v (stderr: %s)", err, errOut.String())
	}

	var g migrate.RuleGraph
	if err := json.Unmarshal(out.Bytes(), &g); err != nil {
		t.Fatalf("unmarshal rulegraph JSON: %v\n%s", err, out.String())
	}
	if g.Counts.Recorded != 1 || g.Counts.Consumed != 1 || g.Counts.Orphan != 0 {
		t.Fatalf("counts = %+v, want recorded 1 / consumed 1 / orphan 0", g.Counts)
	}
	if len(g.Recorded) != 1 {
		t.Fatalf("recorded = %+v, want exactly 1 (the alert: rule is not harvested)", g.Recorded)
	}
	n := g.Recorded[0]
	if n.Name != "job:error_lines:rate5m" {
		t.Errorf("recorded[0].Name = %q, want job:error_lines:rate5m", n.Name)
	}
	if !strings.HasPrefix(n.Source, "loki-rule:") {
		t.Errorf("recorded[0].Source = %q, want loki-rule: prefix", n.Source)
	}
	if n.Status != migrate.StatusConsumed {
		t.Errorf("recorded[0].Status = %q, want %q", n.Status, migrate.StatusConsumed)
	}
	if len(n.Consumers) != 1 || n.Consumers[0] != "dash:overview/errlines" {
		t.Errorf("recorded[0].Consumers = %v, want [dash:overview/errlines]", n.Consumers)
	}
}

// TestRuleGraphLokiRulesNoFilesMatchedBlocks pins that a --loki-rules glob
// matching zero files surfaces as a counted skip, not a silent empty graph —
// the same posture --rules already has for a missed glob.
func TestRuleGraphLokiRulesNoFilesMatchedBlocks(t *testing.T) {
	dir := t.TempDir()
	pattern := filepath.Join(dir, "does-not-exist-*.yml")

	var out, errOut bytes.Buffer
	if err := runMigrate([]string{"rulegraph", "--loki-rules", pattern, "--json"}, &out, &errOut); err != nil {
		t.Fatalf("rulegraph --loki-rules (missing glob): %v (stderr: %s)", err, errOut.String())
	}

	var g migrate.RuleGraph
	if err := json.Unmarshal(out.Bytes(), &g); err != nil {
		t.Fatalf("unmarshal rulegraph JSON: %v\n%s", err, out.String())
	}
	if g.Counts.Skipped != 1 {
		t.Fatalf("counts.Skipped = %d, want 1", g.Counts.Skipped)
	}
	if len(g.Skipped) != 1 || g.Skipped[0].Source != pattern || g.Skipped[0].Reason != "no files matched" {
		t.Fatalf("skipped = %+v, want one entry naming %q with reason %q", g.Skipped, pattern, "no files matched")
	}
}

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
	if err := run([]string{"rulegraph", "--rules", ruleFile, "--corpus", corpusFile}, &out, &errOut); err != nil {
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
	if err := run([]string{"rulegraph", "--rules", ruleFile, "--corpus", corpusFile, "--json"}, &out, &errOut); err != nil {
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
	if err := run([]string{"rulegraph"}, &out, &errOut); err == nil {
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
	if err := run([]string{"rulegraph", "--rules", ruleFile, "--out", graphFile}, &out, &errOut); err != nil {
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

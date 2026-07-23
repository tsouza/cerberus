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

// TestHarvestThreeHeadedCorpus pins the three-headed harvest end to end through
// the real `harvest` flags: a dashboard with one Prometheus panel (PromQL), one
// Loki panel (LogQL), one Tempo panel (TraceQL, read from the `query` field), and
// one unknown-datasource panel that is counted as a skip — plus a `--loki-rules`
// file that harvests as LogQL. The corpus carries all three languages with the
// right provenance, and the single unusable panel is counted, never dropped.
func TestHarvestThreeHeadedCorpus(t *testing.T) {
	dir := t.TempDir()

	lokiRulesFile := filepath.Join(dir, "loki-rules.yml")
	const lokiRules = `
groups:
  - name: logs
    rules:
      - record: job:errors:rate5m
        expr: sum(rate({app="svc"} |= "error" [5m]))
`
	if err := os.WriteFile(lokiRulesFile, []byte(lokiRules), 0o600); err != nil {
		t.Fatal(err)
	}

	dashDir := filepath.Join(dir, "dash")
	if err := os.MkdirAll(dashDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// One panel per head plus an unknown (Elasticsearch) datasource. The Tempo
	// panel deliberately carries its TraceQL in `query`, not `expr`.
	const dashboard = `{
  "title": "svc",
  "panels": [
    {"id": 1, "title": "reqs", "datasource": {"type": "prometheus"},
     "targets": [{"refId": "A", "expr": "sum(rate(http_requests_total[5m]))"}]},
    {"id": 2, "title": "logs", "datasource": {"type": "loki"},
     "targets": [{"refId": "A", "expr": "{app=\"svc\"} |= \"error\""}]},
    {"id": 3, "title": "traces", "datasource": {"type": "tempo"},
     "targets": [{"refId": "A", "query": "{ span.http.status_code >= 500 }"}]},
    {"id": 4, "title": "docs", "datasource": {"type": "elasticsearch"},
     "targets": [{"refId": "A", "expr": "whatever"}]}
  ]
}`
	if err := os.WriteFile(filepath.Join(dashDir, "board.json"), []byte(dashboard), 0o600); err != nil {
		t.Fatal(err)
	}

	corpusFile := filepath.Join(dir, "corpus.json")
	var out, errOut bytes.Buffer
	if err := runMigrate([]string{
		"harvest",
		"--loki-rules", lokiRulesFile,
		"--dashboards", dashDir,
		"--out", corpusFile,
	}, &out, &errOut); err != nil {
		t.Fatalf("harvest: %v (stderr: %s)", err, errOut.String())
	}

	data, err := os.ReadFile(corpusFile) //nolint:gosec // test-controlled temp path.
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var corpus migrate.Corpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatalf("corpus is not valid JSON: %v\n%s", err, data)
	}

	// Three heads harvested: one LogQL rule + one PromQL panel + one LogQL panel +
	// one TraceQL panel = 4 queries.
	if len(corpus.Queries) != 4 {
		t.Fatalf("corpus queries = %d, want 4: %+v", len(corpus.Queries), corpus.Queries)
	}

	// Provenance: each language present with the right expr and kind.
	byLang := map[string]migrate.CorpusQuery{}
	for _, q := range corpus.Queries {
		byLang[q.Lang] = q
	}
	if q := byLang[migrate.LangPromQL]; q.Kind != migrate.KindPanel ||
		!strings.Contains(q.Expr, "http_requests_total") {
		t.Errorf("PromQL provenance wrong: %+v", q)
	}
	if q := byLang[migrate.LangTraceQL]; q.Kind != migrate.KindPanel ||
		q.Expr != "{ span.http.status_code >= 500 }" {
		t.Errorf("TraceQL provenance wrong (must read the panel `query` field): %+v", q)
	}
	// LogQL appears twice (rule + panel); assert both provenances exist.
	var sawLogQLRule, sawLogQLPanel bool
	for _, q := range corpus.Queries {
		if q.Lang != migrate.LangLogQL {
			continue
		}
		switch q.Kind {
		case migrate.KindRecord:
			sawLogQLRule = strings.Contains(q.Source, "job:errors:rate5m")
		case migrate.KindPanel:
			sawLogQLPanel = strings.Contains(q.Expr, `|= "error"`)
		}
	}
	if !sawLogQLRule {
		t.Errorf("expected a LogQL recording rule from --loki-rules, got: %+v", corpus.Queries)
	}
	if !sawLogQLPanel {
		t.Errorf("expected a LogQL dashboard panel, got: %+v", corpus.Queries)
	}

	// The Elasticsearch panel is the one unusable target: counted, never dropped.
	if len(corpus.Skipped) != 1 {
		t.Fatalf("expected exactly 1 skip (the unknown datasource), got %+v", corpus.Skipped)
	}
	if !strings.Contains(corpus.Skipped[0].Reason, "elasticsearch") ||
		!strings.Contains(corpus.Skipped[0].Reason, "unsupported datasource type") {
		t.Errorf("unknown-datasource skip should name the type honestly, got: %+v", corpus.Skipped[0])
	}

	// Deterministic: a second harvest to a fresh file is byte-identical.
	corpusFile2 := filepath.Join(dir, "corpus2.json")
	var out2, errOut2 bytes.Buffer
	if err := runMigrate([]string{
		"harvest",
		"--loki-rules", lokiRulesFile,
		"--dashboards", dashDir,
		"--out", corpusFile2,
	}, &out2, &errOut2); err != nil {
		t.Fatalf("harvest (2): %v (stderr: %s)", err, errOut2.String())
	}
	data2, err := os.ReadFile(corpusFile2) //nolint:gosec // test-controlled temp path.
	if err != nil {
		t.Fatalf("read corpus2: %v", err)
	}
	if !bytes.Equal(data, data2) {
		t.Errorf("three-headed harvest is not deterministic:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", data, data2)
	}
}

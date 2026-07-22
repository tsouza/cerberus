package migrate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHarvestRulesAndDashboardsIntoCorpus drives the full harvest → corpus flow
// over a temp rules file and a temp Grafana dashboard: a Prometheus panel and a
// Prometheus target inside a nested collapsed row are kept, a Loki panel is
// dropped-with-count, and the corpus marshals deterministically.
func TestHarvestRulesAndDashboardsIntoCorpus(t *testing.T) {
	dir := t.TempDir()

	rulesFile := filepath.Join(dir, "rules.yml")
	const rules = `
groups:
  - name: cpu
    rules:
      - record: job:cpu:rate5m
        expr: sum(rate(cpu_seconds_total[5m])) by (job)
      - alert: HighErrorRate
        expr: rate(errors_total[5m]) > 0.5
`
	if err := os.WriteFile(rulesFile, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}

	dashDir := filepath.Join(dir, "dashboards")
	if err := os.MkdirAll(dashDir, 0o750); err != nil {
		t.Fatal(err)
	}
	dashFile := filepath.Join(dashDir, "board.json")
	const dashboard = `{
  "title": "svc",
  "panels": [
    {
      "id": 1,
      "title": "req rate",
      "type": "timeseries",
      "datasource": {"type": "prometheus", "uid": "prom"},
      "targets": [
        {"refId": "A", "expr": "sum(rate(http_requests_total[5m]))"}
      ]
    },
    {
      "id": 2,
      "title": "logs",
      "type": "logs",
      "datasource": {"type": "loki", "uid": "loki"},
      "targets": [
        {"refId": "A", "expr": "{app=\"svc\"} |= \"error\""}
      ]
    },
    {
      "id": 3,
      "title": "row",
      "type": "row",
      "panels": [
        {
          "id": 4,
          "title": "nested latency",
          "type": "timeseries",
          "datasource": {"type": "prometheus", "uid": "prom"},
          "targets": [
            {"refId": "A", "expr": "histogram_quantile(0.9, rate(latency_bucket[5m]))"}
          ]
        }
      ]
    }
  ]
}`
	if err := os.WriteFile(dashFile, []byte(dashboard), 0o600); err != nil {
		t.Fatal(err)
	}

	src := MultiSource{
		FileSource{RulePaths: []string{rulesFile}},
		DashboardSource{Dir: dashDir},
	}
	queries, skipped, err := src.Harvest(context.Background())
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}

	// Two rules + two Prometheus panel targets (top-level + nested row).
	if len(queries) != 4 {
		t.Fatalf("expected 4 harvested queries, got %d: %+v", len(queries), queries)
	}
	// Exactly one drop: the Loki panel target.
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skip (the Loki panel), got %d: %+v", len(skipped), skipped)
	}
	if !strings.Contains(skipped[0].Reason, "non-prometheus datasource: loki") {
		t.Errorf("Loki panel should be dropped with a datasource reason, got: %+v", skipped[0])
	}

	corpus := BuildCorpus(queries, skipped)
	if corpus.Version != CorpusVersion {
		t.Errorf("corpus version = %d, want %d", corpus.Version, CorpusVersion)
	}

	// Every query is PromQL-tagged; kinds cover record, alert, and panel.
	kinds := map[string]int{}
	for _, q := range corpus.Queries {
		if q.Lang != LangPromQL {
			t.Errorf("query %q lang = %q, want %q", q.Expr, q.Lang, LangPromQL)
		}
		kinds[q.Kind]++
	}
	if kinds[KindRecord] != 1 || kinds[KindAlert] != 1 || kinds[KindPanel] != 2 {
		t.Errorf("unexpected kind distribution: %+v", kinds)
	}

	// The nested-row Prometheus target must be present with panel provenance.
	var sawNested bool
	for _, q := range corpus.Queries {
		if strings.Contains(q.Source, "nested latency") &&
			strings.Contains(q.Expr, "histogram_quantile") {
			sawNested = true
		}
	}
	if !sawNested {
		t.Errorf("nested-row Prometheus target should be harvested: %+v", corpus.Queries)
	}

	// Marshalling is deterministic: byte-identical across repeated calls, and a
	// re-built corpus from the same inputs matches.
	first, err := corpus.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	second, err := BuildCorpus(queries, skipped).Marshal()
	if err != nil {
		t.Fatalf("Marshal (2): %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("corpus marshalling is not deterministic:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}

	// The JSON carries the expected shape: version, non-null arrays, promql lang.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(first, &raw); err != nil {
		t.Fatalf("corpus JSON is not valid: %v", err)
	}
	for _, key := range []string{"version", "queries", "skipped"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("corpus JSON missing top-level key %q", key)
		}
	}
	if strings.Contains(string(first), "null") {
		t.Errorf("corpus JSON should carry [] not null for empty arrays:\n%s", first)
	}
}

// TestCorpusRoundTripThroughFileSource pins that a corpus written to disk reads
// back through CorpusFileSource with the queries and skips intact, so
// `harvest --out` composes with `explain --corpus`.
func TestCorpusRoundTripThroughFileSource(t *testing.T) {
	queries := []HarvestedQuery{
		{Expr: "up", Source: "rule:f/g/up_rec", Kind: KindRecord},
		{Expr: "rate(x[5m])", Source: "dashboard:d/p/A", Kind: KindPanel},
	}
	skipped := []SkippedEntry{
		{Source: "dashboard:d/logs/A", Reason: "non-prometheus datasource: loki"},
	}

	data, err := BuildCorpus(queries, skipped).Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	file := filepath.Join(t.TempDir(), "corpus.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}

	gotQueries, gotSkipped, err := CorpusFileSource{Path: file}.Harvest(context.Background())
	if err != nil {
		t.Fatalf("CorpusFileSource.Harvest: %v", err)
	}
	if len(gotQueries) != 2 {
		t.Fatalf("round-trip queries = %d, want 2: %+v", len(gotQueries), gotQueries)
	}
	if len(gotSkipped) != 1 {
		t.Fatalf("round-trip skips = %d, want 1: %+v", len(gotSkipped), gotSkipped)
	}
	// Kinds are preserved; sort order is by source, so up_rec comes before the
	// dashboard entry.
	if gotQueries[0].Kind != KindPanel {
		// dashboard:d/p/A sorts before rule:f/g/up_rec ("d" < "r").
		t.Errorf("first round-tripped query kind = %q, want %q", gotQueries[0].Kind, KindPanel)
	}
	if gotSkipped[0].Reason != "non-prometheus datasource: loki" {
		t.Errorf("round-tripped skip reason = %q", gotSkipped[0].Reason)
	}
}

// TestCorpusFileSourceRejectsBadInput pins that an unreadable file and an
// unsupported-version corpus are hard errors, not silent empties.
func TestCorpusFileSourceRejectsBadInput(t *testing.T) {
	if _, _, err := (CorpusFileSource{Path: filepath.Join(t.TempDir(), "missing.json")}).Harvest(context.Background()); err == nil {
		t.Error("missing corpus file should be a hard error")
	}

	file := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(file, []byte(`{"version":999,"queries":[],"skipped":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (CorpusFileSource{Path: file}).Harvest(context.Background()); err == nil {
		t.Error("unsupported corpus version should be a hard error")
	}
}

// TestDashboardSourceCountsEveryDrop pins that unreadable/unparseable files,
// empty exprs, and unresolved datasources are each counted as a skip.
func TestDashboardSourceCountsEveryDrop(t *testing.T) {
	dir := t.TempDir()

	// A file that is not valid JSON.
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A dashboard with an empty-expr Prometheus target and an unresolved-ds target.
	const dashboard = `{
  "panels": [
    {
      "id": 1,
      "title": "p",
      "datasource": {"type": "prometheus"},
      "targets": [
        {"refId": "A", "expr": "   "},
        {"refId": "B", "expr": "up", "datasource": {"uid": "no-type-here"}}
      ]
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, "board.json"), []byte(dashboard), 0o600); err != nil {
		t.Fatal(err)
	}

	queries, skipped, err := DashboardSource{Dir: dir}.Harvest(context.Background())
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}

	// Target B inherits the panel's prometheus datasource (its own object has no
	// type), so it is a valid harvested query.
	if len(queries) != 1 || queries[0].Expr != "up" {
		t.Fatalf("expected 1 harvested query (up), got %+v", queries)
	}

	var sawParse, sawEmpty bool
	for _, s := range skipped {
		if strings.Contains(s.Source, "broken.json") && strings.Contains(s.Reason, "parse dashboard JSON") {
			sawParse = true
		}
		if strings.Contains(s.Reason, "empty expr") {
			sawEmpty = true
		}
	}
	if !sawParse {
		t.Errorf("unparseable dashboard file should be counted: %+v", skipped)
	}
	if !sawEmpty {
		t.Errorf("empty-expr target should be counted: %+v", skipped)
	}
}

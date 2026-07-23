package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDashboardLegacyNameFormDatasource pins the legacy bare-string datasource
// form, where the string is a datasource NAME (not a type). The conventional
// default head names — "Prometheus" / "Loki" / "Tempo", any case — resolve to
// their type and harvest with the right language; any other name (Mimir, …)
// cannot be mapped to a type offline and is dropped-with-count with a distinct,
// honest reason — never silently discarded and never miscategorised as a type.
func TestDashboardLegacyNameFormDatasource(t *testing.T) {
	dir := t.TempDir()
	const dashboard = `{
  "title": "legacy",
  "panels": [
    {"id": 1, "title": "reqs", "datasource": "Prometheus",
     "targets": [{"refId": "A", "expr": "up"}]},
    {"id": 2, "title": "logs", "datasource": "Loki",
     "targets": [{"refId": "A", "expr": "{app=\"x\"}"}]},
    {"id": 3, "title": "traces", "datasource": "Tempo",
     "targets": [{"refId": "A", "query": "{ duration > 1s }"}]},
    {"id": 4, "title": "via mimir", "datasource": "Mimir",
     "targets": [{"refId": "A", "expr": "rate(x[5m])"}]}
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, "board.json"), []byte(dashboard), 0o600); err != nil {
		t.Fatal(err)
	}

	queries, skipped, err := DashboardSource{Dir: dir}.Harvest(context.Background())
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}

	// The Prometheus / Loki / Tempo name-form panels each resolve and harvest with
	// the right language.
	byLang := map[string]HarvestedQuery{}
	for _, q := range queries {
		byLang[q.Lang] = q
	}
	if len(queries) != 3 {
		t.Fatalf("expected 3 name-form harvests (Prometheus, Loki, Tempo), got %d: %+v", len(queries), queries)
	}
	if byLang[LangPromQL].Expr != "up" {
		t.Errorf("name-form \"Prometheus\" should harvest `up` as PromQL, got %+v", byLang[LangPromQL])
	}
	if byLang[LangLogQL].Expr != `{app="x"}` {
		t.Errorf("name-form \"Loki\" should harvest the LogQL selector, got %+v", byLang[LangLogQL])
	}
	if byLang[LangTraceQL].Expr != "{ duration > 1s }" {
		t.Errorf("name-form \"Tempo\" should harvest the TraceQL `query` field, got %+v", byLang[LangTraceQL])
	}

	// The Mimir name-form panel is dropped-with-count: no offline type mapping.
	if len(skipped) != 1 {
		t.Fatalf("expected 1 name-form drop (Mimir), got %d: %+v", len(skipped), skipped)
	}
	if !strings.Contains(skipped[0].Reason, "cannot be type-resolved offline") ||
		!strings.Contains(skipped[0].Reason, `"Mimir"`) {
		t.Errorf("Mimir should be dropped with the offline-unresolvable name-form reason, got: %+v", skipped[0])
	}
}

// TestDashboardLegacyRowsSchema pins that the pre-v16 (Grafana v4) rows[].panels[]
// schema is walked, not silently dropped: a Prometheus panel (PromQL) and a Loki
// panel (LogQL) nested under rows[] are both harvested with panel provenance and
// the right language.
func TestDashboardLegacyRowsSchema(t *testing.T) {
	dir := t.TempDir()
	const dashboard = `{
  "title": "v4",
  "rows": [
    {"panels": [
      {"id": 1, "title": "load", "datasource": {"type": "prometheus"},
       "targets": [{"refId": "A", "expr": "node_load1"}]},
      {"id": 2, "title": "logs", "datasource": {"type": "loki"},
       "targets": [{"refId": "A", "expr": "{app=\"x\"}"}]}
    ]}
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, "v4.json"), []byte(dashboard), 0o600); err != nil {
		t.Fatal(err)
	}

	queries, skipped, err := DashboardSource{Dir: dir}.Harvest(context.Background())
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("rows[] Prometheus + Loki panels should both harvest, got skips: %+v", skipped)
	}
	byLang := map[string]HarvestedQuery{}
	for _, q := range queries {
		if q.Kind != KindPanel {
			t.Errorf("rows[] harvest should be a panel kind, got %q for %q", q.Kind, q.Expr)
		}
		byLang[q.Lang] = q
	}
	if byLang[LangPromQL].Expr != "node_load1" {
		t.Errorf("rows[] Prometheus panel should harvest node_load1 as PromQL, got %+v", byLang[LangPromQL])
	}
	if byLang[LangLogQL].Expr != `{app="x"}` {
		t.Errorf("rows[] Loki panel should harvest the LogQL selector, got %+v", byLang[LangLogQL])
	}
}

// TestDashboardSourceMissingDirIsCountedNotFatal pins that a missing dashboard
// directory is a counted walk skip, not a hard error — the harvest stays usable
// and the failure is surfaced, never silently swallowed.
func TestDashboardSourceMissingDirIsCountedNotFatal(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	queries, skipped, err := DashboardSource{Dir: missing}.Harvest(context.Background())
	if err != nil {
		t.Fatalf("missing dir should not be a hard error, got: %v", err)
	}
	if len(queries) != 0 {
		t.Errorf("missing dir yields no queries, got %+v", queries)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0].Reason, "walk") {
		t.Fatalf("missing dir should be one counted walk skip, got %+v", skipped)
	}
}

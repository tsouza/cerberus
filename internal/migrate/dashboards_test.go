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
// default "Prometheus" name (any case) is resolved to the prometheus type and
// harvested; any other name (Loki, Mimir, …) cannot be mapped to a type offline
// and is dropped-with-count with a distinct, honest reason — never silently
// discarded and never miscategorised as a type.
func TestDashboardLegacyNameFormDatasource(t *testing.T) {
	dir := t.TempDir()
	const dashboard = `{
  "title": "legacy",
  "panels": [
    {"id": 1, "title": "reqs", "datasource": "Prometheus",
     "targets": [{"refId": "A", "expr": "up"}]},
    {"id": 2, "title": "logs", "datasource": "Loki",
     "targets": [{"refId": "A", "expr": "{app=\"x\"}"}]},
    {"id": 3, "title": "via mimir", "datasource": "Mimir",
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

	// The "Prometheus"-named panel resolves to the prometheus type and is kept.
	if len(queries) != 1 || queries[0].Expr != "up" {
		t.Fatalf("name-form \"Prometheus\" should resolve and harvest `up`, got %+v", queries)
	}

	// Loki + Mimir name-form panels are each dropped with a name-form reason.
	if len(skipped) != 2 {
		t.Fatalf("expected 2 name-form drops (Loki, Mimir), got %d: %+v", len(skipped), skipped)
	}
	var sawLoki, sawMimir bool
	for _, s := range skipped {
		if !strings.Contains(s.Reason, "cannot be type-resolved offline") {
			t.Errorf("name-form drop should carry the offline-unresolvable reason, got: %+v", s)
		}
		if strings.Contains(s.Reason, `"Loki"`) {
			sawLoki = true
		}
		if strings.Contains(s.Reason, `"Mimir"`) {
			sawMimir = true
		}
	}
	if !sawLoki || !sawMimir {
		t.Errorf("both name-form datasources should be named in the drop reasons: %+v", skipped)
	}
}

// TestDashboardLegacyRowsSchema pins that the pre-v16 (Grafana v4) rows[].panels[]
// schema is walked, not silently dropped: a Prometheus panel nested under rows[]
// is harvested with panel provenance.
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
	if len(queries) != 1 || queries[0].Expr != "node_load1" {
		t.Fatalf("rows[] Prometheus panel should be harvested, got %+v", queries)
	}
	if queries[0].Kind != KindPanel {
		t.Errorf("rows[] harvest should be a panel kind, got %q", queries[0].Kind)
	}
	// The Loki panel under the same row is dropped-with-count.
	if len(skipped) != 1 || !strings.Contains(skipped[0].Reason, "non-prometheus datasource: loki") {
		t.Fatalf("rows[] Loki panel should be dropped-with-count, got %+v", skipped)
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

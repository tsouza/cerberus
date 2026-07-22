package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// datasourceTypePrometheus is the Grafana datasource `type` that marks a panel
// target as PromQL. Only targets resolving to this type are harvested; targets
// on any other datasource (Loki, Tempo, mixed, …) are dropped with a count.
const datasourceTypePrometheus = "prometheus"

// dashboardFileExt is the extension of an exported Grafana dashboard file. The
// dashboard source walks a directory tree and considers only these files.
const dashboardFileExt = ".json"

// DashboardSource harvests PromQL from a directory tree of exported Grafana
// dashboard JSON files. It walks every panel's targets (including targets on
// panels nested inside collapsed rows, panels[].panels[]) and keeps only the
// targets whose datasource is Prometheus-typed. Everything else — an unreadable
// or unparseable file, a target with an empty expression, or a target on a
// non-Prometheus datasource — is recorded as a counted SkippedEntry, never
// silently dropped.
type DashboardSource struct {
	Dir string
}

// dashboardDoc is the minimal slice of the Grafana dashboard schema the harvest
// needs: a title (for provenance) and a tree of panels.
type dashboardDoc struct {
	Title  string           `json:"title"`
	Panels []dashboardPanel `json:"panels"`
}

// dashboardPanel is one panel. It may carry its own targets, and — when it is a
// collapsed row — a nested list of child panels. `datasource` is the panel-level
// default a target inherits when it does not name its own.
type dashboardPanel struct {
	ID         int               `json:"id"`
	Title      string            `json:"title"`
	Type       string            `json:"type"`
	Datasource json.RawMessage   `json:"datasource"`
	Targets    []dashboardTarget `json:"targets"`
	Panels     []dashboardPanel  `json:"panels"`
}

// dashboardTarget is one query target on a panel. `datasource` overrides the
// panel default when present.
type dashboardTarget struct {
	RefID      string          `json:"refId"`
	Expr       string          `json:"expr"`
	Datasource json.RawMessage `json:"datasource"`
}

// Harvest walks the dashboard directory, decoding each JSON file and flattening
// its Prometheus panel targets into HarvestedQuery entries. It never returns a
// per-item hard error: a missing directory, an unreadable file, a parse
// failure, an empty expr, or a non-Prometheus target all become counted skips.
func (s DashboardSource) Harvest(_ context.Context) ([]HarvestedQuery, []SkippedEntry, error) {
	var (
		queries []HarvestedQuery
		skipped []SkippedEntry
	)
	walkErr := filepath.WalkDir(s.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			skipped = append(skipped, SkippedEntry{Source: path, Reason: fmt.Sprintf("walk: %v", err)})
			return nil
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(path), dashboardFileExt) {
			return nil
		}
		q, sk := harvestDashboardFile(path)
		queries = append(queries, q...)
		skipped = append(skipped, sk...)
		return nil
	})
	if walkErr != nil {
		return nil, nil, fmt.Errorf("migrate: walk dashboards %q: %w", s.Dir, walkErr)
	}
	return queries, skipped, nil
}

// harvestDashboardFile reads and decodes one dashboard file and flattens its
// panel tree. A read or parse failure yields a single counted skip for the file.
func harvestDashboardFile(file string) ([]HarvestedQuery, []SkippedEntry) {
	data, err := os.ReadFile(file) //nolint:gosec // operator-supplied dashboard path; offline CLI.
	if err != nil {
		return nil, []SkippedEntry{{Source: file, Reason: fmt.Sprintf("read: %v", err)}}
	}
	var doc dashboardDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, []SkippedEntry{{Source: file, Reason: fmt.Sprintf("parse dashboard JSON: %v", err)}}
	}
	var (
		queries []HarvestedQuery
		skipped []SkippedEntry
	)
	// A panel's default datasource is empty at the top level; each panel's own
	// datasource becomes the default its targets inherit.
	walkPanels(file, doc.Panels, "", &queries, &skipped)
	return queries, skipped
}

// walkPanels recursively flattens a panel list, threading each panel's
// datasource down as the inherited default for its targets and its nested
// (collapsed-row) child panels.
func walkPanels(file string, panels []dashboardPanel, inherited string, queries *[]HarvestedQuery, skipped *[]SkippedEntry) {
	for _, p := range panels {
		panelDS := resolveDatasourceType(p.Datasource)
		if panelDS == "" {
			panelDS = inherited
		}
		panelKey := panelIdent(p)
		for i, t := range p.Targets {
			source := fmt.Sprintf("dashboard:%s/%s/%s", file, panelKey, targetIdent(t, i))
			dsType := resolveDatasourceType(t.Datasource)
			if dsType == "" {
				dsType = panelDS
			}
			if dsType != datasourceTypePrometheus {
				*skipped = append(*skipped, SkippedEntry{Source: source, Reason: nonPromReason(dsType)})
				continue
			}
			if strings.TrimSpace(t.Expr) == "" {
				*skipped = append(*skipped, SkippedEntry{Source: source, Reason: "target has an empty expr"})
				continue
			}
			*queries = append(*queries, HarvestedQuery{Expr: t.Expr, Source: source, Kind: KindPanel})
		}
		// Recurse into collapsed-row child panels, inheriting this panel's ds.
		walkPanels(file, p.Panels, panelDS, queries, skipped)
	}
}

// nonPromReason describes why a target was dropped for its datasource: either it
// names a concrete non-Prometheus type, or its datasource could not be resolved
// to any type at all.
func nonPromReason(dsType string) string {
	if dsType == "" {
		return "target datasource is unresolved (not Prometheus-typed)"
	}
	return fmt.Sprintf("non-prometheus datasource: %s", dsType)
}

// resolveDatasourceType extracts the datasource type from a Grafana `datasource`
// field, which may be an object ({"type":"prometheus","uid":...}) or a bare
// string (the legacy form, where the string itself is the type token). An
// absent, null, or shapeless datasource resolves to "".
func resolveDatasourceType(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var obj struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Type != "" {
		return obj.Type
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return strings.TrimSpace(str)
	}
	return ""
}

// panelIdent names a panel for provenance: its title when set, else panel-<id>.
func panelIdent(p dashboardPanel) string {
	if title := strings.TrimSpace(p.Title); title != "" {
		return title
	}
	return fmt.Sprintf("panel-%d", p.ID)
}

// targetIdent names a target for provenance: its refId when set, else
// target-<index>.
func targetIdent(t dashboardTarget, index int) string {
	if ref := strings.TrimSpace(t.RefID); ref != "" {
		return ref
	}
	return fmt.Sprintf("target-%d", index)
}

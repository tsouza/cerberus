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

// The Grafana datasource `type` strings for the three gateway heads. Each maps
// to the query language its panel targets are written in — Prometheus→PromQL,
// Loki→LogQL, Tempo→TraceQL. A target on any other datasource type (a table/logs
// plugin, an unknown backend, a mixed source) is dropped-with-count.
const (
	datasourceTypePrometheus = "prometheus"
	datasourceTypeLoki       = "loki"
	datasourceTypeTempo      = "tempo"
)

// dashboardFileExt is the extension of an exported Grafana dashboard file. The
// dashboard source walks a directory tree and considers only these files.
const dashboardFileExt = ".json"

// langForDatasourceType maps a resolved Grafana datasource `type` to the query
// language its panel targets carry. The three heads — Prometheus (PromQL), Loki
// (LogQL), Tempo (TraceQL) — are the only types the harvest keeps; any other type
// yields ok=false and the target is dropped-with-count.
func langForDatasourceType(typ string) (lang string, ok bool) {
	switch typ {
	case datasourceTypePrometheus:
		return LangPromQL, true
	case datasourceTypeLoki:
		return LangLogQL, true
	case datasourceTypeTempo:
		return LangTraceQL, true
	default:
		return "", false
	}
}

// DashboardSource harvests queries from a directory tree of exported Grafana
// dashboard JSON files across all three heads. It walks every panel's targets —
// targets on top-level panels[], targets on panels nested inside collapsed rows
// (panels[].panels[]), and targets on panels under the legacy pre-v16
// rows[].panels[] schema — and keeps every target whose datasource resolves to a
// Prometheus (PromQL), Loki (LogQL), or Tempo (TraceQL) type, tagged with its
// language. Everything else — an unreadable or unparseable file, a target with an
// empty expression, a target on an unsupported datasource type, or a legacy
// name-form datasource that cannot be type-resolved offline — is recorded as a
// counted SkippedEntry, never silently dropped.
type DashboardSource struct {
	Dir string
}

// dashboardDoc is the minimal slice of the Grafana dashboard schema the harvest
// needs: a title (for provenance) and the panel tree. Modern exports carry a
// flat panels[]; the legacy pre-v16 (Grafana v4) schema nests panels under
// rows[].panels[] instead, so both are decoded and walked.
type dashboardDoc struct {
	Title  string           `json:"title"`
	Panels []dashboardPanel `json:"panels"`
	Rows   []dashboardRow   `json:"rows"`
}

// dashboardRow is one row of the legacy pre-v16 dashboard schema, where panels
// live under rows[].panels[] rather than a flat top-level panels[].
type dashboardRow struct {
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
// panel default when present. Prometheus and Loki targets carry their query in
// `expr`; Tempo targets carry TraceQL in `query` instead — both are decoded and
// the right one is read per the resolved datasource language.
type dashboardTarget struct {
	RefID      string          `json:"refId"`
	Expr       string          `json:"expr"`
	Query      string          `json:"query"`
	Datasource json.RawMessage `json:"datasource"`
}

// Harvest walks the dashboard directory, decoding each JSON file and flattening
// its Prometheus / Loki / Tempo panel targets into lang-tagged HarvestedQuery
// entries. It never returns a per-item hard error: a missing directory, an
// unreadable file, a parse failure, an empty expr/query, or an
// unsupported-datasource target all become counted skips.
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
	// datasource becomes the default its targets inherit. Both the modern flat
	// panels[] and the legacy rows[].panels[] trees are walked.
	walkPanels(file, doc.Panels, datasourceRef{}, &queries, &skipped)
	for _, row := range doc.Rows {
		walkPanels(file, row.Panels, datasourceRef{}, &queries, &skipped)
	}
	return queries, skipped
}

// walkPanels recursively flattens a panel list, threading each panel's
// datasource down as the inherited default for its targets and its nested
// (collapsed-row) child panels.
func walkPanels(file string, panels []dashboardPanel, inherited datasourceRef, queries *[]HarvestedQuery, skipped *[]SkippedEntry) {
	for _, p := range panels {
		panelDS := resolveDatasource(p.Datasource)
		if !panelDS.set() {
			panelDS = inherited
		}
		panelKey := panelIdent(p)
		for i, t := range p.Targets {
			source := fmt.Sprintf("dashboard:%s/%s/%s", file, panelKey, targetIdent(t, i))
			ds := resolveDatasource(t.Datasource)
			if !ds.set() {
				ds = panelDS
			}
			lang, ok := langForDatasourceType(ds.typ)
			if !ok {
				*skipped = append(*skipped, SkippedEntry{Source: source, Reason: ds.dropReason()})
				continue
			}
			// Tempo panels carry TraceQL in `query`; Prometheus/Loki panels carry
			// their query in `expr`. Read the field that matches the resolved head.
			expr, field := t.Expr, "expr"
			if lang == LangTraceQL {
				expr, field = t.Query, "query"
			}
			if strings.TrimSpace(expr) == "" {
				*skipped = append(*skipped, SkippedEntry{Source: source, Reason: "target has an empty " + field})
				continue
			}
			*queries = append(*queries, HarvestedQuery{Expr: expr, Source: source, Kind: KindPanel, Lang: lang})
		}
		// Recurse into collapsed-row child panels, inheriting this panel's ds.
		walkPanels(file, p.Panels, panelDS, queries, skipped)
	}
}

// datasourceRef is a resolved Grafana `datasource` reference. At most one of typ
// / name is populated; both empty means the field was absent and the datasource
// is inherited from the enclosing panel/row. The object form
// ({"type":"prometheus","uid":...}) yields a concrete typ. The legacy bare
// string form is a datasource NAME, not a type — a name that case-folds to one of
// the head type names ("prometheus" / "loki" / "tempo") is safely resolvable to
// that type offline; any other name (Mimir, Cortex, a custom name, a "${var}"
// template) cannot be mapped to a type without querying Grafana, so it is carried
// as name and dropped-with-count rather than silently miscategorised as a type.
type datasourceRef struct {
	typ  string
	name string
}

// set reports whether the field named a datasource at all; an unset ref inherits
// the enclosing panel/row default.
func (r datasourceRef) set() bool { return r.typ != "" || r.name != "" }

// dropReason explains why a target was dropped: a name-form datasource that
// cannot be type-resolved offline, a datasource that resolved to no type at all,
// or a concrete type that is none of the three supported heads.
func (r datasourceRef) dropReason() string {
	switch {
	case r.name != "":
		return fmt.Sprintf("legacy name-form datasource %q cannot be type-resolved offline", r.name)
	case r.typ == "":
		return "target datasource is unresolved (no prometheus/loki/tempo type)"
	default:
		return fmt.Sprintf("unsupported datasource type %q (not prometheus/loki/tempo)", r.typ)
	}
}

// resolveDatasource resolves a Grafana `datasource` field, which may be an object
// ({"type":"prometheus","uid":...}) or a legacy bare string (a datasource NAME).
// An absent, null, or shapeless datasource yields an unset ref (inherit).
func resolveDatasource(raw json.RawMessage) datasourceRef {
	if len(raw) == 0 || string(raw) == "null" {
		return datasourceRef{}
	}
	var obj struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Type != "" {
		return datasourceRef{typ: obj.Type}
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		s := strings.TrimSpace(str)
		if s == "" {
			return datasourceRef{}
		}
		if typ, ok := resolveDatasourceName(s); ok {
			return datasourceRef{typ: typ}
		}
		return datasourceRef{name: s}
	}
	return datasourceRef{}
}

// resolveDatasourceName resolves a legacy bare-string datasource NAME to a head
// type when the name is one of the conventional default datasource names
// ("Prometheus" / "Loki" / "Tempo", any case) — the only names safely mapped to a
// type offline. Any other name needs a live Grafana lookup, so it is left
// unresolved and dropped-with-count.
func resolveDatasourceName(name string) (typ string, ok bool) {
	switch strings.ToLower(name) {
	case datasourceTypePrometheus:
		return datasourceTypePrometheus, true
	case datasourceTypeLoki:
		return datasourceTypeLoki, true
	case datasourceTypeTempo:
		return datasourceTypeTempo, true
	default:
		return "", false
	}
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

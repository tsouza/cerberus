// Package preflight implements cerberus's boot-time requirements check.
//
// Cerberus assumes (a) a ClickHouse server new enough for the SQL it
// emits and (b) a target schema with the OTel-CH default shape. Neither
// was historically validated at boot, so a too-old server or a divergent
// schema only surfaced later as an opaque query-time error. The preflight
// closes that gap with two gates run after the schema-create step:
//
//   - Gate 1 (version): the connected server's version() is compared
//     against a config-derived minimum — the base supported floor, raised
//     to the native-rate floor when the experimental native-rate path is
//     enabled (computed as max-of-applicable-minimums so future knobs can
//     raise it).
//   - Gate 2 (schema shape): the configured tables are introspected via
//     system.columns and validated to carry the essential columns the
//     emitters require, with the attribute-map columns typed
//     Map(String, String).
//
// Both gates are validated against the active, override-resolved config —
// never hardcoded names. Failures are aggregated: Run reports EVERY unmet
// requirement, not just the first, so an operator fixes the deployment in
// one pass.
package preflight

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// chVersion is a major.minor ClickHouse version. Patch and build suffixes
// are intentionally dropped: cerberus's SQL-surface requirements track
// feature availability, which lands at minor-version granularity, so the
// comparison is over (major, minor) only.
type chVersion struct {
	Major int
	Minor int
}

// String renders the version as "<major>.<minor>" for the diagnostic
// messages (the floor cerberus requires is always a bare major.minor).
func (v chVersion) String() string { return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) }

// atLeast reports whether v is greater than or equal to min, comparing
// major first and minor as the tie-break.
func (v chVersion) atLeast(min chVersion) bool {
	if v.Major != min.Major {
		return v.Major > min.Major
	}
	return v.Minor >= min.Minor
}

// minCHBase is the supported / CI-tested ClickHouse floor — the version
// README and docs state as the minimum. The chDB test substrate is
// 25.8.2.1-lts and the compose / e2e / compatibility lanes run 25.8, so
// the SQL cerberus emits is exercised against this floor.
var minCHBase = chVersion{Major: 25, Minor: 8}

// minCHNativeRate is the ClickHouse floor the native timeSeriesRateToGrid
// family was introduced at (v25.6.0). It only applies when the
// experimental native-rate path is enabled; the effective requirement is
// max(minCHBase, minCHNativeRate) so enabling a feature whose floor sits
// above the base raises the requirement, and the base wins otherwise.
var minCHNativeRate = chVersion{Major: 25, Minor: 6}

// attrMapType is the ClickHouse type the OTel-CH attribute-map columns
// (Attributes / ResourceAttributes / ScopeAttributes) must carry. Stored
// canonical (no spaces); the deployed type read from system.columns is
// normalised the same way before comparison.
const attrMapType = "Map(String,String)"

// Requirements is the active, override-resolved configuration the
// preflight validates against. Every name comes from the resolved schema
// structs, so CERBERUS_SCHEMA_* overrides are respected automatically.
type Requirements struct {
	// Database is the configured ClickHouse database the tables live in
	// (CERBERUS_CH_DATABASE). system.columns is filtered by it.
	Database string

	// NativeRateEnabled mirrors CERBERUS_EXPERIMENTAL_TS_GRID_RANGE. When
	// true the version floor is raised to max(base, native-rate floor).
	NativeRateEnabled bool

	// Metrics / Logs / Traces are the active schema shapes (defaults with
	// CERBERUS_SCHEMA_* overrides applied). The schema gate validates the
	// essential columns of each.
	Metrics schema.Metrics
	Logs    schema.Logs
	Traces  schema.Traces
}

// minVersion computes the effective version floor: the base minimum,
// raised to the native-rate floor when that path is enabled. Encoded as
// max-of-applicable-minimums (not a baked single number) so a future knob
// whose floor exceeds the base raises the requirement generically.
func (r Requirements) minVersion() chVersion {
	min := minCHBase
	if r.NativeRateEnabled {
		if minCHNativeRate.atLeast(min) {
			min = minCHNativeRate
		}
	}
	return min
}

// Querier is the narrow ClickHouse read surface the preflight needs:
// a single-string scalar query (for version()) and a (name, type)
// pair query (for system.columns). *chclient.Client satisfies it; tests
// supply a stub so no live ClickHouse is required.
type Querier interface {
	QueryStrings(ctx context.Context, sql string, args ...any) ([]string, error)
	QueryNameTypePairs(ctx context.Context, sql string, args ...any) ([]chclient.NameTypePair, error)
}

// RunIfEnabled is the ON/OFF gate around Run. When enabled is false both
// gates are bypassed entirely and q is never touched (the caller logs the
// disabled line); when true it delegates to Run. Keeping the knob check
// here makes the disabled path unit-testable without standing up cmd wiring.
func RunIfEnabled(ctx context.Context, enabled bool, q Querier, req Requirements) error {
	if !enabled {
		return nil
	}
	return Run(ctx, q, req)
}

// Run executes both gates against q and returns an aggregated error
// listing every unmet requirement, or nil when all requirements hold.
// The caller is responsible for turning a non-nil error into a non-zero
// process exit. Use RunIfEnabled for the CERBERUS_STARTUP_PREFLIGHT gate.
func Run(ctx context.Context, q Querier, req Requirements) error {
	var problems []string

	problems = append(problems, checkVersion(ctx, q, req)...)
	problems = append(problems, checkSchema(ctx, q, req)...)

	if len(problems) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("startup preflight failed:")
	for _, p := range problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return fmt.Errorf("%s", b.String())
}

// checkVersion runs gate 1: it reads version() and compares the parsed
// major.minor against the effective floor. An unreadable or unparseable
// version is itself a failure (never a silent pass).
func checkVersion(ctx context.Context, q Querier, req Requirements) []string {
	min := req.minVersion()
	rateNote := "native rate disabled"
	if req.NativeRateEnabled {
		rateNote = "native rate enabled"
	}

	sql, args := chsql.NewQuery().Select(chsql.Call("version")).Build()
	rows, err := q.QueryStrings(ctx, sql, args...)
	if err != nil {
		return []string{fmt.Sprintf("could not read clickhouse version: %v", err)}
	}
	if len(rows) == 0 {
		return []string{"clickhouse version query returned no rows"}
	}
	raw := strings.TrimSpace(rows[0])
	got, ok := parseCHVersion(raw)
	if !ok {
		return []string{fmt.Sprintf("clickhouse version %q is unparseable; required minimum %s (%s)", raw, min, rateNote)}
	}
	if !got.atLeast(min) {
		return []string{fmt.Sprintf("clickhouse version %s is below the required minimum %s (%s)", raw, min, rateNote)}
	}
	return nil
}

// parseCHVersion extracts the leading major.minor from a ClickHouse
// version string. The wire format looks like "25.8.2.1", "25.8.2.1-lts",
// or carries a build suffix; only the first two dot-separated integer
// fields are read, and any trailing non-digit run on the minor field
// (e.g. a "-lts" glued directly to it) is trimmed. Returns ok=false when
// the string has no leading integer major or minor field.
func parseCHVersion(s string) (chVersion, bool) {
	fields := strings.Split(strings.TrimSpace(s), ".")
	if len(fields) < 2 {
		return chVersion{}, false
	}
	major, ok := leadingInt(fields[0])
	if !ok {
		return chVersion{}, false
	}
	minor, ok := leadingInt(fields[1])
	if !ok {
		return chVersion{}, false
	}
	return chVersion{Major: major, Minor: minor}, true
}

// leadingInt parses the leading run of ASCII digits in s. Returns
// ok=false when s does not start with a digit, so a field like "lts" or
// an empty field is rejected rather than silently coerced to 0.
func leadingInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

// tableReq describes the shape required of one configured table: its
// resolved name, the essential columns that must exist, and the subset of
// those that must be typed Map(String, String). An empty Name disables
// the table (e.g. an override blanked it) — it contributes no checks.
type tableReq struct {
	name    string
	columns []string
	attrMap []string
}

// requiredTables resolves the active config into the per-table shape
// requirements. Only the columns the emitters actually read are listed —
// the essential keys (timestamp / value / identity columns) plus the
// attribute maps, validated against the override-resolved names.
func requiredTables(req Requirements) []tableReq {
	m := req.Metrics
	var tables []tableReq

	// Gauge + Sum share the plain-sample shape: name, timestamp, value,
	// service, and the two attribute maps. These are the columns the
	// PromQL emitter projects for a Sample.
	for _, t := range []string{m.GaugeTable, m.SumTable} {
		tables = append(tables, tableReq{
			name: t,
			columns: nonEmpty(
				m.MetricNameColumn, m.TimestampColumn, m.ValueColumn,
				m.ServiceNameColumn, m.AttributesColumn, m.ResourceAttributesColumn,
			),
			attrMap: nonEmpty(m.AttributesColumn, m.ResourceAttributesColumn, m.ScopeAttributesColumn),
		})
	}
	// Histogram + exp-histogram carry the decomposed observation columns
	// instead of a Value: Count / Sum live on the row keyed by the bare
	// metric name. The attribute maps still apply.
	for _, t := range []string{m.HistogramTable, m.ExpHistogramTable} {
		tables = append(tables, tableReq{
			name: t,
			columns: nonEmpty(
				m.MetricNameColumn, m.TimestampColumn, m.CountColumn, m.SumColumn,
				m.AttributesColumn, m.ResourceAttributesColumn,
			),
			attrMap: nonEmpty(m.AttributesColumn, m.ResourceAttributesColumn, m.ScopeAttributesColumn),
		})
	}

	l := req.Logs
	tables = append(tables, tableReq{
		name: l.LogsTable,
		columns: nonEmpty(
			l.TimestampColumn, l.BodyColumn, l.ServiceNameColumn,
			l.AttributesColumn, l.ResourceAttributesColumn,
		),
		attrMap: nonEmpty(l.AttributesColumn, l.ResourceAttributesColumn, l.ScopeAttributesColumn),
	})

	tr := req.Traces
	tables = append(tables, tableReq{
		name: tr.SpansTable,
		columns: nonEmpty(
			tr.TraceIDColumn, tr.SpanIDColumn, tr.SpanNameColumn, tr.ServiceNameColumn,
			tr.DurationColumn, tr.StartTimeColumn,
			tr.AttributesColumn, tr.ResourceAttributesColumn,
		),
		attrMap: nonEmpty(tr.AttributesColumn, tr.ResourceAttributesColumn, tr.ScopeAttributesColumn),
	})

	return tables
}

// nonEmpty returns the non-empty members of vals in order. Schema fields
// left blank (e.g. ScopeAttributes on traces, which the upstream template
// omits) are dropped so they never produce a spurious missing-column
// failure.
func nonEmpty(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// checkSchema runs gate 2: for each configured table it reads the
// deployed (name, type) columns from system.columns and validates that
// every essential column exists, with the attribute-map columns typed
// Map(String, String). Distinct tables that resolve to the same physical
// name (Gauge==Sum on a collapsed schema) are introspected once.
func checkSchema(ctx context.Context, q Querier, req Requirements) []string {
	seen := map[string]bool{}
	var problems []string
	for _, t := range requiredTables(req) {
		if t.name == "" || seen[t.name] {
			continue
		}
		seen[t.name] = true
		problems = append(problems, checkTable(ctx, q, req.Database, t)...)
	}
	return problems
}

// checkTable introspects one table via system.columns and validates its
// shape. A query error or a wholly-absent table is reported as a single
// failure; otherwise each missing column and each wrong attribute-map
// type is reported individually so the aggregated message is complete.
func checkTable(ctx context.Context, q Querier, database string, t tableReq) []string {
	sql, args := chsql.NewQuery().
		Select(chsql.Col("name"), chsql.Col("type")).
		From(chsql.Qual("system", "columns")).
		Where(
			chsql.Eq(chsql.Col("database"), chsql.Lit(database)),
			chsql.Eq(chsql.Col("table"), chsql.Lit(t.name)),
		).
		Build()
	rows, err := q.QueryNameTypePairs(ctx, sql, args...)
	if err != nil {
		return []string{fmt.Sprintf("could not introspect table %s: %v", t.name, err)}
	}
	if len(rows) == 0 {
		return []string{fmt.Sprintf("table %s: not found in database %s (no columns reported)", t.name, database)}
	}

	types := make(map[string]string, len(rows))
	for _, r := range rows {
		types[r.Name] = r.Type
	}

	var problems []string
	for _, col := range t.columns {
		if _, ok := types[col]; !ok {
			problems = append(problems, fmt.Sprintf("table %s: missing required column %s", t.name, col))
		}
	}
	for _, col := range t.attrMap {
		got, ok := types[col]
		if !ok {
			// The missing-column loop above already flagged it when the
			// column is one of the essential columns; attribute maps are
			// always in that set, so a type check on an absent column would
			// double-report. Skip — the missing-column message stands.
			continue
		}
		if normalizeType(got) != attrMapType {
			problems = append(problems, fmt.Sprintf(
				"table %s column %s: expected %s, found %s", t.name, col, attrMapType, got,
			))
		}
	}
	return problems
}

// normalizeType canonicalises a ClickHouse type string for comparison:
// inner whitespace is stripped so "Map(String, String)" (the spacing
// system.columns reports) matches the canonical attrMapType. A LowCardinality
// wrapper around the map's value type is unwrapped so the OTel-CH
// Map(String, LowCardinality(String)) variant some exporters emit is
// accepted as the expected map shape.
func normalizeType(s string) string {
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "LowCardinality(String)", "String")
	return s
}

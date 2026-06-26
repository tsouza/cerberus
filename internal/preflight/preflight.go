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
// never hardcoded names.
//
// # Fatal vs transient: the absent-schema race
//
// Cerberus is a drop-in query gateway frequently deployed ALONGSIDE its
// ingestion pipeline (an otel-collector that owns schema creation as
// telemetry first flows). On a cold cluster cerberus can legitimately boot
// BEFORE any table exists. Crash-looping in that window is wrong — it is a
// transient startup race, not a misconfiguration — so the preflight
// classifies its findings into two buckets:
//
//   - FATAL — a too-old / unreadable / unparseable server version, or a
//     table that EXISTS but whose SHAPE is wrong (a missing essential
//     column, an attribute map typed something other than
//     Map(String, String)). These never self-heal, so they fail startup
//     fast with an aggregated diff (every unmet requirement at once).
//   - TRANSIENT — any of (a) the configured tables are ENTIRELY ABSENT
//     (system.columns reports zero rows for them): the schema has not been
//     provisioned yet; (b) the configured DATABASE does not exist yet — the
//     connection carries it as the session default (Auth.Database), so even a
//     database-independent probe like SELECT version() fails with
//     UNKNOWN_DATABASE (code 81, "Database <name> does not exist") until an
//     external writer or the auto-create hook creates it; or (c) ClickHouse is
//     ENTIRELY UNREACHABLE at boot (the version / introspection probes fail with
//     a transport / dial / connection-refused error — cerberus started before
//     ClickHouse accepted connections). None is a misconfiguration: all heal on
//     their own. This does NOT fail startup; cerberus boots but reports NOT
//     READY on /readyz with a precise reason, and an external re-probe flips
//     it ready once the server appears (and the database + schema exist) — no
//     restart.
//
// Run returns a Result that carries the buckets separately. The caller
// turns the fatal bucket into a non-zero exit and feeds the absent /
// unreachable buckets into the readiness machinery.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// chopt.Version is the canonical major.minor ClickHouse version (parse +
// compare); preflight reuses it rather than re-implementing the gate, so the
// auto-picker and the preflight floor agree on what a server string means.
// Patch and build suffixes are dropped: cerberus's SQL-surface requirements
// track feature availability, which lands at minor-version granularity.

// minCHBase is the supported / CI-tested ClickHouse floor — the version
// README and docs state as the minimum. The differential compatibility
// suite (the source of truth for all three heads) runs ClickHouse 24.8
// and is green, so the SQL cerberus emits is exercised end-to-end against
// this floor; the 24.8 empty-input / parse-unit / filter-path workarounds
// are all emitted unconditionally.
var minCHBase = chopt.Version{Major: 24, Minor: 8}

// minCHNativeRate is the ClickHouse floor at which the native
// timeSeriesRateToGrid family is Prometheus-correct. The aggregates first
// shipped in v25.6.0 but used a CLOSED membership window until v25.9 (PR
// #86588 made it left-open / right-closed to match PromQL's range selector);
// below 25.9 the native path diverges from the fan-out on grid-aligned data,
// so 25.9 is the floor the native path requires. It only applies when the
// experimental native-rate path is enabled; the effective requirement is
// max(minCHBase, minCHNativeRate) so enabling a feature whose floor sits
// above the base raises the requirement, and the base wins otherwise.
var minCHNativeRate = chopt.Version{Major: 25, Minor: 9}

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
func (r Requirements) minVersion() chopt.Version {
	min := minCHBase
	if r.NativeRateEnabled {
		if minCHNativeRate.AtLeast(min) {
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

// Result is the outcome of a preflight pass, splitting findings by how the
// caller must react to them.
//
//   - Fatal is the aggregated, non-self-healing failure: a bad server
//     version or a wrong-shape table. When non-nil the caller exits
//     non-zero. It already carries the "requirements check failed:" header
//     and one bullet per unmet requirement.
//   - AbsentTables names every configured table that is ENTIRELY absent
//     (not yet provisioned). It is empty when no table is missing. A
//     non-empty AbsentTables with a nil Fatal is the transient "schema not
//     yet provisioned" state: the caller boots NOT READY and re-probes
//     rather than exiting.
//   - Unreachable is set when a probe failed with a transport / dial /
//     connection-refused error, i.e. ClickHouse is not yet accepting
//     connections. Like AbsentTables this is transient (it self-heals once
//     the server appears), so a Result with Unreachable set and a nil Fatal
//     is the "boot ahead of ClickHouse" race: the caller boots NOT READY and
//     re-probes rather than exiting. UnreachableErr carries the underlying
//     transport error for the /readyz reason + logs.
//   - DatabaseAbsent is set when ClickHouse is reachable but the configured
//     database does not exist yet (UNKNOWN_DATABASE / code 81). Because the
//     connection carries the database as its session default, even SELECT
//     version() fails with this code, so the version gate surfaces it FIRST and
//     short-circuits here. Like AbsentTables this is transient — the database
//     self-heals once an external writer (the collector) or the auto-create
//     hook creates it — so a Result with DatabaseAbsent set and a nil Fatal is
//     the cold-cluster "database not yet provisioned" race: the caller boots NOT
//     READY and re-probes rather than exiting. DatabaseAbsentErr carries the
//     underlying UNKNOWN_DATABASE error for the /readyz reason + logs.
//
// The buckets are independent. A pass can report several (e.g. an old server
// AND an absent table) — the Fatal bucket still wins (the caller exits),
// because a too-old server never heals on its own. An unreachable server, or a
// not-yet-created database, short-circuits the shape gate (it cannot be
// introspected) so neither ever coexists with a Fatal wrong-shape finding.
type Result struct {
	Fatal             error
	AbsentTables      []string
	Unreachable       bool
	UnreachableErr    error
	DatabaseAbsent    bool
	DatabaseAbsentErr error
}

// SchemaProvisioned reports whether the schema is fully present (whatever
// its shape) AND ClickHouse was reachable enough to confirm it: no
// configured table missing and no transport failure. The readiness wiring
// consults this to decide whether to gate /readyz on a not-yet-ready
// backend (an unreachable server can't have a confirmed-present schema).
func (r Result) SchemaProvisioned() bool {
	return len(r.AbsentTables) == 0 && !r.Unreachable && !r.DatabaseAbsent
}

// UnreachableReason renders a precise /readyz body reason for the
// transport-failure case, e.g. "clickhouse not reachable: dial tcp
// clickhouse:9000: connect: connection refused". Returns "" when the server
// was reachable.
func (r Result) UnreachableReason() string {
	if !r.Unreachable {
		return ""
	}
	if r.UnreachableErr != nil {
		return fmt.Sprintf("clickhouse not reachable: %v", r.UnreachableErr)
	}
	return "clickhouse not reachable"
}

// DatabaseAbsentReason renders a precise /readyz body reason for the
// not-yet-created-database case, naming the configured database, e.g.
// `database "otel" not yet provisioned: ...`. Returns "" when the database
// exists. The database name is carried so the reason is self-contained even
// though the underlying error already embeds it.
func (r Result) DatabaseAbsentReason(database string) string {
	if !r.DatabaseAbsent {
		return ""
	}
	if r.DatabaseAbsentErr != nil {
		return fmt.Sprintf("database %q not yet provisioned: %v", database, r.DatabaseAbsentErr)
	}
	return fmt.Sprintf("database %q not yet provisioned", database)
}

// AbsentReason renders the absent-tables list into a single precise reason
// string for the /readyz body, e.g. "schema not yet provisioned: tables
// otel_logs, otel_traces absent". Returns "" when nothing is absent.
func (r Result) AbsentReason() string {
	if len(r.AbsentTables) == 0 {
		return ""
	}
	noun := "table"
	if len(r.AbsentTables) > 1 {
		noun = "tables"
	}
	return fmt.Sprintf("schema not yet provisioned: %s %s absent", noun, strings.Join(r.AbsentTables, ", "))
}

// RunIfEnabled is the ON/OFF gate around Run. When enabled is false both
// gates are bypassed entirely and q is never touched (the caller logs the
// disabled line) and an all-clear Result is returned; when true it
// delegates to Run. Keeping the knob check here makes the disabled path
// unit-testable without standing up cmd wiring.
func RunIfEnabled(ctx context.Context, enabled bool, q Querier, req Requirements) Result {
	if !enabled {
		return Result{}
	}
	return Run(ctx, q, req)
}

// Run executes both gates against q and returns a Result splitting fatal
// (version / wrong-shape) findings from the transient absent-table set.
// The fatal bucket aggregates EVERY non-self-healing requirement so an
// operator fixes the deployment in one pass; the absent bucket lets the
// caller boot-but-wait on a not-yet-provisioned schema. Use RunIfEnabled
// for the CERBERUS_REQUIREMENTS_CHECK gate.
func Run(ctx context.Context, q Querier, req Requirements) Result {
	// Gate 1 (version) probes ClickHouse first. A transport failure here means
	// the server isn't accepting connections yet — the boot-ahead-of-ClickHouse
	// race. That's transient, not a misconfiguration, so short-circuit to an
	// Unreachable Result rather than running the shape gate against a server
	// that can't answer (and rather than mislabelling a dial error as fatal).
	// An UNKNOWN_DATABASE error here means the server IS up but the configured
	// database does not exist yet (the connection carries it as the session
	// default, so even version() fails) — equally transient, so short-circuit to
	// a DatabaseAbsent Result rather than mislabelling it a fatal version-read.
	versionProblems, unreachable, dbAbsent := checkVersion(ctx, q, req)
	if unreachable != nil {
		return Result{Unreachable: true, UnreachableErr: unreachable}
	}
	if dbAbsent != nil {
		return Result{DatabaseAbsent: true, DatabaseAbsentErr: dbAbsent}
	}

	schemaProblems, absent, unreachable := checkSchema(ctx, q, req)
	if unreachable != nil {
		return Result{Unreachable: true, UnreachableErr: unreachable}
	}

	problems := append(versionProblems, schemaProblems...)

	res := Result{AbsentTables: absent}
	if len(problems) == 0 {
		return res
	}
	var b strings.Builder
	b.WriteString("requirements check failed:")
	for _, p := range problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	res.Fatal = fmt.Errorf("%s", b.String())
	return res
}

// isUnreachable reports whether err is a transport / connectivity failure
// (ClickHouse not accepting connections yet) rather than a server-side
// rejection. It prefers typed detection — a *net.OpError (dial / read / write
// at the socket layer, e.g. connect: connection refused, no route to host)
// or any net.Error reporting a timeout — over string matching, since the
// clickhouse-go/v2 driver returns the raw dial error through its connection
// acquire path (the chclient stage-prefix wrappers use %w, preserving the
// chain). The named broad-substring fallback below only catches transport
// failures the driver might wrap opaquely enough to defeat errors.As; it is
// deliberately narrow to transport phrasing so a server-side error (a too-old
// version rejection, a wrong-shape introspection result) never matches.
func isUnreachable(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return matchesTransportPhrase(err)
}

// chCodeUnknownDatabase is ClickHouse's UNKNOWN_DATABASE server error code
// (ErrorCodes.cpp: 81), raised as "Database <name> does not exist". On a cold
// cluster the configured database may not exist yet, and the connection carries
// it as the session default (Auth.Database) — so even a database-independent
// probe like SELECT version() fails with this code until an external writer
// (the collector) or the auto-create hook creates the database.
const chCodeUnknownDatabase = 81

// isDatabaseAbsent reports whether err is ClickHouse's UNKNOWN_DATABASE
// rejection (code 81). It prefers typed detection — errors.As against
// *clickhouse.Exception checking Code — mirroring the chclient memory-limit
// detector; the named broad-substring fallback only catches the case where the
// driver wraps the exception opaquely enough to defeat errors.As. The phrases
// are deliberately narrow to the UNKNOWN_DATABASE vocabulary so a successful
// query whose result data merely mentions a database name never trips it.
func isDatabaseAbsent(err error) bool {
	if err == nil {
		return false
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) && ex.Code == chCodeUnknownDatabase {
		return true
	}
	return matchesUnknownDatabasePhrase(err)
}

// matchesUnknownDatabasePhrase is the string-matching fallback for
// isDatabaseAbsent. "unknown_database" is the distinctive code name the server
// appends; the "database … does not exist" pair covers a wrapper that drops the
// code name but keeps the message.
func matchesUnknownDatabasePhrase(err error) bool {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "unknown_database") {
		return true
	}
	return strings.Contains(msg, "database") && strings.Contains(msg, "does not exist")
}

// transportPhrases are the lower-cased substrings that mark a connectivity
// failure when the driver wraps the underlying error opaquely enough that
// errors.As on *net.OpError / net.Error no longer reaches it. Kept narrow to
// transport vocabulary so a server-side rejection never trips it.
var transportPhrases = []string{
	"connection refused",
	"connection reset",
	"no route to host",
	"network is unreachable",
	"no such host",
	"i/o timeout",
	"dial tcp",
}

// matchesTransportPhrase is the string-matching fallback for isUnreachable.
func matchesTransportPhrase(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, p := range transportPhrases {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// checkVersion runs gate 1: it reads version() and compares the parsed
// major.minor against the effective floor. Two error classes are returned
// separately so the caller short-circuits to the matching transient state: a
// TRANSPORT error (ClickHouse not reachable) via unreachable, and an
// UNKNOWN_DATABASE error (the configured database does not exist yet — the
// connection carries it as the session default, so even version() fails) via
// dbAbsent. An unreadable-but-reachable response against an existing database
// (other server-side query error, empty result, or unparseable / too-old
// version) is a fatal problem (never a silent pass).
func checkVersion(ctx context.Context, q Querier, req Requirements) (problems []string, unreachable, dbAbsent error) {
	min := req.minVersion()
	rateNote := "native rate disabled"
	if req.NativeRateEnabled {
		rateNote = "native rate enabled"
	}

	sql, args := chsql.NewQuery().Select(chsql.Call("version")).Build()
	rows, err := q.QueryStrings(ctx, sql, args...)
	if err != nil {
		if isUnreachable(err) {
			return nil, err, nil
		}
		if isDatabaseAbsent(err) {
			return nil, nil, err
		}
		return []string{fmt.Sprintf("could not read clickhouse version: %v", err)}, nil, nil
	}
	if len(rows) == 0 {
		return []string{"clickhouse version query returned no rows"}, nil, nil
	}
	raw := strings.TrimSpace(rows[0])
	got, ok := chopt.ParseVersion(raw)
	if !ok {
		return []string{fmt.Sprintf("clickhouse version %q is unparseable; required minimum %s (%s)", raw, min, rateNote)}, nil, nil
	}
	if !got.AtLeast(min) {
		return []string{fmt.Sprintf("clickhouse version %s is below the required minimum %s (%s)", raw, min, rateNote)}, nil, nil
	}
	return nil, nil, nil
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
//
// It returns two slices: the FATAL wrong-shape problems (missing columns /
// wrong attribute-map types / introspection errors) for the aggregated
// boot failure, and the ABSENT-table names — tables system.columns reports
// zero rows for, i.e. not yet provisioned. An entirely-absent table is
// transient (the schema race), so it lands in the second slice and is NOT
// a wrong-shape problem; a table that exists but has the wrong columns is.
func checkSchema(ctx context.Context, q Querier, req Requirements) (problems, absent []string, unreachable error) {
	seen := map[string]bool{}
	for _, t := range requiredTables(req) {
		if t.name == "" || seen[t.name] {
			continue
		}
		seen[t.name] = true
		probs, isAbsent, unreach := checkTable(ctx, q, req.Database, t)
		if unreach != nil {
			// A transport failure mid-introspection means the server dropped
			// (or never came up): abandon the shape gate and report unreachable
			// so the caller waits rather than recording a half-introspected
			// schema as wrong-shape.
			return nil, nil, unreach
		}
		problems = append(problems, probs...)
		if isAbsent {
			absent = append(absent, t.name)
		}
	}
	return problems, absent, nil
}

// checkTable introspects one table via system.columns and validates its
// shape. It returns the per-table wrong-shape problems plus an absent flag.
//
//   - A table that reports ZERO columns is treated as absent (not yet
//     provisioned): absent=true, no wrong-shape problem. The caller turns
//     this into a transient NOT-READY state, not a boot failure.
//   - A TRANSPORT error (ClickHouse unreachable) is returned via the third
//     value so the caller short-circuits to the transient Unreachable state —
//     a dial failure is not a clean "table missing" signal, but it is just as
//     transient as an absent table.
//   - A non-transport query error is fatal (the introspection failed against
//     a reachable server) and is reported as a problem.
//   - Otherwise each missing column and each wrong attribute-map type is
//     reported individually so the aggregated message is complete.
func checkTable(ctx context.Context, q Querier, database string, t tableReq) (problems []string, absent bool, unreachable error) {
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
		if isUnreachable(err) {
			return nil, false, err
		}
		return []string{fmt.Sprintf("could not introspect table %s: %v", t.name, err)}, false, nil
	}
	if len(rows) == 0 {
		// Entirely absent: the schema has not been provisioned yet. This is
		// the transient startup race, not a misconfiguration — surface it as
		// absent so the caller waits (NOT READY) rather than exiting.
		return nil, true, nil
	}

	types := make(map[string]string, len(rows))
	for _, r := range rows {
		types[r.Name] = r.Type
	}

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
	return problems, false, nil
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

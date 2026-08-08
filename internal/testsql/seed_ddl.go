package testsql

import (
	"slices"
	"strings"
)

// metricsTablePrefix is the storage prefix every OTel-CH metric table
// (gauge / sum / histogram / exp-histogram / summary) shares; the
// resource-attribute backfill scopes itself to these so a fixture's own
// helper tables stay untouched.
const metricsTablePrefix = "otel_metrics_"

// histogramCountSumTables are the OTel-CH metric tables that store the
// scalar `Count` / `Sum` companion columns on the same row as the bucket
// payload — the two tables a `<base>_count` / `<base>_sum` selector
// lowers against. The gauge and sum tables carry no such columns in
// production, so the backfill must NOT invent them there: a seed that
// silently grew a `Count` column would mask a real bug where a query
// projects a companion column off a table that cannot have one.
var histogramCountSumTables = []string{
	"otel_metrics_histogram",
	"otel_metrics_exponential_histogram",
}

// backfilledColumn pairs a column name with the definition the backfill
// injects for it. Every DEFAULT lets the existing positional INSERTs keep
// their value count: the backfilled INSERTs carry an explicit column list
// that omits these columns, so the defaults are filled — matching
// production, where every metric table carries a (possibly empty)
// ResourceAttributes map and a (possibly empty) ServiceName. A nil
// `tables` applies the column to every metric table; a non-nil one
// restricts it to those tables, so the injected shape stays faithful to
// the production DDL per table kind.
type backfilledColumn struct {
	name   string
	ddl    string
	tables []string
}

// appliesTo reports whether this column belongs in `table` (already
// lower-cased by the caller).
func (c backfilledColumn) appliesTo(table string) bool {
	if c.tables == nil {
		return true
	}
	return slices.Contains(c.tables, table)
}

// backfilledColumns is the ordered registry of production OTel-CH metric
// columns the spec fixtures' simplified seed DDL may omit. The first two
// are read unconditionally by the read path — ResourceAttributes by the
// resource merge, ServiceName by the dedicated-column overlay — so a seed
// table missing either fails with UNKNOWN_IDENTIFIER. `Count` / `Sum` are
// read the same way by the classic- and exp-histogram companion lowering,
// which projects `toFloat64(Count)` / `toFloat64(Sum)` against those two
// tables whenever a selector can name a `_count` / `_sum` series —
// including the regex `__name__` fan-out, which reaches BOTH histogram
// tables for any selector at all, so a seed that declares neither column
// fails even when it seeds no histogram rows. Extend this slice when a
// new column joins the unconditional projection.
var backfilledColumns = []backfilledColumn{
	{name: "ResourceAttributes", ddl: "ResourceAttributes Map(String, String) DEFAULT map()"},
	{name: "ServiceName", ddl: "ServiceName LowCardinality(String) DEFAULT ''"},
	{name: "Count", ddl: "Count UInt64 DEFAULT 0", tables: histogramCountSumTables},
	{name: "Sum", ddl: "Sum Float64 DEFAULT 0", tables: histogramCountSumTables},
	// AggregationTemporality backs the rate() / increase() DELTA-vs-CUMULATIVE
	// runtime branch (chsql.CounterOrDeltaSum / internal/promql's
	// counterIncreaseFold — see issue #1628). DEFAULT 2 is
	// schema.AggregationTemporalityCumulative, matching the historical,
	// pre-#1628 unconditional reading every existing seed that omits the
	// column implicitly relied on. Scoped to the sum + classic-histogram
	// tables (temporalityTables) — the two tables rate/increase's
	// TemporalityColumn wiring ever reads from; the gauge table has no such
	// column in production and exp-histogram stays out of #1628's scope
	// (see the nil-temporality call sites in histogram_quantile_range.go /
	// histogram_quantile_native_window.go).
	{name: "AggregationTemporality", ddl: "AggregationTemporality Int32 DEFAULT 2", tables: temporalityTables},
}

// temporalityTables are the OTel-CH metric tables rate() / increase()'s
// DELTA-vs-CUMULATIVE branch ever reads AggregationTemporality from — see
// the AggregationTemporality entry in backfilledColumns.
var temporalityTables = []string{"otel_metrics_sum", "otel_metrics_histogram"}

// tracesTableName is the fixed table name TraceQL round-trip seeds declare
// for the spans table — schema.DefaultOTelTraces().SpansTable, mirrored here
// as a literal so this build-tag-free file doesn't need to import
// internal/schema just for one string.
const tracesTableName = "otel_traces"

// tracesBackfilledColumns are the production otel_traces columns
// wrapWithSampleProjection's canonical wrap (internal/api/tempo/handler.go)
// reads unconditionally: TraceId / SpanId / ParentSpanId feed the reserved
// `__cerberus_*` Attributes keys, SpanName / ResourceAttributes / Timestamp /
// Duration are the four Sample-shape source columns. A TraceQL spec fixture's
// seed is written minimally — declaring only the columns ITS OWN pre-wrap
// `-- sql --` touches — so most fixtures omit some of these; the strict-scan
// differential's wrap reconstruction (traceqlWrapForStrictScan) needs all
// seven present or the wrapped SQL 502s with UNKNOWN_IDENTIFIER, a false
// failure that has nothing to do with the strict-scan / type-coercion bug
// class this lane exists to catch. Types + names mirror the upstream OTel-CH
// traces DDL (see internal/schema/traces.go's DefaultOTelTraces).
var tracesBackfilledColumns = []backfilledColumn{
	{name: "TraceId", ddl: "TraceId String DEFAULT ''"},
	{name: "SpanId", ddl: "SpanId String DEFAULT ''"},
	{name: "ParentSpanId", ddl: "ParentSpanId String DEFAULT ''"},
	{name: "SpanName", ddl: "SpanName String DEFAULT ''"},
	{name: "Duration", ddl: "Duration Int64 DEFAULT 0"},
	{name: "Timestamp", ddl: "Timestamp DateTime64(9) DEFAULT toDateTime64(0, 9)"},
	{name: "ResourceAttributes", ddl: "ResourceAttributes Map(String, String) DEFAULT map()"},
}

func BackfillTracesColumns(stmts []string) []string {
	// table name -> ordered column names (pre-backfill) for tables we
	// injected a column into. Only these tables' INSERTs are rewritten.
	cols := map[string][]string{}
	out := make([]string, 0, len(stmts))
	for _, stmt := range stmts {
		if table, colNames, body, ok := parseTracesCreate(stmt); ok {
			cols[table] = colNames
			out = append(out, body)
			continue
		}
		if rewritten, ok := rewriteBackfilledInsert(stmt, cols); ok {
			out = append(out, rewritten)
			continue
		}
		out = append(out, stmt)
	}
	return out
}

// parseTracesCreate reports whether stmt is a `CREATE TABLE otel_traces` that
// omits at least one [tracesBackfilledColumns] entry. On a match it returns
// the table name, the ordered pre-backfill column names, and the rewritten
// statement with the missing columns appended to the column list (order is
// irrelevant — the INSERTs become explicit-column, mirroring
// [parseMetricsCreate]).
func parseTracesCreate(stmt string) (table string, colNames []string, rewritten string, ok bool) {
	trimmed := stripLeadingNoise(stmt)
	rest, ok := createTableTail(trimmed)
	if !ok {
		return "", nil, "", false
	}
	name := strings.ToLower(strings.TrimSpace(firstToken(rest)))
	if name != tracesTableName {
		return "", nil, "", false
	}
	open := strings.IndexByte(stmt, '(')
	if open < 0 {
		return "", nil, "", false
	}
	closeParen := matchParen(stmt, open)
	if closeParen < 0 {
		return "", nil, "", false
	}
	bodyCols := stmt[open+1 : closeParen]
	defs := splitTopLevelCommas(bodyCols)
	declared := make(map[string]bool, len(defs))
	names := make([]string, 0, len(defs))
	normDefs := make([]string, len(defs))
	changed := false
	for i, d := range defs {
		cn := firstToken(strings.TrimSpace(d))
		if cn != "" {
			declared[cn] = true
			names = append(names, cn)
		}
		nd := normalizeTracesColumnDef(d)
		if nd != d {
			changed = true
		}
		normDefs[i] = nd
	}
	missing := make([]string, 0, len(tracesBackfilledColumns))
	for _, c := range tracesBackfilledColumns {
		if !declared[c.name] {
			missing = append(missing, c.ddl)
		}
	}
	if len(missing) == 0 && !changed {
		return "", nil, "", false
	}
	newDefs := make([]string, 0, len(normDefs)+len(missing))
	newDefs = append(newDefs, normDefs...)
	for _, ddl := range missing {
		newDefs = append(newDefs, " "+ddl)
	}
	rewritten = stmt[:open+1] + strings.Join(newDefs, ",") + stmt[closeParen:]
	return name, names, rewritten, true
}

// normalizeTracesColumnDef rewrites a single otel_traces column definition so
// it type-matches production's OTel-CH schema closely enough for the
// strict-scan wrap reconstruction (traceqlWrapForStrictScan) to strict-scan
// cleanly, without touching any other lane's seed application — only the
// strict-scan differential calls [BackfillTracesColumns]. Two independent
// fixups, each addressing a fixture-authoring shortcut that predates the
// wrap reconstruction and was invisible under the old pre-wrap-only `-- sql
// --`:
//
//  1. `MATERIALIZED` -> `DEFAULT`. ClickHouse's bare `SELECT *` omits
//     MATERIALIZED columns from the result set by design; several fixtures
//     use MATERIALIZED as a terse way to derive a column (typically
//     ResourceAttributes) from another column's value without a verbose
//     per-row INSERT. wrapWithSampleProjection's canonical branch always
//     projects from a `SELECT * FROM otel_traces WHERE ...` subquery, so a
//     MATERIALIZED column it needs silently vanishes from that subquery's
//     column set ("Unknown identifier") — not a real production bug, since
//     real otel_traces columns are never MATERIALIZED. DEFAULT computes the
//     identical value at INSERT time (these fixtures never override it) and
//     DOES appear in `SELECT *`.
//  2. Timestamp / Duration retyped to their real OTel-CH types
//     (DateTime64(9) / Int64, see internal/schema/traces.go's
//     DefaultOTelTraces) when a fixture declares them as a bare integer
//     (UInt64 etc) — a shortcut that was invisible while nothing read
//     Timestamp through the typed TimeUnix (time.Time) alias the wrap
//     projects it under.
func normalizeTracesColumnDef(def string) string {
	rewritten := strings.Replace(def, " MATERIALIZED ", " DEFAULT ", 1)
	switch firstToken(strings.TrimSpace(rewritten)) {
	case "Timestamp":
		if !hasColumnType(rewritten, "DateTime64") {
			rewritten = replaceColumnType(rewritten, "DateTime64(9)")
		}
	case "Duration":
		if !hasColumnType(rewritten, "Int64") {
			rewritten = replaceColumnType(rewritten, "Int64")
		}
	}
	return rewritten
}

// hasColumnType reports whether def's column-type token starts with prefix
// (e.g. prefix "DateTime64" matches both "DateTime64(9)" and a bare
// "DateTime64").
func hasColumnType(def, prefix string) bool {
	trimmed := strings.TrimSpace(def)
	name := firstToken(trimmed)
	rest := strings.TrimSpace(trimmed[len(name):])
	return strings.HasPrefix(rest, prefix)
}

// replaceColumnType swaps def's leading type token (the run up to the next
// top-level space, honouring the type's own parens — e.g. "DateTime64(9)")
// for wantType, leaving any trailing DEFAULT/MATERIALIZED clause untouched.
func replaceColumnType(def, wantType string) string {
	trimmed := strings.TrimSpace(def)
	name := firstToken(trimmed)
	rest := strings.TrimSpace(trimmed[len(name):])
	depth := 0
	end := len(rest)
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ' ':
			if depth == 0 {
				end = i
			}
		}
		if end != len(rest) {
			break
		}
	}
	return name + " " + wantType + rest[end:]
}

func BackfillMetricsColumns(stmts []string) []string {
	// table name -> ordered column names (pre-backfill) for tables we
	// injected a column into. Only these tables' INSERTs are rewritten.
	cols := map[string][]string{}
	out := make([]string, 0, len(stmts))
	for _, stmt := range stmts {
		if table, colNames, body, ok := parseMetricsCreate(stmt); ok {
			cols[table] = colNames
			out = append(out, body)
			continue
		}
		if rewritten, ok := rewriteBackfilledInsert(stmt, cols); ok {
			out = append(out, rewritten)
			continue
		}
		out = append(out, stmt)
	}
	return out
}

// createTableHeads are the `CREATE … TABLE ` spellings a seed may open a
// metric-table definition with. Matching only the bare `CREATE TABLE `
// form is what let the exotic-PromQL and perf seeds — both written as
// `CREATE OR REPLACE TABLE` — slip past the backfill and fail at query
// time with UNKNOWN_IDENTIFIER on a column the read path projects.
// Longest-first, so `CREATE OR REPLACE TABLE ` is never mis-read as the
// bare form.
var createTableHeads = []string{
	"CREATE OR REPLACE TABLE ",
	"CREATE TABLE IF NOT EXISTS ",
	"CREATE TABLE ",
}

// createTableTail returns everything after whichever [createTableHeads]
// spelling stmt opens with — i.e. the text starting at the table name.
func createTableTail(trimmed string) (string, bool) {
	upper := strings.ToUpper(trimmed)
	for _, head := range createTableHeads {
		if strings.HasPrefix(upper, head) {
			return trimmed[len(head):], true
		}
	}
	return "", false
}

// parseMetricsCreate reports whether stmt is a `CREATE TABLE
// otel_metrics_*` that declares an `Attributes` column and omits at least
// one [backfilledColumns] entry. On a match it returns the table name, the
// ordered pre-backfill column names, and the rewritten statement with the
// missing columns injected right after the Attributes column.
func parseMetricsCreate(stmt string) (table string, colNames []string, rewritten string, ok bool) {
	trimmed := stripLeadingNoise(stmt)
	rest, ok := createTableTail(trimmed)
	if !ok {
		return "", nil, "", false
	}
	name := strings.ToLower(strings.TrimSpace(firstToken(rest)))
	if !strings.HasPrefix(name, metricsTablePrefix) {
		return "", nil, "", false
	}
	open := strings.IndexByte(stmt, '(')
	if open < 0 {
		return "", nil, "", false
	}
	// Match the column-list close paren by balancing from the first `(`
	// — NOT strings.LastIndexByte, which would grab the ENGINE/ORDER BY
	// `(MetricName, …)` paren and drag the engine clause into the column
	// body.
	closeParen := matchParen(stmt, open)
	if closeParen < 0 {
		return "", nil, "", false
	}
	bodyCols := stmt[open+1 : closeParen]
	defs := splitTopLevelCommas(bodyCols)
	hasAttributes := false
	declared := make(map[string]bool, len(defs))
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		cn := firstToken(strings.TrimSpace(d))
		if cn == "Attributes" {
			hasAttributes = true
		}
		if cn != "" {
			declared[cn] = true
			names = append(names, cn)
		}
	}
	missing := make([]string, 0, len(backfilledColumns))
	for _, c := range backfilledColumns {
		if !declared[c.name] && c.appliesTo(name) {
			missing = append(missing, c.ddl)
		}
	}
	if !hasAttributes || len(missing) == 0 {
		return "", nil, "", false
	}
	// Inject the columns directly after the Attributes definition so the
	// rewritten DDL reads naturally; column order is otherwise irrelevant
	// because the INSERTs become explicit-column.
	newDefs := make([]string, 0, len(defs)+len(missing))
	for _, d := range defs {
		newDefs = append(newDefs, d)
		if firstToken(strings.TrimSpace(d)) != "Attributes" {
			continue
		}
		for _, ddl := range missing {
			newDefs = append(newDefs, " "+ddl)
		}
	}
	rewritten = stmt[:open+1] + strings.Join(newDefs, ",") + stmt[closeParen:]
	return name, names, rewritten, true
}

// rewriteBackfilledInsert rewrites a positional `INSERT INTO <table> VALUES`
// into one carrying the recorded column list (sans the injected columns)
// so the DEFAULT-filled columns do not break the value count. Inserts into
// untracked tables, or that already carry an explicit column list, pass
// through unchanged. Shared by both the metrics backfill
// ([BackfillMetricsColumns]) and the traces backfill
// ([BackfillTracesColumns]) — the rewrite is column-registry-driven, not
// metrics-specific.
func rewriteBackfilledInsert(stmt string, cols map[string][]string) (string, bool) {
	trimmed := stripLeadingNoise(stmt)
	prefix := stmt[:len(stmt)-len(trimmed)]
	upper := strings.ToUpper(trimmed)
	const needle = "INSERT INTO "
	if !strings.HasPrefix(upper, needle) {
		return "", false
	}
	rest := trimmed[len(needle):]
	name := strings.ToLower(strings.TrimSpace(firstToken(rest)))
	colNames, tracked := cols[name]
	if !tracked {
		return "", false
	}
	// Find the table-name token end; if the next non-space char is '(',
	// the INSERT already carries an explicit column list — leave it.
	afterName := strings.TrimLeft(rest[len(firstToken(rest)):], " \t\n\r")
	if strings.HasPrefix(afterName, "(") {
		return "", false
	}
	colList := "(" + strings.Join(colNames, ", ") + ") "
	return prefix + needle + firstToken(rest) + " " + colList + afterName, true
}

// matchParen returns the index of the `)` that balances the `(` at
// position open in s, honouring single-quoted strings, or -1 when
// unbalanced. open must index a `(`.
func matchParen(s string, open int) int {
	depth := 0
	inStr := false
	esc := false
	for i := open; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '\'':
			inStr = !inStr
		case inStr:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// firstToken returns the leading identifier of s (up to the first space,
// tab, newline, '(' or ',').
func firstToken(s string) string {
	s = strings.TrimLeft(s, " \t\n\r")
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r', '(', ',':
			return s[:i]
		}
	}
	return s
}

// splitTopLevelCommas splits s on commas that sit at paren-depth 0,
// shielding commas inside nested parens (e.g. `Map(String, String)`) and
// single-quoted strings. Used to walk a CREATE TABLE column-definition
// list.
func splitTopLevelCommas(s string) []string {
	var (
		out   []string
		buf   strings.Builder
		depth int
		inStr bool
		esc   bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
			buf.WriteByte(c)
		case c == '\\' && inStr:
			esc = true
			buf.WriteByte(c)
		case c == '\'':
			inStr = !inStr
			buf.WriteByte(c)
		case inStr:
			buf.WriteByte(c)
		case c == '(':
			depth++
			buf.WriteByte(c)
		case c == ')':
			depth--
			buf.WriteByte(c)
		case c == ',' && depth == 0:
			out = append(out, buf.String())
			buf.Reset()
		default:
			buf.WriteByte(c)
		}
	}
	if strings.TrimSpace(buf.String()) != "" {
		out = append(out, buf.String())
	}
	return out
}

// PromoteCreateTable rewrites a bare `CREATE TABLE …` statement to
// `CREATE OR REPLACE TABLE …` so re-running a seed against a session that
// already holds the table is idempotent. Other variants
// (`CREATE OR REPLACE TABLE`, `CREATE TABLE IF NOT EXISTS`,
// `CREATE TEMPORARY TABLE`) are left untouched.
//
// Leading whitespace and SQL line comments (`-- …`) are skipped when
// locating the `CREATE TABLE` keyword: fixture authors routinely
// document the seed shape with a comment block above the DDL, and
// without comment-skipping those fixtures would bypass the
// OR-REPLACE rewrite and trip TABLE_ALREADY_EXISTS on the second
// run inside a shared session.
func PromoteCreateTable(stmt string) string {
	trimmed := stripLeadingNoise(stmt)
	prefix := stmt[:len(stmt)-len(trimmed)]
	upper := strings.ToUpper(trimmed)
	const needle = "CREATE TABLE "
	if !strings.HasPrefix(upper, needle) {
		return stmt
	}
	rest := trimmed[len(needle):]
	return prefix + "CREATE OR REPLACE TABLE " + rest
}

// stripLeadingNoise consumes leading whitespace and `-- …\n` line
// comments from s, returning the remaining suffix. Block comments
// (`/* … */`) are not stripped — no current fixture uses them — but
// the loop is structured so a future extension stays a one-case add.
func stripLeadingNoise(s string) string {
	for {
		t := strings.TrimLeft(s, " \t\n\r")
		if strings.HasPrefix(t, "--") {
			if nl := strings.IndexByte(t, '\n'); nl >= 0 {
				s = t[nl+1:]
				continue
			}
			return ""
		}
		return t
	}
}

// SplitStatements splits a seed script on top-level semicolons, shielding
// semicolons inside single-quoted strings AND inside `-- …` line comments.
//
// The comment-awareness is load-bearing: a fixture's `-- seed --` block
// routinely opens with a prose comment that contains an apostrophe (e.g.
// "the runner's `SELECT *`"). Without skipping comment runs, that stray
// apostrophe flips the in-string state machine and the very next real `;`
// — the one terminating `CREATE TABLE … ENGINE = Memory` — is swallowed as
// "inside a string", gluing the CREATE and the following INSERT into one
// statement. chdb-go's database/sql driver tolerates the resulting
// multi-statement; clickhouse-go's native Exec rejects it (code 62,
// "Multi-statements are not allowed"). Skipping comment bytes (and the `;`
// they may contain) keeps both drivers fed one statement at a time.
func SplitStatements(s string) []string {
	var (
		out   []string
		buf   strings.Builder
		inStr bool
		esc   bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
			buf.WriteByte(c)
		case c == '\\' && inStr:
			esc = true
			buf.WriteByte(c)
		// A `--` line comment outside a string runs to end-of-line; copy it
		// through verbatim (so the emitted statement still documents itself)
		// but do NOT let its bytes touch the quote / semicolon state machine.
		case c == '-' && !inStr && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				buf.WriteByte(s[i])
				i++
			}
			if i < len(s) {
				buf.WriteByte(s[i]) // the '\n'
			}
		case c == '\'':
			inStr = !inStr
			buf.WriteByte(c)
		case c == ';' && !inStr:
			out = append(out, buf.String())
			buf.Reset()
		default:
			buf.WriteByte(c)
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

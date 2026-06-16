//go:build chdb

package chclienttest

import "strings"

// chdbEOFSentinel is the spurious "empty row" error chdb-go's parquet
// driver returns instead of io.EOF at end-of-iteration (see chdb-go
// v1.11.0 chdb/driver/parquet.go). tolerantRowsErr swallows it so
// rows.Err() looks normal to callers. Any other error is real.
const chdbEOFSentinel = "empty row"

func tolerantRowsErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), chdbEOFSentinel) {
		return nil
	}
	return err
}

// mapColumnNames is the conservative allow-list of OTel Map column
// names that the rewriter will wrap in toJSONString(...) before issuing
// the query against chDB. We don't have type information here; the
// rewrite is a textual transform keyed off this list. Extend the list
// if a new Map column lands in the schema.
//
// ExemplarAttributes is the alias the chsql.EmitQueryExemplars outer
// SELECT projects for `Exemplars.FilteredAttributes` (a Map(LowCardinality
// (String),String)). Without the toJSONString wrap chDB's parquet driver
// panics decoding the column as a Go string in the chclienttest scan
// path — same Map-panic probe Attributes / ResourceAttributes hit.
// log_attributes / stream_labels are the aliases
// loki.buildDetectedFieldsSQL projects for `LogAttributes` /
// `ResourceAttributes` — distinct from the source column names so the
// toJSONString wrap can't shadow the raw map the WHERE predicate
// references (CH resolves WHERE identifiers against SELECT aliases
// first).
var mapColumnNames = []string{
	"Attributes",
	"ExemplarAttributes",
	"LogAttributes",
	"ResourceAttributes",
	"ScopeAttributes",
	"SpanAttributes",
	"log_attributes",
	"stream_labels",
}

func isMapColumn(name string) bool {
	for _, c := range mapColumnNames {
		if name == c {
			return true
		}
	}
	return false
}

// rewriteMapProjections wraps any top-level SELECT projection whose
// alias is a known Map column in toJSONString(...). Only the outermost
// SELECT is touched — subqueries keep their Map columns raw because
// CH consumes them server-side.
//
// Recognised shapes (mirrors test/spec/runner_chdb.go):
//
//	`Attributes`                       → toJSONString(`Attributes`) AS `Attributes`
//	<expr> AS `Attributes`             → toJSONString(<expr>) AS `Attributes`
//	`Attributes` AS `Attributes`       → toJSONString(`Attributes`) AS `Attributes`
//
// Anything else passes through. If a Map column slips through unwrapped
// the chdb-go parquet decoder will panic loudly at scan time, which is
// the failure mode we want.
func rewriteMapProjections(query string) string {
	// Top-level UNION-ALL shape: `(SELECT …) UNION ALL (SELECT …) …`. The
	// fan-in metadata /series path (internal/api/prom/metadata.go) renders
	// the combined Sample query as a bare UnionAll of parenthesised SELECT
	// arms — it does NOT start with `SELECT `, so the single-SELECT rewrite
	// below would pass it through unwrapped and the Map-typed `Attributes`
	// column would hit the chdb parquet decoder's NULL path. Rewrite each
	// arm's outer SELECT independently and re-join.
	if arms, ok := splitTopLevelUnionAll(query); ok {
		for i, a := range arms {
			arms[i] = rewriteMapProjections(a)
		}
		return strings.Join(arms, " UNION ALL ")
	}
	// A UNION-ALL arm arrives wrapped in its own parens — `(SELECT …)`.
	// Strip the outer parens, rewrite the inner SELECT, re-wrap.
	if inner, ok := stripOuterParens(query); ok {
		return "(" + rewriteMapProjections(inner) + ")"
	}
	head, tail := splitOuterSelect(query)
	if head == "" {
		return query
	}
	projs := splitProjections(head)
	for i, p := range projs {
		expr, alias := splitAlias(p)
		if alias == "" {
			alias = unquoteBackticks(strings.TrimSpace(expr))
		}
		if !isMapColumn(alias) {
			continue
		}
		projs[i] = "toJSONString(" + expr + ") AS `" + alias + "`"
	}
	return "SELECT " + strings.Join(projs, ", ") + tail
}

// splitTopLevelUnionAll splits a `<arm> UNION ALL <arm> …` statement on
// its depth-0 ` UNION ALL ` separators, returning the arms verbatim (each
// typically a parenthesised `(SELECT …)`). Returns ok=false when no
// depth-0 ` UNION ALL ` is present, so a plain single SELECT falls through
// to the single-SELECT rewrite. Single-quoted strings and backtick
// identifiers shield any ` UNION ALL ` substring inside literals.
func splitTopLevelUnionAll(query string) (arms []string, ok bool) {
	const sep = " UNION ALL "
	var (
		out   []string
		start int
		depth int
		inStr byte
	)
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case inStr != 0:
			if c == inStr {
				inStr = 0
			}
		case c == '\'' || c == '`':
			inStr = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		}
		if depth == 0 && inStr == 0 && i+len(sep) <= len(query) &&
			strings.EqualFold(query[i:i+len(sep)], sep) {
			out = append(out, strings.TrimSpace(query[start:i]))
			i += len(sep) - 1
			start = i + 1
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	out = append(out, strings.TrimSpace(query[start:]))
	return out, true
}

// stripOuterParens returns the contents of a fully-parenthesised
// expression — `(<inner>)` → `<inner>` — when the leading `(` matches the
// trailing `)` at depth 0 (i.e. the whole string is one parenthesised
// group). Returns ok=false otherwise, so a query that merely contains
// parens (but isn't wholly wrapped) falls through untouched. Quote-aware
// so a literal `)` inside a string doesn't close the group early.
func stripOuterParens(s string) (inner string, ok bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return "", false
	}
	depth := 0
	inStr := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr != 0:
			if c == inStr {
				inStr = 0
			}
		case c == '\'' || c == '`':
			inStr = c
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				// The opening paren closed before the end — the string is
				// not a single wrapped group (e.g. `(a) UNION ALL (b)`).
				return "", false
			}
		}
	}
	if depth != 0 {
		return "", false
	}
	return strings.TrimSpace(s[1 : len(s)-1]), true
}

// splitOuterSelect splits `SELECT <projs> FROM ...` at the depth-0
// FROM. Returns ("", "") if the query isn't a SELECT or the FROM is
// missing.
func splitOuterSelect(query string) (head, tail string) {
	upper := strings.ToUpper(query)
	if !strings.HasPrefix(upper, "SELECT ") {
		return "", ""
	}
	rest := query[len("SELECT "):]
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && i+6 <= len(rest) && strings.EqualFold(rest[i:i+6], " FROM ") {
			return rest[:i], rest[i:]
		}
	}
	return "", ""
}

// splitProjections splits a projection list on depth-0 commas.
// Single-quoted strings and backticks shield commas.
func splitProjections(s string) []string {
	var (
		out   []string
		buf   strings.Builder
		depth int
		inStr byte
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr != 0:
			if c == inStr {
				inStr = 0
			}
			buf.WriteByte(c)
		case c == '\'' || c == '`':
			inStr = c
			buf.WriteByte(c)
		case c == '(':
			depth++
			buf.WriteByte(c)
		case c == ')':
			depth--
			buf.WriteByte(c)
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(buf.String()))
			buf.Reset()
		default:
			buf.WriteByte(c)
		}
	}
	if buf.Len() > 0 {
		out = append(out, strings.TrimSpace(buf.String()))
	}
	return out
}

// splitAlias splits an `<expr> AS <alias>` projection into (expr,
// alias). When no AS clause is present returns (s, "").
func splitAlias(s string) (expr, alias string) {
	depth := 0
	inStr := byte(0)
	lower := strings.ToLower(s)
	for i := 0; i+4 <= len(s); i++ {
		c := s[i]
		switch {
		case inStr != 0:
			if c == inStr {
				inStr = 0
			}
		case c == '\'' || c == '`':
			inStr = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		}
		if depth == 0 && inStr == 0 && lower[i:i+4] == " as " {
			alias = strings.TrimSpace(s[i+4:])
			alias = unquoteBackticks(alias)
			return strings.TrimSpace(s[:i]), alias
		}
	}
	return s, ""
}

func unquoteBackticks(s string) string {
	if len(s) >= 2 && s[0] == '`' && s[len(s)-1] == '`' {
		return s[1 : len(s)-1]
	}
	return s
}

// splitStatements splits a multi-statement SQL script on top-level
// semicolons. Statements inside single-quoted strings keep their
// semicolons literal. Mirrors test/spec/runner_chdb.go.
func splitStatements(s string) []string {
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

func isBlank(s string) bool {
	return strings.TrimSpace(s) == ""
}

// metricsTablePrefix scopes the resource-attribute backfill to OTel-CH
// metric tables; helper tables a test seeds stay untouched.
const metricsTablePrefix = "otel_metrics_"

// resourceAttributesColumnDDL is the column the backfill injects (DEFAULT
// map() so positional INSERTs keep their value count — the backfilled
// INSERTs carry an explicit column list omitting this column).
const resourceAttributesColumnDDL = "ResourceAttributes Map(String, String) DEFAULT map()"

// backfillResourceAttributes mirrors the production OTel-CH invariant —
// every metric table carries a `ResourceAttributes` Map column — onto the
// handler tests' simplified seed DDL, so the rc.5 read-path projection
// (`mapUpdate(sanitize(ResourceAttributes), Attributes)`) resolves rather
// than failing with UNKNOWN_IDENTIFIER. A `CREATE TABLE otel_metrics_*`
// that declares `Attributes` but no `ResourceAttributes` gets the column
// injected (DEFAULT map()), and every subsequent positional `INSERT INTO
// <that table> VALUES …` is rewritten to an explicit column list (sans
// ResourceAttributes) so the DEFAULT fills the gap. Seeds that already
// declare the column, or already use an explicit INSERT column list, pass
// through untouched. Mirrors test/spec/runner_chdb.go::backfillResourceAttributes.
func backfillResourceAttributes(stmts []string) []string {
	cols := map[string][]string{}
	out := make([]string, 0, len(stmts))
	for _, stmt := range stmts {
		if table, names, rewritten, ok := parseMetricsCreate(stmt); ok {
			cols[table] = names
			out = append(out, rewritten)
			continue
		}
		if rewritten, ok := rewriteMetricsInsert(stmt, cols); ok {
			out = append(out, rewritten)
			continue
		}
		out = append(out, stmt)
	}
	return out
}

func parseMetricsCreate(stmt string) (table string, colNames []string, rewritten string, ok bool) {
	trimmed := strings.TrimLeft(stmt, " \t\n\r")
	upper := strings.ToUpper(trimmed)
	if !strings.HasPrefix(upper, "CREATE TABLE ") {
		return "", nil, "", false
	}
	name := strings.ToLower(strings.TrimSpace(ddlFirstToken(trimmed[len("CREATE TABLE "):])))
	if !strings.HasPrefix(name, metricsTablePrefix) {
		return "", nil, "", false
	}
	open := strings.IndexByte(stmt, '(')
	if open < 0 {
		return "", nil, "", false
	}
	closeParen := ddlMatchParen(stmt, open)
	if closeParen < 0 {
		return "", nil, "", false
	}
	defs := ddlSplitTopLevelCommas(stmt[open+1 : closeParen])
	hasAttributes, hasResource := false, false
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		cn := ddlFirstToken(strings.TrimSpace(d))
		switch cn {
		case "Attributes":
			hasAttributes = true
		case "ResourceAttributes":
			hasResource = true
		}
		if cn != "" {
			names = append(names, cn)
		}
	}
	if !hasAttributes || hasResource {
		return "", nil, "", false
	}
	newDefs := make([]string, 0, len(defs)+1)
	for _, d := range defs {
		newDefs = append(newDefs, d)
		if ddlFirstToken(strings.TrimSpace(d)) == "Attributes" {
			newDefs = append(newDefs, " "+resourceAttributesColumnDDL)
		}
	}
	rewritten = stmt[:open+1] + strings.Join(newDefs, ",") + stmt[closeParen:]
	return name, names, rewritten, true
}

func rewriteMetricsInsert(stmt string, cols map[string][]string) (string, bool) {
	trimmed := strings.TrimLeft(stmt, " \t\n\r")
	prefix := stmt[:len(stmt)-len(trimmed)]
	upper := strings.ToUpper(trimmed)
	const needle = "INSERT INTO "
	if !strings.HasPrefix(upper, needle) {
		return "", false
	}
	rest := trimmed[len(needle):]
	name := strings.ToLower(strings.TrimSpace(ddlFirstToken(rest)))
	colNames, tracked := cols[name]
	if !tracked {
		return "", false
	}
	afterName := strings.TrimLeft(rest[len(ddlFirstToken(rest)):], " \t\n\r")
	if strings.HasPrefix(afterName, "(") {
		return "", false // already explicit-column
	}
	colList := "(" + strings.Join(colNames, ", ") + ") "
	return prefix + needle + ddlFirstToken(rest) + " " + colList + afterName, true
}

func ddlFirstToken(s string) string {
	s = strings.TrimLeft(s, " \t\n\r")
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r', '(', ',':
			return s[:i]
		}
	}
	return s
}

func ddlMatchParen(s string, open int) int {
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

func ddlSplitTopLevelCommas(s string) []string {
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

// promoteCreateTable rewrites a bare `CREATE TABLE …` statement to
// `CREATE OR REPLACE TABLE …` so re-running a seed against a chDB
// session that already holds the table is idempotent. Other variants
// (`CREATE OR REPLACE TABLE`, `CREATE TABLE IF NOT EXISTS`,
// `CREATE TEMPORARY TABLE`) are left untouched — the rewrite is
// conservative on purpose. Leading whitespace and comments are
// preserved verbatim by re-emitting them.
func promoteCreateTable(stmt string) string {
	trimmed := strings.TrimLeft(stmt, " \t\n\r")
	prefix := stmt[:len(stmt)-len(trimmed)]
	upper := strings.ToUpper(trimmed)
	// Only touch the bare form. Everything else passes through.
	const needle = "CREATE TABLE "
	if !strings.HasPrefix(upper, needle) {
		return stmt
	}
	rest := trimmed[len(needle):]
	return prefix + "CREATE OR REPLACE TABLE " + rest
}

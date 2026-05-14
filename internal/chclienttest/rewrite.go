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
var mapColumnNames = []string{
	"Attributes",
	"ResourceAttributes",
	"ScopeAttributes",
	"SpanAttributes",
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

//go:build chdb

// Package spec — chDB-backed round-trip executor.
//
// This file is compiled only when the `chdb` build tag is set, which
// also implies the chdb-go driver and libchdb.so are present (see
// `just chdb-install`). The default `just test` lane stays
// CGO_ENABLED=0 and never sees this code.
//
// The executor opens an ephemeral in-process chDB session, runs the
// fixture's `seed:` DDL+INSERT statements, executes the emitted `sql:`
// (with `args:` bound), and asserts the resulting rows match the
// `expected_rows:` JSON. Map columns are wrapped server-side in
// `toJSONString(...)` to dodge the native parquet Map scan panic
// documented by the chDB driver probe.
package spec

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

// chdbEOFSentinel is the spurious end-of-iteration error chdb-go's
// parquet driver returns instead of io.EOF (see chdb-go v1.11.0's
// `parquet.go`: `return fmt.Errorf("empty row")`). It surfaces on
// rows.Err() and must be ignored — any other error is real.
const chdbEOFSentinel = "empty row"

// tolerantRowsErr matches the helper used by the chDB probe in
// internal/chclient/chdb_probe_test.go.
func tolerantRowsErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), chdbEOFSentinel) {
		return nil
	}
	return err
}

// mapColumnNames is the conservative list of OTel-CH Map column names
// the runner will rewrite to `toJSONString(<name>)` in the emitted
// SQL before execution. We don't have type information at this
// layer; the rewrite is a textual transform keyed off this allow-
// list. Authors with custom Map columns can extend the list as
// fixtures grow.
var mapColumnNames = []string{
	"Attributes",
	"ResourceAttributes",
	"ScopeAttributes",
	"SpanAttributes",
}

// isMapColumn reports whether name (a backtick-quoted alias) is one
// of the known OTel Map column names.
func isMapColumn(name string) bool {
	for _, c := range mapColumnNames {
		if name == c {
			return true
		}
	}
	return false
}

// rewriteMapProjections wraps any top-level SELECT projection whose
// alias is a known Map column in toJSONString(...). The transform
// fires on the OUTER SELECT only — subqueries keep their Map columns
// as raw maps because CH consumes them server-side.
//
// Two shapes are handled:
//
//	`Attributes`                       → toJSONString(`Attributes`) AS `Attributes`
//	<expr> AS `Attributes`             → toJSONString(<expr>) AS `Attributes`
//	`Attributes` AS `Attributes`       → toJSONString(`Attributes`) AS `Attributes`
//
// Anything else passes through; chdb-go will raise a Parquet panic
// at scan time if a Map column slips through unwrapped, which makes
// the failure mode loud and easy to debug.
func rewriteMapProjections(query string) string {
	head, tail := splitOuterSelect(query)
	if head == "" {
		return query
	}
	projs := splitProjections(head)
	for i, p := range projs {
		expr, alias := splitAlias(p)
		// Implicit alias: bare `Col` or `Qual.\`Col\`` projection.
		if alias == "" {
			alias = mapColAlias(strings.TrimSpace(expr))
		}
		if !isMapColumn(alias) {
			continue
		}
		projs[i] = "toJSONString(" + expr + ") AS `" + alias + "`"
	}
	return "SELECT " + strings.Join(projs, ", ") + tail
}

// mapColAlias derives the implicit projection alias for a bare column
// reference. Handles both `\`Col\`` (unqualified) and `Q.\`Col\``
// (qualifier-prefixed, e.g. the `L.\`Attributes\`` form vector_join
// emits) so the surrounding Map-rewrite pass can recognise Attributes
// projected through the join's left / right side.
func mapColAlias(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return unquoteBackticks(s)
}

// splitOuterSelect returns the (projection-list, rest) split of a
// `SELECT <projs> FROM ...` query. If the query doesn't start with
// SELECT or the FROM is missing at depth 0, returns ("", "").
func splitOuterSelect(query string) (head string, tail string) {
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
// Quoted strings (single-quotes, backticks) shield commas. The
// returned slices have leading/trailing whitespace trimmed.
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

// splitAlias separates `<expr> AS \`alias\`` into (expr, alias). When
// no AS clause is present returns (s, "").
func splitAlias(s string) (expr, alias string) {
	// Find the last depth-0 " AS " (case-insensitive). Backtick-
	// quoted "AS" is shielded.
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

// extractProjectionCount counts top-level SELECT projections by
// re-splitting the outer SELECT's projection list on depth-0 commas.
// Used to size the scan-target slice without calling
// rows.ColumnTypes() (which panics on Map columns per the chDB probe).
func extractProjectionCount(query string) int {
	head, _ := splitOuterSelect(query)
	if head == "" {
		return 0
	}
	return len(splitProjections(head))
}

// openChDB returns a fresh ephemeral chDB session. The empty DSN
// triggers a temp-dir-backed session that's torn down with the
// connection — there is no `:memory:` literal in chdb-go.
func openChDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	return db
}

// applySeed splits a multi-statement script on top-level semicolons
// and exec's each piece. Statements wrapped in single-quoted strings
// keep their semicolons literal (handled by a tiny state machine).
//
// Cross-fixture isolation: chdb-go shares one engine across a process,
// so bare `CREATE TABLE foo` from a prior fixture survives to clash
// with the next. The applier promotes bare `CREATE TABLE` to
// `CREATE OR REPLACE TABLE` so re-running a fixture in the same
// process is idempotent. Fixture authors who want strict CH semantics
// can opt out by writing `CREATE OR REPLACE TABLE` /
// `CREATE TABLE IF NOT EXISTS` themselves.
func applySeed(t *testing.T, db *sql.DB, seed string) {
	t.Helper()
	for _, stmt := range splitStatements(seed) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		stmt = promoteCreateTable(stmt)
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed exec failed:\n--- stmt ---\n%s\n--- err ---\n%v", stmt, err)
		}
	}
}

// promoteCreateTable rewrites a bare `CREATE TABLE …` statement to
// `CREATE OR REPLACE TABLE …` so re-running a seed against a chDB
// session that already holds the table is idempotent. Other variants
// (`CREATE OR REPLACE TABLE`, `CREATE TABLE IF NOT EXISTS`,
// `CREATE TEMPORARY TABLE`) are left untouched.
func promoteCreateTable(stmt string) string {
	trimmed := strings.TrimLeft(stmt, " \t\n\r")
	prefix := stmt[:len(stmt)-len(trimmed)]
	upper := strings.ToUpper(trimmed)
	const needle = "CREATE TABLE "
	if !strings.HasPrefix(upper, needle) {
		return stmt
	}
	rest := trimmed[len(needle):]
	return prefix + "CREATE OR REPLACE TABLE " + rest
}

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

// RunRoundTrip executes a fixture's seed + rewritten SQL against an
// ephemeral chDB session and asserts the resulting rows match
// `expected_rows:`. Caller passes the loaded fixture; if it's not a
// round-trip fixture, the call is a no-op.
//
// Determinism contract: the fixture's `sql:` (or the seed's INSERT
// ordering combined with a server-side ORDER BY in the emitted SQL)
// MUST produce a stable row order — RunRoundTrip does NOT sort rows.
// Map column comparison uses reflect.DeepEqual on map[string]any so
// JSON key ordering is irrelevant.
func RunRoundTrip(t *testing.T, c *Case) {
	t.Helper()
	rt, err := LoadRoundTrip(c)
	if err != nil {
		t.Fatalf("LoadRoundTrip: %v", err)
	}
	if !rt.IsRoundTrip() {
		return
	}
	if strings.TrimSpace(rt.SQL) == "" {
		t.Fatalf("fixture %s has seed/expected_rows but missing sql section", c.Name)
	}

	db := openChDB(t)
	applySeed(t, db, rt.Seed)

	query := rewriteMapProjections(rt.SQL)
	colCount := extractProjectionCount(query)
	if colCount == 0 {
		t.Fatalf("fixture %s: cannot determine SELECT projection count from sql", c.Name)
	}

	rows, err := db.Query(query, rt.Args...)
	if err != nil {
		t.Fatalf("query failed:\n--- query ---\n%s\n--- args ---\n%#v\n--- err ---\n%v",
			query, rt.Args, err)
	}
	defer func() { _ = rows.Close() }()

	got := make([][]any, 0, len(rt.ExpectedRows))
	for rows.Next() {
		// Scan into *interface{} so we receive the chdb-go driver's
		// native Go value (string, int64, float64, time.Time, []byte)
		// per chdb/driver/parquet.go's switch table. This sidesteps
		// rows.ColumnTypes() (which panics on Map columns per the
		// chDB driver probe).
		cells := make([]any, colCount)
		ptrs := make([]any, colCount)
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		row := make([]any, colCount)
		for i, v := range cells {
			row[i] = decodeCell(v)
		}
		got = append(got, row)
	}
	if err := tolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	// Coerce expected rows in-place: JSON numbers always decode as
	// float64; the runner normalizes the actual scan output through
	// the same lens so DeepEqual is symmetric. Same for Map columns
	// (already decoded as map[string]any on the got side).
	want := normalizeExpected(rt.ExpectedRows)
	gotNorm := normalizeExpected(got)

	if !reflect.DeepEqual(gotNorm, want) {
		t.Fatalf("round-trip mismatch (fixture %s)\n got = %s\nwant = %s",
			c.Name, mustJSON(gotNorm), mustJSON(want))
	}
}

// decodeCell turns a chdb-go driver-native value into the Go value
// used for comparison. The driver hands back time.Time, int64,
// float64, bool, string, []byte — see chdb/driver/parquet.go's
// switch table.
//
// For Map columns we wrapped server-side in toJSONString(...), the
// driver returns a string; we try JSON-decode and fall back to the
// raw string. time.Time values are normalized to RFC3339Nano so
// fixture authors can write them as quoted strings.
func decodeCell(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		return decodeBytes(x)
	case string:
		return decodeString(x)
	default:
		return v
	}
}

func decodeBytes(b []byte) any {
	return decodeString(string(b))
}

func decodeString(s string) any {
	trim := strings.TrimSpace(s)
	if len(trim) > 0 && (trim[0] == '{' || trim[0] == '[') {
		var v any
		if err := json.Unmarshal([]byte(trim), &v); err == nil {
			return v
		}
	}
	return s
}

// normalizeExpected walks a [][]any and coerces numeric cells to
// float64 so JSON-decoded numbers compare equal to scanned values.
func normalizeExpected(rows [][]any) [][]any {
	out := make([][]any, len(rows))
	for i, row := range rows {
		nr := make([]any, len(row))
		for j, v := range row {
			nr[j] = normalizeValue(v)
		}
		out[i] = nr
	}
	return out
}

func normalizeValue(v any) any {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case float32:
		return float64(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = normalizeValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = normalizeValue(vv)
		}
		return out
	default:
		return v
	}
}

func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("<json err: %v>", err)
	}
	return string(b)
}

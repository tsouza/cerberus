//go:build chdb

package property

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

// chdbEOFSentinel is the spurious end-of-iteration error chdb-go's
// parquet driver returns instead of io.EOF (see chdb-go v1.11.0
// `parquet.go`: `return fmt.Errorf("empty row")`). Replicated here so
// the property runner doesn't depend on test/spec internals.
const chdbEOFSentinel = "empty row"

// tolerantRowsErr mirrors the helper used by spec/runner_chdb.go.
func tolerantRowsErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), chdbEOFSentinel) {
		return nil
	}
	return err
}

// openChDB returns a fresh ephemeral chDB session bound to t's
// lifetime. Replicated from test/spec/runner_chdb.go; kept local so
// test/property doesn't import test/spec.
func openChDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("property: open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("property: ping chdb: %v", err)
	}
	return db
}

// applyDDL splits a multi-statement seed script on top-level
// semicolons and exec's each piece. Single-quoted strings shield
// embedded semicolons. Bare `CREATE TABLE` is promoted to
// `CREATE OR REPLACE TABLE` so re-running the property test in the
// same process is idempotent.
//
// Same logic as test/spec/runner_chdb.go's applySeed; replicated
// here so test/property has no dependency on test/spec internals.
func applyDDL(t *testing.T, db *sql.DB, ddl string) {
	t.Helper()
	for _, stmt := range splitStatements(ddl) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		stmt = promoteCreateTable(stmt)
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("property: ddl exec failed:\n--- stmt ---\n%s\n--- err ---\n%v", stmt, err)
		}
	}
}

// promoteCreateTable rewrites a bare `CREATE TABLE …` to
// `CREATE OR REPLACE TABLE …`. Other variants are left untouched.
// Mirrors test/spec/runner_chdb.go.
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

// splitStatements splits a multi-statement script on top-level
// semicolons. Single-quoted strings (with simple backslash escapes)
// shield embedded semicolons. Mirrors test/spec/runner_chdb.go.
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

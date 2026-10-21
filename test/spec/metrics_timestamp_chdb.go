//go:build chdb

package spec

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chdbsession"
	"github.com/tsouza/cerberus/internal/testsql"
)

// RunMetricsTimestampDifferential executes one fixture's statement over the
// two OTel-CH metrics timestamp schemas and requires the same rows from
// both.
//
// The fixture's seed is applied once, then copied into two databases that
// hold the SAME whole-second samples and differ only in the physical type
// of the metric tables' timestamp columns:
//
//   - the DateTime64 world keeps each timestamp column's seeded type, with
//     every value truncated to its whole second;
//   - the DateTime world types those columns `DateTime` (Array(DateTime) for
//     a Nested exemplar timestamp), the OTel-CH exporter's metrics schema
//     from v0.157.
//
// Both worlds take their values through the same DateTime conversion, so a
// value the DateTime type cannot hold lands identically in both. dateTime64
// runs in the DateTime64 world and dateTime — the same plan emitted under
// chsql.MetricsTimestampDateTime — in the DateTime world. Any difference in
// the decoded rows fails the fixture, as does a statement ClickHouse
// rejects in one world but not the other, or rejects with a different
// error code. Truncation can make a fixture's data invalid for the query —
// two samples a fraction of a second apart become a duplicate sample the
// statement's own throwIf rejects — and then both worlds raise the same
// error, which is agreement.
//
// A fixture that does not opt into round-trip execution is a no-op, as it
// is for RunRoundTripSQL.
func RunMetricsTimestampDifferential(t *testing.T, c *Case, dateTime64, dateTime Statement) {
	t.Helper()
	rt, err := LoadRoundTrip(c)
	if err != nil {
		t.Fatalf("LoadRoundTrip: %v", err)
	}
	if !rt.IsRoundTrip() {
		return
	}

	chdbEngineMu.Lock()
	defer chdbEngineMu.Unlock()

	db := OpenChDB(t)
	ApplySeed(t, db, rt.Seed)
	src := currentChDBDatabase(t, db)

	worlds := []struct {
		suffix  string
		convert func(col, typ string) string
		stmt    Statement
	}{
		{suffix: "_dt64", convert: truncatedToSecondsAs, stmt: dateTime64},
		{suffix: "_dt", convert: asDateTime, stmt: dateTime},
	}
	results := make([][][]any, len(worlds))
	errs := make([]error, len(worlds))
	for i, w := range worlds {
		dst := src + w.suffix
		copySeedDatabase(t, db, src, dst, w.convert)
		results[i], errs[i] = queryInDatabase(t, db, rt, dst, w.stmt)
		mustExecChDB(t, db, "USE "+src)
		mustExecChDB(t, db, "DROP DATABASE IF EXISTS "+dst)
	}
	if errs[0] != nil || errs[1] != nil {
		code0, code1 := clickHouseErrorCode(errs[0]), clickHouseErrorCode(errs[1])
		if errs[0] == nil || errs[1] == nil || code0 == "" || code0 != code1 {
			t.Fatalf("metrics timestamp schemas disagree (fixture %s)\nDateTime64 error = %v\n  DateTime error = %v",
				c.Name, errs[0], errs[1])
		}
		return
	}
	if !reflect.DeepEqual(results[0], results[1]) {
		t.Fatalf("metrics timestamp schemas disagree (fixture %s)\nDateTime64 = %s\n  DateTime = %s",
			c.Name, mustJSON(results[0]), mustJSON(results[1]))
	}
}

// currentChDBDatabase names the database the shared chDB session is using.
func currentChDBDatabase(t *testing.T, db *sql.DB) string {
	t.Helper()
	var name string
	if err := db.QueryRow("SELECT currentDatabase()").Scan(&name); err != nil {
		t.Fatalf("currentDatabase: %v", err)
	}
	return name
}

// mustExecChDB executes one statement on the shared chDB session.
func mustExecChDB(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

// copySeedDatabase recreates every table of src in a fresh database dst,
// passing each metric-table timestamp column (a DateTime64 column, or an
// Array of one) through convert and every other column — and every column
// of a fixture's own helper tables — through unchanged.
func copySeedDatabase(t *testing.T, db *sql.DB, src, dst string, convert func(col, typ string) string) {
	t.Helper()
	mustExecChDB(t, db, "DROP DATABASE IF EXISTS "+dst)
	mustExecChDB(t, db, "CREATE DATABASE "+dst)
	for _, table := range seedTables(t, db, src) {
		cols := seedColumns(t, db, src, table)
		exprs := make([]string, len(cols))
		for i, col := range cols {
			exprs[i] = quoteIdent(col.name)
			if strings.HasPrefix(table, testsql.MetricsTablePrefix) && isDateTime64Type(col.typ) {
				exprs[i] = convert(quoteIdent(col.name), col.typ) + " AS " + quoteIdent(col.name)
			}
		}
		mustExecChDB(t, db, "CREATE TABLE "+dst+"."+quoteIdent(table)+
			" ENGINE = MergeTree ORDER BY tuple() AS SELECT "+strings.Join(exprs, ", ")+
			" FROM "+src+"."+quoteIdent(table))
	}
}

// seedColumn is one column of a seeded table, as system.columns reports it.
type seedColumn struct {
	name string
	typ  string
}

// seedTables lists the tables of database.
func seedTables(t *testing.T, db *sql.DB, database string) []string {
	t.Helper()
	rows, err := db.Query("SELECT name FROM system.tables WHERE database = ? ORDER BY name", database)
	if err != nil {
		t.Fatalf("list tables of %s: %v", database, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		out = append(out, name)
	}
	return out
}

// seedColumns lists table's columns in declaration order.
func seedColumns(t *testing.T, db *sql.DB, database, table string) []seedColumn {
	t.Helper()
	rows, err := db.Query("SELECT name, type FROM system.columns WHERE database = ? AND table = ? ORDER BY position", database, table)
	if err != nil {
		t.Fatalf("list columns of %s.%s: %v", database, table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []seedColumn
	for rows.Next() {
		var c seedColumn
		if err := rows.Scan(&c.name, &c.typ); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		out = append(out, c)
	}
	return out
}

// isDateTime64Type reports whether typ is a DateTime64 or an Array of one —
// the two shapes an OTel-CH metrics timestamp column takes (the Nested
// exemplar timestamp is flattened to an Array).
func isDateTime64Type(typ string) bool {
	return strings.HasPrefix(typ, "DateTime64(") || strings.HasPrefix(typ, "Array(DateTime64(")
}

// isArrayType reports whether typ is an Array type.
func isArrayType(typ string) bool { return strings.HasPrefix(typ, "Array(") }

// asDateTime renders col converted to the DateTime schema's type.
func asDateTime(col, typ string) string {
	if isArrayType(typ) {
		return "arrayMap(x -> CAST(x AS DateTime), " + col + ")"
	}
	return "CAST(" + col + " AS DateTime)"
}

// truncatedToSecondsAs renders col converted through DateTime and back to
// its own type typ, so it holds exactly the values asDateTime yields.
func truncatedToSecondsAs(col, typ string) string {
	return "CAST(" + asDateTime(col, typ) + " AS " + typ + ")"
}

// quoteIdent backquotes a ClickHouse identifier.
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "\\`") + "`"
}

// clickHouseErrorCodePattern extracts the numeric code of a ClickHouse
// exception ("Code: 395. DB::Exception: …").
var clickHouseErrorCodePattern = regexp.MustCompile(`Code: (\d+)\.`)

// clickHouseErrorCode returns err's ClickHouse exception code, or "" when
// err is nil or carries none.
func clickHouseErrorCode(err error) string {
	if err == nil {
		return ""
	}
	m := clickHouseErrorCodePattern.FindStringSubmatch(err.Error())
	if m == nil {
		return ""
	}
	return m[1]
}

// queryInDatabase runs stmt through the round-trip prep pipeline in
// database and returns its decoded, normalised, sorted rows, or the error
// ClickHouse rejected it with.
func queryInDatabase(t *testing.T, db *sql.DB, rt *RoundTripSections, database string, stmt Statement) ([][]any, error) {
	t.Helper()
	mustExecChDB(t, db, "USE "+database)
	query, args, colCount := prepareSeededQuery(stmt.SQL, stmt.Args, rt.Seed)
	rows, err := chdbsession.SafeQuery(context.Background(), db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query failed in %s:\n--- query ---\n%s\n--- args ---\n%#v\n--- err ---\n%w", database, query, args, err)
	}
	defer func() { _ = rows.Close() }()
	if colCount == 0 {
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("rows.Columns: %v", err)
		}
		colCount = len(cols)
	}
	got := normalizeExpected(scanDecodedRows(t, rows, colCount, rt.RawStrings))
	sortRows(got)
	return got, nil
}

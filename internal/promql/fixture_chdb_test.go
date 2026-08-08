//go:build chdb

// Shared chDB fixture plumbing for the package's chdb-tagged parity
// tests, and the structural guard that keeps them independent of each
// other.
//
// Every `sql.Open("chdb", "")` in this process resolves to ONE in-process
// ClickHouse session, so the whole package shares a single catalog. That
// makes each fixture's seed visible to every other fixture's query, and
// the metrics read path scans a `merge(currentDatabase(), '^(…)$')`
// fan-out whose column set is the UNION of whatever tables match. A
// fixture that seeds only some of the arms therefore still passes in a
// whole-package run — and fails on its own, with an UNKNOWN_IDENTIFIER
// that reads exactly like a real SQL-lowering bug, under the narrowed
// `go test -run '^TestName$'` invocation the repo mandates for
// reproducing a red check.
//
// [chdbFixture] closes that hole by construction rather than by
// convention: it binds the session to the seed script that populated it,
// and every emitted statement executed through it is first checked
// against that seed alone ([testsql.CheckSeedCoversFanOut]). A fixture
// that leans on a sibling's tables fails immediately, with the arm and
// the column named, instead of passing until someone narrows the run.
package promql_test

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/testsql"
)

// chdbFixture pairs the process-shared chDB session with the seed script
// that is allowed to satisfy the queries run against it.
type chdbFixture struct {
	db   *sql.DB
	seed string
}

// newChDBFixture opens the shared in-process chDB session and applies
// seed to it. Seeds spell their tables `CREATE OR REPLACE TABLE` — the
// session outlives any one test, so a bare `CREATE TABLE` would trip
// TABLE_ALREADY_EXISTS on the second fixture to declare the same table.
func newChDBFixture(t *testing.T, seed string) *chdbFixture {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	for _, stmt := range testsql.SplitStatements(seed) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	return &chdbFixture{db: db, seed: seed}
}

// queryOverEmitted runs `SELECT <projection> FROM (<emitted>)` and
// returns the rows for the caller to close.
//
// The wrap exists because chdb-go's parquet driver cannot decode a `Map`
// cell into a Go destination, so the sort-key / label values are read out
// as plain String columns over the emitted (ORDER BY-bearing) statement,
// preserving the row order the inner query produced.
//
// Before executing anything it asserts the fixture's own seed is
// self-sufficient for the emitted fan-out. That assertion is what makes a
// narrowed single-test run mean the same thing as a whole-package run.
func (f *chdbFixture) queryOverEmitted(t *testing.T, projection, emitted string, args []any) *sql.Rows {
	t.Helper()
	if err := testsql.CheckSeedCoversFanOut(f.seed, emitted); err != nil {
		t.Fatalf("this fixture's seed does not stand on its own — it would pass only while a sibling fixture's tables happen to be present in the shared chDB session: %v\nemitted SQL: %s", err, emitted)
	}
	wrapped := "SELECT " + projection + " FROM (" + emitted + ")"
	rows, err := f.db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, wrapped)
	}
	return rows
}

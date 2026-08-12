//go:build chdb

package chsqltest

import (
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver" // registers the "chdb" sql driver
)

// isolatedDBSeq disambiguates two tests whose names fold to the same
// identifier. Test names are already distinct, so this only guards
// isolatedDBName's lossy character folding.
var isolatedDBSeq atomic.Uint64

// defaultChDBDatabase is the database the shared chDB session starts in, and
// the one OpenIsolatedChDB returns it to on cleanup so no test ever leaves the
// session pointing at a database that has been dropped.
const defaultChDBDatabase = "default"

// OpenIsolatedChDB opens the chDB session and switches it to a database
// private to t, dropping that database when t finishes.
//
// The isolation is the point; see the package doc for why sharing `default`
// coupled unrelated tests together. Callers get an ordinary *sql.DB whose
// current database is their own, so emitted SQL that resolves tables through
// currentDatabase() sees only what this test seeded.
//
// The switch is process-global, exactly as chdb-go's cached session is, so
// this is sound only while the chDB tests run serially. None of them calls
// t.Parallel, and test/regression/chsql_chdb_isolation_test.go pins that.
func OpenIsolatedChDB(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}

	name := isolatedDBName(t.Name())
	// A run that died before its cleanup leaves the database behind in a
	// reused chDB data directory; recovering costs one statement.
	if _, err := db.Exec("DROP DATABASE IF EXISTS " + name); err != nil {
		t.Fatalf("drop stale database %s: %v", name, err)
	}
	if _, err := db.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	if _, err := db.Exec("USE " + name); err != nil {
		t.Fatalf("use database %s: %v", name, err)
	}

	t.Cleanup(func() {
		// Leave the isolated database before dropping it: dropping the
		// CURRENT database leaves the shared session pointing at one that no
		// longer exists, which breaks the NEXT test rather than this one.
		if _, err := db.Exec("USE " + defaultChDBDatabase); err != nil {
			t.Errorf("restore %s database: %v", defaultChDBDatabase, err)
		}
		if _, err := db.Exec("DROP DATABASE IF EXISTS " + name); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
		_ = db.Close()
	})
	return db
}

// isolatedDBName folds a test name into a ClickHouse identifier. Test names
// carry `/` for subtests and may carry any rune a t.Run label contains, so
// everything outside [A-Za-z0-9_] folds to `_`.
func isolatedDBName(testName string) string {
	var b strings.Builder
	b.WriteString("chsql_test_")
	for _, r := range testName {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return fmt.Sprintf("%s_%d", b.String(), isolatedDBSeq.Add(1))
}

// MetricsSeedDDL renders the OTel-CH metric-table shape internal/chsql's chDB
// round-trip tests seed. It is ONE definition rather than a copy per test file
// because the read path decides which columns a selector projects, and a
// column added there has to reach every seed at once: `ServiceName` was added
// to the selector's label projection and eight identical inline CREATEs each
// had to learn about it, which is exactly the drift a shared renderer removes.
// A ninth seed that never adopted it — a hand-rolled three-column
// `otel_metrics_sum` — is what #2074 was.
//
// `ResourceAttributes` and `ServiceName` carry DEFAULTs so the tests' explicit
// INSERT column lists stay short — the read path only needs them to RESOLVE,
// not to be populated.
func MetricsSeedDDL(table string) string {
	return fmt.Sprintf(`
CREATE OR REPLACE TABLE %s (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
`, table)
}

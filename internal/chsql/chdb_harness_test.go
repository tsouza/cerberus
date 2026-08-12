//go:build chdb

// Shared harness for this package's chDB round-trip tests: one isolated
// database per test, and one definition of the metric-table seed shape.
//
// It lives in the INTERNAL `package chsql` test namespace, with exported
// names, because the package's chDB tests straddle both test packages:
// range_window_fused_chdb_test.go is `package chsql` (it drives the
// unexported `emitter` directly), every other chDB test is `package
// chsql_test`. An identifier declared here is visible unqualified to the
// former and as `chsql.X` to the latter, which is the only arrangement that
// gives BOTH a single shared definition. Nothing here reaches a non-test
// build: `_test.go` files are never linked into the binary.

package chsql

import (
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver" // registers the "chdb" sql driver
)

// isolatedDBSeq disambiguates two tests whose names sanitise to the same
// identifier. Test names are already distinct, so this only guards the
// sanitiser's lossy character folding.
var isolatedDBSeq atomic.Uint64

// OpenIsolatedChDB opens the chDB session and switches it to a database
// private to t, dropping that database when t finishes.
//
// The isolation is the point. chdb-go caches ONE session per process
// (#1987), so every `sql.Open("chdb", "")` in this package's tests — whatever
// handle it returns — lands in the same engine and, without this, the same
// `default` database. Table DDL therefore leaked from each test into every
// later one, and the metric read path is acutely sensitive to that: a
// selector emits `merge(currentDatabase(), '^(otel_metrics_gauge|...)$')`,
// and ClickHouse requires a referenced column to exist in EVERY table the
// regex matches. One test seeding an `otel_metrics_*` table with a different
// column set therefore broke an unrelated later test (#2074). Per-test
// databases make that structurally impossible rather than a rule each new
// seed has to remember, because `currentDatabase()` resolves to the caller's
// own database and the regex can only ever match the caller's own tables.
//
// The switch is process-global, exactly as the session is, so this is safe
// only while the chDB tests run serially — none of them calls t.Parallel, and
// test/regression pins that they must not.
func OpenIsolatedChDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}

	name := isolatedDBName(t.Name())
	// A previous run that died before its cleanup leaves the database behind;
	// the session outlives a single test binary only in a developer's reused
	// chDB data directory, but recovering from it costs one statement.
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

// defaultChDBDatabase is the database the shared chDB session starts in and
// the one OpenIsolatedChDB returns it to, so no test ever leaves the session
// pointing at a dropped database.
const defaultChDBDatabase = "default"

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

// MetricsSeedDDL renders the OTel-CH metric-table shape every chDB round-trip
// test in this package seeds. It is ONE definition rather than a copy per test
// file because the read path decides which columns a selector projects, and a
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

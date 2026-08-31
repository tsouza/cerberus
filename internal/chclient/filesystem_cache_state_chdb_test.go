//go:build chdb

package chclient

import (
	"database/sql"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

// TestFilesystemCacheStateSQL_NoCacheConfigured pins filesystemCacheStateSQL's
// all-zero baseline (cerberus issue #2780) against a real ClickHouse engine:
// chDB's embedded engine carries no storage_configuration and therefore no
// named filesystem cache at all, so system.filesystem_cache_settings is
// empty and the query must decode to a single row of zeros — never an
// error, since an aggregate over zero input rows is a well-formed one-row
// result (count()=0, every sum()=0, never NULL — see the SQL constant's own
// doc), not an empty result set QueryFilesystemCacheState would have to
// special-case.
//
// Client wraps clickhouse-go/v2's native-protocol driver, which cannot dial
// chDB's embedded engine, so this test runs the exact SQL constant over
// database/sql + the chdb-go driver directly — the same shape-verification
// pattern internal/chsql's *_chdb_test.go files use to validate emitted SQL
// against a real engine without going through the production client.
func TestFilesystemCacheStateSQL_NoCacheConfigured(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}

	var got FilesystemCacheState
	err = db.QueryRow(filesystemCacheStateSQL).
		Scan(&got.Caches, &got.MaxSizeBytes, &got.MaxElements, &got.CurrentSizeBytes, &got.CurrentElements)
	if err != nil {
		t.Fatalf("query filesystemCacheStateSQL: %v", err)
	}
	got.Configured = got.Caches > 0

	want := FilesystemCacheState{}
	if got != want {
		t.Fatalf("filesystemCacheStateSQL on a cache-less server = %+v; want %+v", got, want)
	}
}

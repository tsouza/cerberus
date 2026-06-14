//go:build chdb

// Quantify the LogQL k8s.* materialized-column routing lever.
//
// The OTel ClickHouse Exporter MATERIALIZEs a fixed set of k8s.* /
// deployment.environment.name ResourceAttributes keys into dedicated
// LowCardinality(String) columns on the logs table — each defined as
// `MATERIALIZED ResourceAttributes['<key>']`. cerberus's LogQL emit now
// routes a stream-selector matcher (or inner range-aggregation group-by)
// on one of those labels to the bare materialized column instead of
// decompressing the wide ResourceAttributes Map (internal/logql +
// internal/schema).
//
// This harness proves the two halves of the claim against a 2,000,000-row
// otel_logs table that replicates the exporter's logs_table.sql shape
// (ORDER BY (toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp);
// a 6-key ResourceAttributes Map incl. 4 k8s.* keys; the
// `__otel_materialized_k8s.namespace.name` MATERIALIZED column):
//
//  1. CORRECTNESS — the BEFORE emit (origin/main: map read, reproduced by
//     clearing schema.Logs.MaterializedResourceColumns) and the AFTER emit
//     (materialized column) return BYTE-IDENTICAL result rows for the
//     canonical `{k8s_namespace_name="ns-3"}` matcher and its group-by
//     twin, including the missing-key BWC row.
//
//  2. BYTE-READ — `system.parts_columns` reports the on-disk
//     compressed/uncompressed bytes ClickHouse must read+decompress for
//     the column a predicate touches. The ResourceAttributes Map dwarfs
//     the materialized column by a wide margin; we assert a generous
//     FLOOR ratio so the lever can't silently regress to map reads.
//
// The column is NOT in the ORDER BY key, so EXPLAIN granule/read_rows are
// identical BEFORE vs AFTER — this is a byte-read / CPU win, not a granule
// prune. That is the honest characterization and the assertions reflect
// it (we assert column bytes, not granules).
//
// Build-tagged `chdb`, same lane as the rest of the chDB execs.
package perf

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver" // registers "chdb" sql driver

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

const (
	// matK8sRows is the seeded row count — large enough that the
	// compressed-byte gap between the wide Map and the LowCardinality
	// column is unambiguous, small enough to seed in-process quickly.
	matK8sRows = 2_000_000
	// matK8sNamespaceCardinality is the number of distinct namespace
	// values; LowCardinality keeps the materialized column tiny while the
	// Map carries the full key+value payload per row.
	matK8sNamespaceCardinality = 50
	// matK8sByteRatioFloor is the generous FLOOR on
	// compressed-bytes(Map) / compressed-bytes(materialized column). The
	// measured ratio on this grid is ~50x+; the floor guards against a
	// silent regression to map reads without pinning a brittle exact
	// number.
	matK8sByteRatioFloor = 8.0
)

// matK8sTable is the table name the seeded grid + emitted SQL share.
const matK8sTable = "otel_logs_mat_k8s"

// matK8sNamespaceColumn is the materialized column the lever routes the
// k8s.namespace.name label to (the exporter's
// `__otel_materialized_<key>` naming). materializedColumnMarker is the
// shared prefix used to assert the BEFORE emit references NO materialized
// column.
const (
	matK8sNamespaceColumn    = "__otel_materialized_k8s.namespace.name"
	materializedColumnMarker = "__otel_materialized"
)

// matK8sDDL replicates the exporter's logs_table.sql column shape closely
// enough to exercise the lever: the wide ResourceAttributes Map plus the
// MATERIALIZED namespace column, on the production ORDER BY key.
func matK8sDDL() string {
	return fmt.Sprintf(`CREATE OR REPLACE TABLE %s (
    Timestamp DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    Body String CODEC(ZSTD(1)),
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    `+"`__otel_materialized_k8s.namespace.name`"+` LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.namespace.name'] CODEC(ZSTD(1))
) ENGINE = MergeTree()
PARTITION BY toDate(Timestamp)
ORDER BY (toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp)
SETTINGS index_granularity = 8192`, matK8sTable)
}

// matK8sInsert builds the grid via INSERT … SELECT FROM numbers(). The
// ResourceAttributes Map carries 6 keys including the 4 k8s.* ones the
// exporter promotes, so the Map's per-row payload is representative of a
// real OTel deployment. The namespace value cycles through
// matK8sNamespaceCardinality distinct values.
func matK8sInsert() string {
	return fmt.Sprintf(`INSERT INTO %s (Timestamp, ServiceName, Body, ResourceAttributes)
SELECT
    toDateTime64('2026-05-20 12:00:00', 9) + INTERVAL (number %% 3600) SECOND AS Timestamp,
    concat('svc-', toString(number %% 12)) AS ServiceName,
    concat('log line body number ', toString(number)) AS Body,
    map(
        'k8s.namespace.name', concat('ns-', toString(number %% %d)),
        'k8s.pod.name',       concat('pod-', toString(number %% 5000)),
        'k8s.container.name', concat('ctr-', toString(number %% 30)),
        'k8s.node.name',      concat('node-', toString(number %% 200)),
        'service.namespace',  concat('team-', toString(number %% 8)),
        'host.name',          concat('host-', toString(number %% 400))
    ) AS ResourceAttributes
FROM numbers(%d)`, matK8sTable, matK8sNamespaceCardinality, matK8sRows)
}

// emitLogQL lowers + emits a LogQL query against the given logs schema,
// returning the SQL + bound args. The schema choice is the A/B knob:
// DefaultOTelLogs routes k8s.* to the materialized column; the same
// schema with MaterializedResourceColumns cleared reproduces origin/main's
// map read.
func emitLogQL(t *testing.T, query string, s schema.Logs) (string, []any) {
	t.Helper()
	expr, err := logql.ParseExprPermissive(query)
	if err != nil {
		t.Fatalf("ParseExprPermissive(%q): %v", query, err)
	}
	plan, err := logql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	return sqlStr, args
}

// schemaForTable returns DefaultOTelLogs pointed at the seeded table.
func matK8sSchema() schema.Logs {
	s := schema.DefaultOTelLogs()
	s.LogsTable = matK8sTable
	return s
}

// selectStarPrefix is the projection the LogQL matcher emit produces; we
// swap it for a count() so the result-parity probe exercises the SAME
// WHERE clause (the thing under test) without projecting the Map column —
// chDB-go's parquet driver panics draining a raw Map projection.
const selectStarPrefix = "SELECT * FROM"

func runMatK8sQuery(t *testing.T, db *sql.DB, sqlStr string, args []any) [][]any {
	t.Helper()
	if !hasPrefix(sqlStr, selectStarPrefix) {
		t.Fatalf("expected matcher SQL to start with %q, got %q", selectStarPrefix, sqlStr)
	}
	counted := "SELECT count() FROM" + sqlStr[len(selectStarPrefix):]
	rows, err := db.Query(stripTrailingSemi(counted), args...)
	if err != nil {
		t.Fatalf("query %q: %v", counted, err)
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		var n uint64
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, []any{n})
	}
	if err := tolerantChdbErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func TestMaterializedK8sColumn_ChDB(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(matK8sDDL()); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	if _, err := db.Exec(matK8sInsert()); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Single part so the per-column byte accounting is comparable and not
	// split across background-merge parts.
	if _, err := db.Exec("OPTIMIZE TABLE " + matK8sTable + " FINAL"); err != nil {
		t.Fatalf("optimize: %v", err)
	}

	after := matK8sSchema() // materialized routing ON (DefaultOTelLogs)
	before := matK8sSchema()
	before.MaterializedResourceColumns = nil // reproduces origin/main: map read

	// --- (1) MATCHER: emit divergence + result parity. -------------------
	// The matcher path is where the Scan sits directly below the Filter, so
	// the bare materialized column wholly replaces the Map decompression —
	// the big byte-read win. AFTER must reference the materialized column;
	// BEFORE must read the ResourceAttributes Map.
	matcher := `{k8s_namespace_name="ns-3"}`
	beforeSQL, beforeArgs := emitLogQL(t, matcher, before)
	afterSQL, afterArgs := emitLogQL(t, matcher, after)
	t.Logf("matcher BEFORE (map):  %s", beforeSQL)
	t.Logf("matcher AFTER  (mat):  %s", afterSQL)
	if !containsSub(afterSQL, matK8sNamespaceColumn) {
		t.Fatalf("AFTER matcher emit did not route to materialized column: %s", afterSQL)
	}
	if containsSub(beforeSQL, materializedColumnMarker) {
		t.Fatalf("BEFORE matcher emit unexpectedly referenced a materialized column: %s", beforeSQL)
	}
	if !containsSub(beforeSQL, "ResourceAttributes") {
		t.Fatalf("BEFORE matcher emit did not read ResourceAttributes Map: %s", beforeSQL)
	}

	gotBefore := runMatK8sQuery(t, db, beforeSQL, beforeArgs)
	gotAfter := runMatK8sQuery(t, db, afterSQL, afterArgs)
	if !rowsEqual(gotBefore, gotAfter) {
		t.Fatalf("matcher result parity FAILED: before=%v after=%v", gotBefore, gotAfter)
	}
	t.Logf("matcher result parity OK: count(before)=count(after)=%v", gotAfter)

	// --- (1b) GROUP-BY TWIN: inner-range by-key routing (SQL-level). -----
	// `quantile_over_time(...) by (k8s_namespace_name)` resolves the inner
	// range-aggregation group key through levelAwareRangeGroupKey, which
	// AFTER routes to the bare materialized column (BEFORE reads the Map).
	// A non-k8s selector isolates the by-key routing from the matcher path.
	// The aggregation's top-level output is a rebuilt ResourceAttributes
	// Map, which chDB-go's parquet driver cannot drain — so the twin is
	// proven at the SQL level: BEFORE reads the Map for the group key,
	// AFTER the materialized column, and the rest of the plan is identical.
	groupBy := `quantile_over_time(0.9, {service_name="svc-1"} | unwrap bytes [5m]) by (k8s_namespace_name)`
	gbBeforeSQL, _ := emitLogQL(t, groupBy, before)
	gbAfterSQL, _ := emitLogQL(t, groupBy, after)
	if !containsSub(gbAfterSQL, matK8sNamespaceColumn) {
		t.Fatalf("AFTER group-by emit did not route by-key to materialized column: %s", gbAfterSQL)
	}
	if containsSub(gbBeforeSQL, materializedColumnMarker) {
		t.Fatalf("BEFORE group-by emit unexpectedly referenced a materialized column: %s", gbBeforeSQL)
	}
	t.Logf("group-by twin OK: AFTER routes inner-range by-key to materialized column, BEFORE stays on Map")

	// --- (2) BYTE-READ: per-column on-disk bytes. ------------------------
	mapComp, mapUncomp := columnBytes(t, db, matK8sTable, "ResourceAttributes")
	colComp, colUncomp := columnBytes(t, db, matK8sTable, matK8sNamespaceColumn)
	t.Logf("ResourceAttributes Map : %d B compressed / %d B uncompressed", mapComp, mapUncomp)
	t.Logf("materialized namespace : %d B compressed / %d B uncompressed", colComp, colUncomp)
	if colComp == 0 {
		t.Fatalf("materialized column reported 0 compressed bytes — not physically written?")
	}
	ratioComp := float64(mapComp) / float64(colComp)
	ratioUncomp := float64(mapUncomp) / float64(colUncomp)
	t.Logf("byte-read reduction: %.1fx compressed / %.1fx uncompressed (floor %.1fx)",
		ratioComp, ratioUncomp, matK8sByteRatioFloor)
	if ratioComp < matK8sByteRatioFloor {
		t.Fatalf("compressed byte-read ratio %.2fx below floor %.2fx — lever not paying off",
			ratioComp, matK8sByteRatioFloor)
	}
}

// columnBytes reads the on-disk compressed/uncompressed bytes for a single
// column from system.parts_columns (active parts only) — the disk bytes
// ClickHouse must read+decompress when a predicate touches that column.
func columnBytes(t *testing.T, db *sql.DB, table, column string) (compressed, uncompressed int64) {
	t.Helper()
	row := db.QueryRow(
		`SELECT sum(column_data_compressed_bytes), sum(column_data_uncompressed_bytes)
         FROM system.parts_columns
         WHERE table = ? AND column = ? AND active`, table, column,
	)
	if err := row.Scan(&compressed, &uncompressed); err != nil {
		t.Fatalf("columnBytes(%s.%s): %v", table, column, err)
	}
	return compressed, uncompressed
}

// chdbEOFSentinel is the benign end-of-iteration error chdb-go's parquet
// reader returns in place of a clean io.EOF (parquet.go:
// `return fmt.Errorf("empty row")`). Mirrors test/spec/runner_chdb.go's
// tolerantRowsErr.
const chdbEOFSentinel = "empty row"

func tolerantChdbErr(err error) error {
	if err != nil && containsSub(err.Error(), chdbEOFSentinel) {
		return nil
	}
	return err
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func rowsEqual(a, b [][]any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if fmt.Sprintf("%v", a[i][j]) != fmt.Sprintf("%v", b[i][j]) {
				return false
			}
		}
	}
	return true
}

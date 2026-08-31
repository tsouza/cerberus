//go:build chdb

// This is the real-ClickHouse proof (issue #2766) that the curated column
// statistics registry actually applies cleanly against a genuine server —
// not a text-parse of the rendered ALTER strings, which would prove nothing
// about whether the column/statistics-type pairing is valid ClickHouse DDL.
// It exists because the FIRST version of this registry was wrong: it paired
// `minmax` with every identity column, including the String /
// LowCardinality(String) ones (ServiceName, MetricName, SpanName, TraceId) —
// and a live probe against this same chDB substrate rejected every one of
// those with `Code: 708, ILLEGAL_STATISTICS` ("Statistics of type 'MinMax'
// does not support data type ... String"). renderMetricsColumnStatistics /
// renderLogsColumnStatistics / renderTracesColumnStatistics were corrected
// to split each table's ALTERs by column-type family (uniq-only for
// String-family columns; minmax+uniq[+tdigest] for numeric ones) — this test
// pins that correction against the real server so a future column addition
// that reintroduces the same mismatch fails here, not in a customer's boot
// log.
package ddl

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

// applyColumnStatisticsSignal renders the package's own real production DDL
// for signal (via the same renderSignal entry point every other chdb probe
// in this package uses) with ColumnStatisticsEnabled=true, and applies it
// against db — CREATE TABLE promoted to CREATE OR REPLACE (see
// promoteCreateTable) so each call starts from an empty table.
func applyColumnStatisticsSignal(t *testing.T, db *sql.DB, s Signal) (cfg Config, stmts []string) {
	t.Helper()
	cfg = Config{ColumnStatisticsEnabled: true}.withDefaults()
	stmts, err := renderSignal(cfg, s)
	if err != nil {
		t.Fatalf("renderSignal(%s): %v", s, err)
	}
	for _, stmt := range stmts {
		if strings.HasPrefix(stmt, "CREATE TABLE IF NOT EXISTS") {
			stmt = promoteCreateTable(stmt)
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("apply %s DDL:\n%s\nerr: %v", s, stmt, err)
		}
	}
	return cfg, stmts
}

// openColumnStatisticsChDB opens a fresh ephemeral chDB session — a real
// (embedded) ClickHouse server, not chdb's SQL-parsing-only mode — for the
// column-statistics probes below.
func openColumnStatisticsChDB(t *testing.T) *sql.DB {
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

// TestColumnStatistics_AppliesCleanly_AllSignals is the base case: the
// curated ADD STATISTICS registry for every signal (metrics, logs, traces)
// must apply without error against a real server — every column/statistics
// type pairing renderMetricsColumnStatistics / renderLogsColumnStatistics /
// renderTracesColumnStatistics choose must be one ClickHouse actually
// accepts for that column's declared type.
func TestColumnStatistics_AppliesCleanly_AllSignals(t *testing.T) {
	for _, s := range []Signal{Metrics, Logs, Traces} {
		t.Run(s.String(), func(t *testing.T) {
			db := openColumnStatisticsChDB(t)
			_, stmts := applyColumnStatisticsSignal(t, db, s)

			var sawStatistics bool
			for _, stmt := range stmts {
				if strings.Contains(stmt, "ADD STATISTICS") {
					sawStatistics = true
				}
			}
			if !sawStatistics {
				t.Fatalf("%s: expected at least one ADD STATISTICS statement, rendered:\n%v", s, stmts)
			}
		})
	}
}

// TestColumnStatistics_IdempotentReapply pins that re-applying the SAME
// curated ADD STATISTICS ALTERs a second time — the boot-every-time
// behavior applySignal exercises for every signal — is a genuine no-op
// against a real server (IF NOT EXISTS), not merely idempotent by
// construction of the rendered SQL text.
func TestColumnStatistics_IdempotentReapply(t *testing.T) {
	db := openColumnStatisticsChDB(t)
	cfg, stmts := applyColumnStatisticsSignal(t, db, Metrics)
	for _, stmt := range stmts {
		if !strings.Contains(stmt, "ADD STATISTICS") {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("second apply of %q: %v", stmt, err)
		}
	}
	_ = cfg
}

// TestColumnStatistics_MaterializeBackfillRunbook pins that the one-time
// `MATERIALIZE STATISTICS` backfill runbook documented in docs/operations.md
// (for a deployment that predates the feature) is real, executable SQL
// against a real server — not just prose. It targets the traces spans
// table's two curated ALTERs (the identity uniq ALTER and the Duration
// minmax/uniq/tdigest ALTER), since MATERIALIZE STATISTICS accepts every
// listed column regardless of which ADD STATISTICS ALTER declared it (see
// operations.md's note).
func TestColumnStatistics_MaterializeBackfillRunbook(t *testing.T) {
	db := openColumnStatisticsChDB(t)
	cfg, _ := applyColumnStatisticsSignal(t, db, Traces)

	// Seed one row so MATERIALIZE has a part to rewrite.
	if _, err := db.Exec(
		"INSERT INTO " + cfg.Tables.Traces + " (Timestamp, TraceId, SpanId, ServiceName, SpanName, Duration) " +
			"VALUES (now64(9), 'a1b2c3d4e5f60708a1b2c3d4e5f60708', 'a1b2c3d4e5f60708', 'probe-svc', 'probe-op', 12345)",
	); err != nil {
		t.Fatalf("seed traces row: %v", err)
	}
	if _, err := db.Exec("OPTIMIZE TABLE " + cfg.Tables.Traces + " FINAL"); err != nil {
		t.Fatalf("optimize final: %v", err)
	}

	materialize := "ALTER TABLE " + cfg.Tables.Traces +
		" MATERIALIZE STATISTICS ServiceName, SpanName, TraceId, Duration"
	if _, err := db.Exec(materialize); err != nil {
		t.Fatalf("materialize statistics backfill:\n%s\nerr: %v", materialize, err)
	}
}

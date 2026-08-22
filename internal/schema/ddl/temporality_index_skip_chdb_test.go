//go:build chdb

// This is the offline proof, against the package's own rendered production
// DDL, that the AggregationTemporality skip index renderAddTemporalityIndex
// installs (issue #2458) actually lets ClickHouse prune granules — not a
// text-parse of the ALTER string, which would prove nothing about query-time
// behavior. It mirrors trace_id_index_probe_chdb_test.go's methodology
// (live EXPLAIN indexes=1, not a hardcoded assertion) for the same reason:
// a skip index can be declared and still go unused if the WHERE predicate
// shape doesn't match it, or if the underlying data isn't granule-homogeneous
// enough for a minmax mark to prove anything.
package ddl

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

// temporalityProbeRowsPerGranule seeds enough rows to span several real
// index granules at the production otel_metrics_sum table's declared
// `SETTINGS index_granularity=8192` (see metrics_sum_table.sql), so the
// probe exercises the SAME granule size a real deployment would, rather
// than a scaled-down stand-in that might hide a granularity-dependent bug.
const temporalityProbeRowsPerGranule = 8192

// temporalityProbeGranules is the number of full granules
// temporalityProbeAllCumulativeRows seeds.
const temporalityProbeGranules = 4

// openMetricsProbeChDB renders + applies the package's own real production
// Metrics DDL (via the same renderSignal entry point TestRenderSignal_Metrics
// exercises) — CREATE TABLE, the curated projections, AND the
// AggregationTemporality skip index this issue adds — against a fresh
// ephemeral chDB session, never a hand-copied DDL string. Every CREATE TABLE
// IF NOT EXISTS is promoted to CREATE OR REPLACE TABLE so the probe starts
// from an empty table each run (see promoteCreateTable's doc comment for why
// that's safe here and wrong for a real cluster).
func openMetricsProbeChDB(t *testing.T) (db *sql.DB, cfg Config) {
	t.Helper()

	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}

	cfg = Config{}.withDefaults()
	stmts, err := renderSignal(cfg, Metrics)
	if err != nil {
		t.Fatalf("renderSignal(Metrics): %v", err)
	}
	for _, stmt := range stmts {
		if strings.HasPrefix(stmt, "CREATE TABLE IF NOT EXISTS") {
			stmt = promoteCreateTable(stmt)
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("apply DDL:\n%s\nerr: %v", stmt, err)
		}
	}
	return db, cfg
}

// temporalityProbeAllCumulativeRows returns the VALUES tuples for
// temporalityProbeGranules*temporalityProbeRowsPerGranule rows, all sharing
// ONE MetricName and ALL carrying CUMULATIVE (2) AggregationTemporality —
// the realistic, dominant real-world shape #2458 describes: a given series'
// AggregationTemporality is set once per exporter configuration, so its
// samples land in temporality-homogeneous runs almost always. TimeUnix
// increments by one second per row so ORDER BY (MetricName, Attributes,
// ServiceName, ...) does not need to reshuffle insertion order.
func temporalityProbeAllCumulativeRows(metricName string) string {
	var b strings.Builder
	n := temporalityProbeGranules * temporalityProbeRowsPerGranule
	for i := range n {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "('%s', map(), '', toDateTime64('2026-01-01 00:00:00', 9) + toIntervalSecond(%d), %d, 2, true)",
			metricName, i, i)
	}
	return b.String()
}

// TestTemporalityIndex_PrunesAllCumulativeGranules is the base case #2458
// exists to fix: a sum-table series whose every sample is CUMULATIVE (the
// overwhelmingly common real-world shape) must let ClickHouse prune EVERY
// granule for a DELTA-only predicate — the exact predicate
// NativeRateLowerer.LowerRate's fan-out (DELTA) arm issues — while the
// CUMULATIVE-only predicate the native arm issues must still read every
// granule (nothing to prune: every row matches it). Both properties are
// asserted from a live EXPLAIN indexes=1 plan, not a text-parse of the ALTER
// string.
func TestTemporalityIndex_PrunesAllCumulativeGranules(t *testing.T) {
	db, cfg := openMetricsProbeChDB(t)

	const metricName = "cerberus_queries_total"
	insert := "INSERT INTO " + cfg.Tables.MetricsSum +
		" (MetricName, Attributes, ServiceName, TimeUnix, Value, AggregationTemporality, IsMonotonic) VALUES " +
		temporalityProbeAllCumulativeRows(metricName)
	if _, err := db.Exec(insert); err != nil {
		t.Fatalf("seed all-cumulative rows: %v", err)
	}
	if _, err := db.Exec("OPTIMIZE TABLE " + cfg.Tables.MetricsSum + " FINAL"); err != nil {
		t.Fatalf("optimize final: %v", err)
	}

	deltaPlan := explainIndexes(t, db,
		"SELECT count() FROM "+cfg.Tables.MetricsSum+
			" WHERE MetricName = '"+metricName+"' AND AggregationTemporality = 1")
	if !strings.Contains(deltaPlan, temporalityIndexName) {
		t.Fatalf("DELTA-predicate EXPLAIN plan never mentions %s — index not consulted:\n%s",
			temporalityIndexName, deltaPlan)
	}
	if !strings.Contains(deltaPlan, "Granules: 0/") {
		t.Errorf("DELTA-predicate EXPLAIN plan did not prune to 0 granules on an all-CUMULATIVE table — "+
			"the redundant DELTA-arm scan #2458 reports would still read real data:\n%s", deltaPlan)
	}

	cumulativePlan := explainIndexes(t, db,
		"SELECT count() FROM "+cfg.Tables.MetricsSum+
			" WHERE MetricName = '"+metricName+"' AND AggregationTemporality != 1")
	if strings.Contains(cumulativePlan, "Granules: 0/") {
		t.Errorf("CUMULATIVE-predicate EXPLAIN plan pruned granules it must actually read "+
			"(every seeded row matches != DELTA) — a false skip would silently drop real rows:\n%s", cumulativePlan)
	}
}

// TestTemporalityIndex_DoesNotPruneAMixedGranule is the required negative
// case: a granule carrying BOTH CUMULATIVE and DELTA rows must not be
// skipped for EITHER predicate — a minmax mark spanning [1, 2] (DELTA,
// CUMULATIVE) matches both `= 1` and `!= 1`, so ClickHouse must still read
// it. Without this case, TestTemporalityIndex_PrunesAllCumulativeGranules
// alone could pass even if the index (or this test's own EXPLAIN parsing)
// treated the predicate as always-prunable regardless of what the data
// actually contains.
func TestTemporalityIndex_DoesNotPruneAMixedGranule(t *testing.T) {
	db, cfg := openMetricsProbeChDB(t)

	const metricName = "cerberus_queries_total"
	var b strings.Builder
	for i := range temporalityProbeRowsPerGranule {
		if i > 0 {
			b.WriteString(",\n")
		}
		// Every 4096th row (well inside this one granule) flips to DELTA, so
		// the granule's AggregationTemporality min/max mark spans [1, 2] and
		// cannot be pruned for either predicate.
		temporality := 2
		if i == temporalityProbeRowsPerGranule/2 {
			temporality = 1
		}
		fmt.Fprintf(&b, "('%s', map(), '', toDateTime64('2026-01-01 00:00:00', 9) + toIntervalSecond(%d), %d, %d, true)",
			metricName, i, i, temporality)
	}
	insert := "INSERT INTO " + cfg.Tables.MetricsSum +
		" (MetricName, Attributes, ServiceName, TimeUnix, Value, AggregationTemporality, IsMonotonic) VALUES " + b.String()
	if _, err := db.Exec(insert); err != nil {
		t.Fatalf("seed mixed-granule rows: %v", err)
	}
	if _, err := db.Exec("OPTIMIZE TABLE " + cfg.Tables.MetricsSum + " FINAL"); err != nil {
		t.Fatalf("optimize final: %v", err)
	}

	deltaPlan := explainIndexes(t, db,
		"SELECT count() FROM "+cfg.Tables.MetricsSum+
			" WHERE MetricName = '"+metricName+"' AND AggregationTemporality = 1")
	if strings.Contains(deltaPlan, "Granules: 0/") {
		t.Errorf("DELTA-predicate EXPLAIN plan pruned a granule that genuinely contains a DELTA row — "+
			"a false skip would silently drop it from the answer:\n%s", deltaPlan)
	}

	var count int64
	if err := db.QueryRow(
		"SELECT count() FROM " + cfg.Tables.MetricsSum +
			" WHERE MetricName = '" + metricName + "' AND AggregationTemporality = 1",
	).Scan(&count); err != nil {
		t.Fatalf("count DELTA rows: %v", err)
	}
	if count != 1 {
		t.Errorf("count(AggregationTemporality = 1) = %d, want 1 (the single seeded DELTA row) — "+
			"the index must never change the ANSWER, only the granules read to compute it", count)
	}
}

// explainIndexes runs `EXPLAIN indexes = 1 <query>` against db and returns
// the plan text joined by newlines.
func explainIndexes(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN indexes = 1 " + query)
	if err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("explain scan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	return plan.String()
}

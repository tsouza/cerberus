//go:build chdb

package ddl

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/deltaprefix"
	"github.com/tsouza/cerberus/internal/downsampletier"
	"github.com/tsouza/cerberus/internal/schema"
)

// backfillBefore bounds the DELTA-prefix backfill after every seeded sample.
var backfillBefore = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

// TestRenderAll_MetricsTimestampPrecision_ChDB applies RenderAll's metrics
// DDL — the five exporter tables, their curated projections and indexes, and
// the cerberus-owned DELTA-prefix and downsample-tier tables with their
// materialized views — to chDB for each MetricsTimestampPrecision, then
// writes samples through the views. It pins three things the rendered text
// alone cannot: ClickHouse accepts every statement for both precisions, the
// deployed timestamp columns carry the type the precision promises, and the
// views (whose aggregate states are typed DateTime64(9)) still accept rows
// from a DateTime source table.
func TestRenderAll_MetricsTimestampPrecision_ChDB(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// applySignal enables this per statement for the downsample-tier DDL;
	// here the whole session runs with it.
	mustExec(t, db, "SET "+downsampleExperimentalSetting+" = 1")

	cases := []struct {
		precision schema.MetricsTimestampPrecision
		typ       string
		// storedTimeUnix is what a sample written at 12:00:00.250 reads
		// back as: the precision's lossy truncation is part of its contract.
		storedTimeUnix string
		// tierContents is every downsample-tier row, rendered by
		// downsampleTierContents: one per source table, in the first bucket
		// ending at or after the stored sample, holding that sample.
		tierContents string
	}{
		{
			schema.MetricsTimestampNanos, "DateTime64(9)", "2026-01-01 12:00:00.250000000",
			"reqs_total 2026-01-01 12:05:00.000000000 [('2026-01-01 12:00:00.250000000',3)]; " +
				"temp 2026-01-01 12:05:00.000000000 [('2026-01-01 12:00:00.250000000',21)]",
		},
		{
			// The truncated sample sits exactly on a bucket boundary, so it
			// lands in the bucket ENDING there.
			schema.MetricsTimestampSeconds, "DateTime", "2026-01-01 12:00:00",
			"reqs_total 2026-01-01 12:00:00.000000000 [('2026-01-01 12:00:00.000000000',3)]; " +
				"temp 2026-01-01 12:00:00.000000000 [('2026-01-01 12:00:00.000000000',21)]",
		},
	}
	for _, c := range cases {
		t.Run(c.precision.String(), func(t *testing.T) {
			cfg := Config{
				Database:                  "precision_" + c.precision.String(),
				MetricsTimestampPrecision: c.precision,
				DeltaPrefixEnabled:        true,
				DownsampleTierEnabled:     true,
			}.withDefaults()
			mustExec(t, db, "DROP DATABASE IF EXISTS "+cfg.Database)
			t.Cleanup(func() { mustExec(t, db, "DROP DATABASE IF EXISTS "+cfg.Database) })
			stmts, err := RenderAll(cfg, []Signal{Metrics})
			if err != nil {
				t.Fatalf("RenderAll: %v", err)
			}
			for _, stmt := range stmts {
				mustExec(t, db, stmt)
			}

			metricTables := []string{
				cfg.Tables.MetricsGauge, cfg.Tables.MetricsSum, cfg.Tables.MetricsHistogram,
				cfg.Tables.MetricsExpHistogram, cfg.Tables.MetricsSummary,
			}
			for _, table := range metricTables {
				for column, want := range map[string]string{
					"TimeUnix":           c.typ,
					"StartTimeUnix":      c.typ,
					"Exemplars.TimeUnix": "Array(" + c.typ + ")",
				} {
					got, ok := columnTypeOf(t, db, cfg.Database, table, column)
					if !ok && column == "Exemplars.TimeUnix" && table == cfg.Tables.MetricsSummary {
						continue // the summary table carries no exemplars
					}
					if got != want {
						t.Errorf("%s.%s is %q, want %q", table, column, got, want)
					}
				}
			}

			const ts = "toDateTime64('2026-01-01 12:00:00.250', 9)"
			const deltaTemporality = 1
			mustExec(t, db, fmt.Sprintf(
				"INSERT INTO %s.%s (MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic) VALUES "+
					"('reqs_total', map('job', 'api'), %s, 3, %d, true)",
				cfg.Database, cfg.Tables.MetricsSum, ts, deltaTemporality,
			))
			mustExec(t, db, fmt.Sprintf(
				"INSERT INTO %s.%s (MetricName, Attributes, TimeUnix, Value) VALUES ('temp', map('room', 'a'), %s, 21)",
				cfg.Database, cfg.Tables.MetricsGauge, ts,
			))

			if got := queryString(t, db, fmt.Sprintf("SELECT toString(TimeUnix) FROM %s.%s", cfg.Database, cfg.Tables.MetricsSum)); got != c.storedTimeUnix {
				t.Errorf("stored TimeUnix = %q, want %q", got, c.storedTimeUnix)
			}
			if got := queryString(t, db, fmt.Sprintf("SELECT toString(sum(%s)) FROM %s.%s",
				cfg.DeltaPrefixSumColumn, cfg.Database, cfg.Tables.MetricsDeltaPrefix)); got != "3" {
				t.Errorf("DELTA-prefix partial sum = %q, want 3", got)
			}
			if got := downsampleTierContents(t, db, cfg.Database); got != c.tierContents {
				t.Errorf("downsample tier contents:\n got %s\nwant %s", got, c.tierContents)
			}

			// The one-time backfills read the same source tables outside the
			// views; each must run against either timestamp type.
			m := schema.DefaultOTelMetrics()
			m.DeltaPrefixTable = cfg.Tables.MetricsDeltaPrefix
			m.DeltaPrefixBucketColumn = cfg.DeltaPrefixBucketColumn
			m.DeltaPrefixSumColumn = cfg.DeltaPrefixSumColumn
			tier := downsampletier.FromSchema(cfg.Database, m)
			mustExec(t, db, downsampletier.TruncateSQL(tier))
			for _, stmt := range downsampletier.RebuildSQL(tier) {
				if _, err := db.Exec(stmt.SQL, stmt.Args...); err != nil {
					t.Fatalf("downsample-tier rebuild:\n%s\nerr: %v", stmt.SQL, err)
				}
			}
			if got := downsampleTierContents(t, db, cfg.Database); got != c.tierContents {
				t.Errorf("rebuilt downsample tier contents:\n got %s\nwant %s", got, c.tierContents)
			}
			mustExec(t, db, fmt.Sprintf("TRUNCATE TABLE %s.%s", cfg.Database, cfg.Tables.MetricsDeltaPrefix))
			backfill, args := deltaprefix.BackfillSQL(deltaprefix.FromSchema(cfg.Database, m), backfillBefore)
			if _, err := db.Exec(backfill, args...); err != nil {
				t.Fatalf("DELTA-prefix backfill:\n%s\nerr: %v", backfill, err)
			}
			if got := queryString(t, db, fmt.Sprintf("SELECT toString(sum(%s)) FROM %s.%s",
				cfg.DeltaPrefixSumColumn, cfg.Database, cfg.Tables.MetricsDeltaPrefix)); got != "3" {
				t.Errorf("backfilled DELTA-prefix partial sum = %q, want 3", got)
			}
		})
	}
}

func mustExec(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("exec:\n%s\nerr: %v", stmt, err)
	}
}

func queryString(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(query).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return s
}

// columnTypeOf reports a column's type as system.columns has it.
func columnTypeOf(t *testing.T, db *sql.DB, database, table, column string) (string, bool) {
	t.Helper()
	rows, err := db.Query("SELECT type FROM system.columns WHERE database = ? AND table = ? AND name = ?", database, table, column)
	if err != nil {
		t.Fatalf("system.columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return "", false
	}
	var typ string
	if err := rows.Scan(&typ); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return strings.TrimSpace(typ), true
}

// downsampleTierContents renders every row of database's downsample tier as
// `MetricName BucketEnd LastTwoSamples` (the state finalized), one per line in
// MetricName order.
func downsampleTierContents(t *testing.T, db *sql.DB, database string) string {
	t.Helper()
	return queryString(t, db, fmt.Sprintf(
		"SELECT arrayStringConcat(groupArray(line), '; ') FROM (SELECT concat(MetricName, ' ', toString(%s), ' ', "+
			"toString(finalizeAggregation(%s))) AS line FROM %s.%s ORDER BY MetricName)",
		schema.DownsampleTierBucketColumn, schema.DownsampleTierSamplesColumn, database, schema.DownsampleTierTable,
	))
}

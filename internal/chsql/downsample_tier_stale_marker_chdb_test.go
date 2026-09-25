//go:build chdb

// chDB-backed regression proof that the downsampled long-range tier never
// folds an OTel NoRecordedValue row — the collector's translation of a
// Prometheus stale marker, stored with Value = 0 — into its
// timeSeriesLastTwoSamples state. Both write paths into the tier are
// exercised against the REAL rendered SQL: the live materialized views
// (internal/schema/ddl) and the rebuild / backfill INSERT ... SELECT
// (internal/downsampletier).
//
// The seeded counter reads 10 at 00:01 and 20 at 00:02, then a stale marker
// at 00:03 — its target disappeared. Folded as a sample, the marker becomes the bucket's
// trailing sample and the trailing pair (20, 0) reads as a counter reset:
// irate 0 and last_over_time 0. Excluded, the trailing pair is (10, 20):
// irate 10/60 and last_over_time 20, which is what reference Prometheus
// answers too — a range selection never contains a stale marker.
package chsql_test

import (
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/downsampletier"
	"github.com/tsouza/cerberus/internal/schema"
)

// staleMarkerTierSample is one raw otel_metrics_sum row, flags included.
type staleMarkerTierSample struct {
	ts    time.Time
	value float64
	flags uint32
}

var staleMarkerTierSeed = []staleMarkerTierSample{
	{time.Date(2024, 1, 1, 0, 1, 0, 0, time.UTC), 10, 0},
	{time.Date(2024, 1, 1, 0, 2, 0, 0, time.UTC), 20, 0},
	{time.Date(2024, 1, 1, 0, 3, 0, 0, time.UTC), 0, schema.NoRecordedValueFlag},
}

// staleMarkerTierHost is the single series' host label.
const staleMarkerTierHost = "stale"

func insertStaleMarkerTierSamples(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, s := range staleMarkerTierSeed {
		_, err := db.Exec(
			`INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value, Flags, AggregationTemporality) VALUES (?, map('host', ?), ?, ?, ?, ?)`,
			"cpu_seconds_total", staleMarkerTierHost, s.ts, s.value, s.flags, 2,
		)
		if err != nil {
			t.Fatalf("insert ts=%s: %v", s.ts, err)
		}
	}
}

// assertStaleMarkerExcludedFromTier reads the tier through the real
// tier-routed irate / last_over_time lowering and pins the marker-free
// answers.
func assertStaleMarkerExcludedFromTier(t *testing.T, db *sql.DB) {
	t.Helper()
	const wantIrate = 10.0 / 60
	const wantLast = 20.0
	irate := runDownsampleTierPromQL(t, db, `irate(cpu_seconds_total[5m])`, true)
	if got, ok := irate[staleMarkerTierHost]; !ok || math.Abs(got-wantIrate) > 1e-12 {
		t.Errorf("tier irate = %v (present=%v); want %v — the stale marker was folded as a sample", got, ok, wantIrate)
	}
	last := runDownsampleTierPromQL(t, db, `last_over_time(cpu_seconds_total[5m])`, true)
	if got, ok := last[staleMarkerTierHost]; !ok || got != wantLast {
		t.Errorf("tier last_over_time = %v (present=%v); want %v — the stale marker was folded as a sample", got, ok, wantLast)
	}
}

// TestDownsampleTier_StaleMarkerExcludedByView folds the seed through the
// live materialized views.
func TestDownsampleTier_StaleMarkerExcludedByView(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	downsampleTierChdbSetup(t, db)
	insertStaleMarkerTierSamples(t, db)
	assertStaleMarkerExcludedFromTier(t, db)
}

// TestDownsampleTier_StaleMarkerExcludedByRebuild drops the views, seeds the
// base table, then populates the tier with internal/downsampletier's rebuild
// INSERT ... SELECT statements alone — the path that repopulates history the
// views never saw.
func TestDownsampleTier_StaleMarkerExcludedByRebuild(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	downsampleTierChdbSetup(t, db)
	var currentDB string
	if err := db.QueryRow("SELECT currentDatabase()").Scan(&currentDB); err != nil {
		t.Fatalf("read currentDatabase(): %v", err)
	}
	for _, view := range []string{
		schema.DownsampleTierTable + "_mv",
		schema.DownsampleTierTable + "_gauge_mv",
	} {
		if _, err := db.Exec(chsql.DropView(currentDB, view).SQL()); err != nil {
			t.Fatalf("drop %s: %v", view, err)
		}
	}
	insertStaleMarkerTierSamples(t, db)
	var n int
	if err := db.QueryRow("SELECT count() FROM " + schema.DownsampleTierTable).Scan(&n); err != nil {
		t.Fatalf("count tier rows: %v", err)
	}
	if n != 0 {
		t.Fatalf("tier holds %d rows after dropping its views; the rebuild below would not be what populated it", n)
	}
	cols := downsampletier.FromSchema(currentDB, schema.DefaultOTelMetrics())
	for _, stmt := range downsampletier.RebuildSQL(cols) {
		if _, err := db.Exec(stmt.SQL, stmt.Args...); err != nil {
			t.Fatalf("rebuild insert: %v\n%s", err, stmt.SQL)
		}
	}
	assertStaleMarkerExcludedFromTier(t, db)
}

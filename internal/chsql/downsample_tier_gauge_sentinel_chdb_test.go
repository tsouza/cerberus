//go:build chdb

// TestDownsampleTierGaugeSentinelTemporality_TakesSafeBranch is the sentinel
// safety proof cerberus issue #2858 requires: if the Go-level eligibility
// gate (internal/promql's attachDownsampleTierArm) ever had a bug and routed
// an irate() call onto a Gauge-sourced tier row anyway, the
// schema.DownsampleTierGaugeTemporalitySentinel value that row's Temporality
// column carries must NOT be misread as schema.AggregationTemporalityDelta —
// it must take the SAME "not DELTA" (counter-reset-aware) branch
// chsql.CounterOrDeltaPairDelta already takes for a nil temporality (a
// Gauge-table scan with no TemporalityColumn at all).
//
// This bypasses the normal PromQL lowering entirely (which structurally
// never produces this shape for a real query — see
// range_window_downsample_tier_gauge_chdb_test.go's
// TestDownsampleTierGauge_IrateIdeltaNeverRouteToTier for that proof) and
// hand-builds the chplan.RangeWindow the emitter itself would receive if
// eligibility had failed open, to prove the EMITTER's own branch selection
// is safe independent of the Go-level gate — defense in depth, not a
// substitute for it.
package chsql

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/schema"
)

// downsampleTierGaugeSentinelDDL hand-writes the tier table's real column
// shape (schema.DownsampleTierTable's own doc: MetricName, Attributes,
// ResourceAttributes, ServiceName, BucketEnd, LastTwoSamples, Temporality) —
// mirroring range_window_delta_prefix_aggregate_chdb_test.go's own
// hand-written-DDL precedent for an internal-package chDB test (importing
// internal/schema/ddl here would cycle back into this package).
func downsampleTierGaugeSentinelDDL(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE otel_metrics_sum_downsample_tier (
		MetricName LowCardinality(String),
		Attributes Map(LowCardinality(String), String),
		ResourceAttributes Map(LowCardinality(String), String),
		ServiceName LowCardinality(String),
		BucketEnd DateTime64(9),
		LastTwoSamples AggregateFunction(timeSeriesLastTwoSamples, DateTime64(9), Float64),
		Temporality SimpleAggregateFunction(any, Int32)
	) ENGINE = AggregatingMergeTree ORDER BY (MetricName, BucketEnd, Attributes, ResourceAttributes, ServiceName)`); err != nil {
		t.Fatalf("create otel_metrics_sum_downsample_tier: %v", err)
	}
}

// downsampleTierGaugeSentinelAnchor is the bucket this hand-seeded row
// belongs to and the single grid anchor the test evaluates at.
var downsampleTierGaugeSentinelAnchor = time.Date(2024, 6, 1, 0, 5, 0, 0, time.UTC)

// seedDownsampleTierGaugeSentinelRow writes ONE tier row directly (bypassing
// any MV) whose LastTwoSamples state holds an INCREASING pair — prev=30 at
// 00:01:00, curr=45 at 00:03:00 — chosen so the DELTA branch (raw curr
// alone, 45) and the "not DELTA" branch (curr-prev, 15) disagree; a test
// that instead used a decreasing ("reset") pair could not distinguish the
// two branches, since Prometheus's reset-heuristic collapses them both to
// the raw curr value in that case. Temporality carries the fixed
// schema.DownsampleTierGaugeTemporalitySentinel literal — exactly what
// renderDownsampleTierGaugeView would have written for a real Gauge sample.
func seedDownsampleTierGaugeSentinelRow(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO otel_metrics_sum_downsample_tier
		SELECT 'gauge_metric', map('host', 'h1'), map(), '',
		       toDateTime64('2024-06-01 00:05:00', 9),
		       timeSeriesLastTwoSamplesState(t, v),
		       ?
		FROM (
			SELECT toDateTime64('2024-06-01 00:01:00', 9) AS t, 30.0 AS v
			UNION ALL
			SELECT toDateTime64('2024-06-01 00:03:00', 9) AS t, 45.0 AS v
		)`, schema.DownsampleTierGaugeTemporalitySentinel)
	if err != nil {
		t.Fatalf("seed sentinel tier row: %v", err)
	}
}

// downsampleTierGaugeSentinelWindow hand-builds the SAME chplan.RangeWindow
// shape attachDownsampleTierArm/buildDownsampleTierArm would produce for an
// irate() call — DownsampleTier: true, DownsampleTierInput scanning the
// tier table directly (the isolated chDB database holds exactly the one row
// seedDownsampleTierGaugeSentinelRow wrote, so no MetricName filter is
// needed to isolate it).
func downsampleTierGaugeSentinelWindow() *chplan.RangeWindow {
	return &chplan.RangeWindow{
		DownsampleTier:      true,
		DownsampleTierInput: &chplan.Scan{Table: schema.DownsampleTierTable},
		Func:                "irate",
		Range:               schema.DownsampleTierBucket,
		Step:                schema.DownsampleTierBucket,
		Start:               downsampleTierGaugeSentinelAnchor,
		End:                 downsampleTierGaugeSentinelAnchor,
		TimestampColumn:     "TimeUnix",
		ValueColumn:         "Value",
		GroupBy:             []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

func TestDownsampleTierGaugeSentinelTemporality_TakesSafeBranch(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	downsampleTierGaugeSentinelDDL(t, db)
	seedDownsampleTierGaugeSentinelRow(t, db)

	sqlText, args, err := Emit(context.Background(), downsampleTierGaugeSentinelWindow())
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	wrapped := "SELECT `Value` FROM (" + sqlText + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, wrapped)
	}
	defer func() { _ = rows.Close() }()
	var value float64
	got := false
	for rows.Next() {
		if got {
			t.Fatal("expected exactly one row")
		}
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !got {
		t.Fatal("expected exactly one row, got none")
	}

	const interval = 120.0 // seconds between 00:01:00 and 00:03:00
	wantNotDelta := 15.0 / interval
	wantDelta := 45.0 / interval
	if math.Abs(value-wantDelta) < 1e-12 {
		t.Fatalf("value=%v took the DELTA branch (raw curr alone) — the sentinel %d was misread as "+
			"schema.AggregationTemporalityDelta (%d)", value, schema.DownsampleTierGaugeTemporalitySentinel, schema.AggregationTemporalityDelta)
	}
	if math.Abs(value-wantNotDelta) > 1e-12 {
		t.Fatalf("value=%v; want %v (the counter-reset-aware curr-prev branch, matching a nil-temporality Gauge read)", value, wantNotDelta)
	}
}

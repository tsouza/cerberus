//go:build integration

// Real-ClickHouse regression pin for the window-narrower-than-step shape
// behind chopt.FeatureTSGridLastOverTime's 26.6 floor. See that registry
// entry, and the file doc on range_window_last_over_time_chdb_test.go, for
// the full story: ClickHouse/ClickHouse#106577 ("Fix timeSeriesLastToGrid()
// for out-of-window timestamps") fixed a real correctness bug in
// timeSeriesResampleToGridWithStaleness's sparse carry-forward
// (AggregateFunctionTimeseriesToGridSparse.h) where a sample landing in grid
// bucket i's coarse step-wide span could be reported as bucket i's answer
// WITHOUT re-checking staleness against bucket i's own precise window — only
// when the staleness window is narrower than the grid step, which is the
// common last_over_time shape and NOT the shape ts_grid_resample's fixed 5m
// default lookback usually exercises. This repo's shared chDB substrate
// (versions.yaml, ClickHouse 26.5) and the wider testcontainers pin
// elsewhere in the suite (25.9-alpine) both sit BELOW that floor, so this is
// the one lane that can exercise the native path on a patched engine at
// all; it needs Docker and is gated by the `integration` build tag
// accordingly, mirroring
// TestHistogramQuantile_RankWalkNative_DifferentialRealCH's own posture for
// chopt.FeatureQuantilePromHistogram's 25.10 floor.
package chsql_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	promparser "github.com/prometheus/prometheus/promql/parser"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// tsGridLastOverTimeImage pins a ClickHouse version at or above
// chopt.FeatureTSGridLastOverTime's 26.6 floor (see that registry entry). A
// plain literal rather than an import of internal/chopt: this test's only
// dependency on the feature is the version its own testcontainers image is
// pinned to. The Justfile's `ts-grid-last-over-time-integration` recipe
// deliberately reuses CH_STRICT_SCAN_IMAGE rather than pinning its own
// dedicated variable (unlike quantile_prom_histogram's
// CH_QUANTILE_PROM_HISTOGRAM_IMAGE): this feature's 26.6 floor is the
// highest in the registry, so strict-scan's own pin was raised to clear it
// (TestStrictScanImageClearsChoptFloors) and now floors at the identical
// version — introducing a second variable would just be the same string
// twice. TestIntegrationImagePinsMatchTheJustfile holds this literal
// against that shared pin.
const tsGridLastOverTimeImage = "clickhouse/clickhouse-server:26.6-alpine"

// TestLastOverTime_NativeResample_WindowNarrowerThanStep_RealCH reproduces,
// end to end through cerberus's own lowering + emitter, the EXACT shape
// upstream's own #106577 regression fixture exercises: a staleness window
// (10s) narrower than the query_range grid step (15s), with a sample
// positioned so it is NOT the last-known-value for ANY grid anchor under
// correct PromQL semantics — series "affected" places a sample at t=138s
// with a 15s-step grid at 0/15/.../180s and a 10s window: the sample is
// visible only to anchors in [138, 148), and no multiple of 15 falls in that
// half-open span, so EVERY anchor must come back absent on a correct engine.
// Pre-#106577, ClickHouse's sparse carry-forward directly (and wrongly)
// assigned it to grid bucket 150 (its coarse (135, 150] step-span) without
// re-checking that 138 is stale for bucket 150's own (140, 150] window.
//
// Series "control" is the same grid with a sample placed safely inside one
// anchor's true window, so the test cannot pass merely because the whole
// pipeline returned nothing — it proves the native and fan-out paths agree
// on ordinary inclusion too, on the SAME real server.
func TestLastOverTime_NativeResample_WindowNarrowerThanStep_RealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcclickhouse.Run(
		ctx,
		tsGridLastOverTimeImage,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	db := clickhouse.OpenDB(&clickhouse.Options{
		Addr: []string{host + ":" + port.Port()},
		Auth: clickhouse.Auth{
			Database: "otel",
			Username: "cerberus",
			Password: "cerberus",
		},
	})
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	assertTimeSeriesResampleToGridWithStalenessPresent(ctx, t, db)

	if _, err := db.ExecContext(ctx, `
CREATE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix)
`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('disk_used_bytes', map('host', 'affected'), toDateTime64('2026-01-01 00:02:18', 9), 42.0),
    ('disk_used_bytes', map('host', 'control'), toDateTime64('2026-01-01 00:02:25', 9), 7.0)
`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// grid: 00:00:00 .. 00:03:00 step 15s (matches upstream's own
	// regression fixture step); window 10s (< step).
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(3 * time.Minute)
	const step = 15 * time.Second

	fanout := runLastOverTimeRealCH(ctx, t, db, rangeStart, rangeEnd, step, false)
	native := runLastOverTimeRealCH(ctx, t, db, rangeStart, rangeEnd, step, true)

	if got := fanout["affected"]; len(got) != 0 {
		t.Fatalf("fan-out: host=affected want zero rows (no anchor's window covers a sample at :02:18 with a 10s window), got %v", got)
	}
	if got := native["affected"]; len(got) != 0 {
		t.Errorf("native REGRESSED to the pre-#106577 bug: host=affected want zero rows on ClickHouse >= 26.6, got %v — "+
			"a grid-bucket-local sample that is stale for its own anchor's window was carried forward anyway", got)
	}

	if len(fanout["control"]) == 0 {
		t.Fatal("fan-out: host=control want at least one row (the sample sits inside a real anchor's window) — fixture is broken")
	}
	if len(native["control"]) != len(fanout["control"]) {
		t.Fatalf("host=control row-count divergence: native=%d fanout=%d", len(native["control"]), len(fanout["control"]))
	}
	for anchor, fv := range fanout["control"] {
		nv, ok := native["control"][anchor]
		if !ok {
			t.Errorf("host=control anchor=%s present in fan-out but absent in native", anchor)
			continue
		}
		if nv != fv {
			t.Errorf("host=control anchor=%s: native=%v fanout=%v NOT equal", anchor, nv, fv)
		}
	}
}

// assertTimeSeriesResampleToGridWithStalenessPresent probes system.functions
// directly, mirroring assertQuantilePrometheusHistogramPresent's own
// rationale: a future ClickHouse release that renames or removes the
// function fails this test with a clear message instead of the differential
// assertions failing opaquely.
func assertTimeSeriesResampleToGridWithStalenessPresent(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	var present bool
	if err := db.QueryRowContext(
		ctx,
		"SELECT count() > 0 FROM system.functions WHERE name = 'timeSeriesResampleToGridWithStaleness'",
	).Scan(&present); err != nil {
		t.Fatalf("probe system.functions: %v", err)
	}
	if !present {
		t.Fatalf("timeSeriesResampleToGridWithStaleness not present in system.functions on %s — floor probe assumption broken", tsGridLastOverTimeImage)
	}
}

// runLastOverTimeRealCH lowers + emits `last_over_time(disk_used_bytes[10s])`
// over [start, end] at step with the native-last_over_time strategy set to
// `native`, runs the resulting SQL on db (with the experimental setting
// scoped to this one query via a SETTINGS clause — a raw *sql.DB connection
// pool does not guarantee a session-level SET survives to the connection a
// later query reuses), and returns the per-host, per-anchor selected value.
func runLastOverTimeRealCH(ctx context.Context, t *testing.T, db *sql.DB, start, end time.Time, step time.Duration, native bool) map[string]map[string]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr("last_over_time(disk_used_bytes[10s])")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var lowerers promql.RangeLowerers
	if native {
		lowerers.LastOverTime = promql.NativeLastOverTimeLowerer{Fallback: promql.FanoutLastOverTimeLowerer{}}
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		start, end, step,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower (native=%v): %v", native, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit (native=%v): %v", native, err)
	}
	wrapped := fmt.Sprintf(
		"SELECT toJSONString(`Attributes`) AS host_json, `TimeUnix`, `Value` FROM (%s) SETTINGS %s = 1",
		sqlStr, chclient.SettingExperimentalTSGridAggregate,
	)
	rows, err := db.QueryContext(ctx, wrapped, args...)
	if err != nil {
		t.Fatalf("query (native=%v): %v\nSQL: %s", native, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]map[string]float64{"affected": {}, "control": {}}
	for rows.Next() {
		var hostJSON string
		var ts time.Time
		var v float64
		if err := rows.Scan(&hostJSON, &ts, &v); err != nil {
			t.Fatalf("scan (native=%v): %v", native, err)
		}
		host := realCHExtractHostLabel(hostJSON)
		if out[host] == nil {
			out[host] = map[string]float64{}
		}
		out[host][ts.UTC().Format(time.RFC3339)] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err (native=%v): %v", native, err)
	}
	return out
}

// realCHExtractHostLabel pulls the host value out of the JSON-encoded
// Attributes map (`{"host":"a"}`). A local copy rather than a shared helper:
// the chdb-tagged extractHostLabel (range_window_changes_chdb_test.go) and
// its indexOf helper are not compiled into an integration-only build.
func realCHExtractHostLabel(jsonStr string) string {
	const key = `"host":"`
	i := strings.Index(jsonStr, key)
	if i < 0 {
		return ""
	}
	rest := jsonStr[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return rest
	}
	return rest[:j]
}

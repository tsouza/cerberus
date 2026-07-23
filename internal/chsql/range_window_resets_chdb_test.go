//go:build chdb

// chDB-backed dual-emit parity pin for the experimental timeSeriesResetsToGrid
// lowering (chplan.RangeWindowNative, Func="resets").
//
// The test lowers the SAME `sum by (job) (resets(http_requests_total[5m]))`
// query_range expression TWICE against the SAME seed — once with the native
// resets strategy OFF (the arrayPopBack/arrayPopFront `c < p` count fan-out,
// RangeWindow) and once with it ON (the native timeSeriesResetsToGrid,
// RangeWindowNative) — runs BOTH on the same ephemeral chDB session, and
// compares the per-(series, anchor) reset-count values. Counts are exact
// integers in float64, so the assertion is BIT-IDENTICAL.
//
// Substrate: exercised on CI. timeSeriesResetsToGrid shipped in v25.9
// (PR #86010, the sibling of timeSeriesChangesToGrid, floor-pinned 25.9 in
// internal/chopt). The chDB parity substrate is chdb-core v26.5.0 (CH 26.5.1.1,
// versions.yaml chdb_substrate), so the function is PRESENT and the native half
// fires in the `chdb` CI lane. The test feature-detects via system.functions so
// the fan-out half still validates on an older local libchdb; the forbid-skip CI
// gate bans the test-skip API, so this is a documented runtime conditional that
// ALWAYS executes and never silently loses coverage. See the changes sibling test
// for the full rationale.
//
// Semantic parity, exercised on the CI substrate: native timeSeriesResetsToGrid
// requires >= 1 sample/window (a single-sample window is a 0 count, not absent),
// matching the fan-out's `length(window_vals) >= 1` + per-pair `c < p` count.
// The half-open `(t-range, t]` vs closed-window left-edge distinction is
// measure-zero (same note as resample); the seed uses integer-minute samples to
// avoid the boundary entirely.
package chsql_test

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// resetsSeed mirrors the production OTel-CH default schema. Two job series: api
// is a sawtooth counter (multiple resets), web is monotonic (zero resets), so
// the per-series count differs and a 0-count series is exercised.
const resetsSeed = `
CREATE OR REPLACE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value) VALUES
    ('http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:00:00', 9), 1.0),
    ('http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:01:00', 9), 5.0),
    ('http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:02:00', 9), 2.0),
    ('http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:03:00', 9), 8.0),
    ('http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:04:00', 9), 1.0),
    ('http_requests_total', map('job', 'api'), toDateTime64('2026-01-01 00:05:00', 9), 6.0),
    ('http_requests_total', map('job', 'web'), toDateTime64('2026-01-01 00:00:00', 9), 10.0),
    ('http_requests_total', map('job', 'web'), toDateTime64('2026-01-01 00:01:00', 9), 20.0),
    ('http_requests_total', map('job', 'web'), toDateTime64('2026-01-01 00:02:00', 9), 30.0),
    ('http_requests_total', map('job', 'web'), toDateTime64('2026-01-01 00:03:00', 9), 40.0),
    ('http_requests_total', map('job', 'web'), toDateTime64('2026-01-01 00:04:00', 9), 50.0),
    ('http_requests_total', map('job', 'web'), toDateTime64('2026-01-01 00:05:00', 9), 60.0);
`

// resetsQuery wraps the resets() matrix fn in a transparent `sum by`, so the
// per-(series, anchor) reset count is what both paths must agree on.
const resetsQuery = `sum by(job) (resets(http_requests_total[5m]))`

func TestNativeTSGridResets_DualEmitParity(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	for _, stmt := range splitSeedStatements(resetsSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	fanout := runResetsEmit(t, db, false, false)
	if !resetsFnPresent(t, db) {
		t.Logf("NOTICE: timeSeriesResetsToGrid absent on this local chDB substrate " +
			"(older libchdb than the pinned chdb-core v26.5.0) — native parity assertion " +
			"bypassed (fan-out half still validated). Run `just chdb-install` at the pinned " +
			"version to exercise the native half; it runs in the `chdb` CI lane on CH 26.5. " +
			"The always-on SQL-shape golden (native_resets_range_step.txtar) still pins the emit.")
		return
	}
	native := runResetsEmit(t, db, true, false)

	nativeOpt := runResetsEmit(t, db, true, true)
	if len(nativeOpt) != len(native) {
		t.Fatalf("optimized-native row-count divergence: opt=%d wide=%d cells", len(nativeOpt), len(native))
	}
	for cell, wv := range native {
		ov, ok := nativeOpt[cell]
		if !ok {
			t.Errorf("cell %+v present in wide native but absent in optimized native (a column was dropped)", cell)
			continue
		}
		if math.Float64bits(ov) != math.Float64bits(wv) {
			t.Errorf("cell %+v: optimized-native=%.20g wide-native=%.20g NOT bit-identical — "+
				"the scan narrowing changed a count (the narrowing is WRONG)", cell, ov, wv)
		}
	}

	if len(native) != len(fanout) {
		t.Fatalf("row-count divergence: native=%d fanout=%d cells\nnative=%v\nfanout=%v",
			len(native), len(fanout), native, fanout)
	}
	for cell, fv := range fanout {
		nv, ok := native[cell]
		if !ok {
			t.Errorf("cell %+v present in fan-out but absent in native", cell)
			continue
		}
		if math.Float64bits(nv) != math.Float64bits(fv) {
			t.Errorf("cell %+v: native=%.20g fanout=%.20g NOT bit-identical — "+
				"the native reset count diverged from the fan-out", cell, nv, fv)
		}
	}
	t.Logf("resets dual-emit parity: %d/%d cells bit-identical. "+
		"native == fan-out == Prometheus on the resets shape.", len(fanout), len(fanout))
}

// runResetsEmit lowers + emits the resets query with the native-resets strategy
// set to `native`, optionally runs the default optimizer pipeline, runs the
// resulting SQL on db, and returns the per-cell reset counts.
func runResetsEmit(t *testing.T, db *sql.DB, native, optimize bool) map[resampleCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(resetsQuery)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	var lowerers promql.RangeLowerers
	if native {
		lowerers.Resets = promql.NativeResetsLowerer{Fallback: promql.FanoutResetsLowerer{}}
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		rangeStart, rangeEnd, 30*time.Second,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower (native=%v): %v", native, err)
	}
	if optimize {
		plan = optimizer.Default().Run(context.Background(), plan)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit (native=%v): %v", native, err)
	}
	wrapped := "SELECT toJSONString(`Attributes`) AS job_json, `TimeUnix`, `Value` FROM (" + sqlStr + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query (native=%v): %v\nSQL: %s", native, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[resampleCell]float64)
	for rows.Next() {
		var jobJSON string
		var ts time.Time
		var v float64
		if err := rows.Scan(&jobJSON, &ts, &v); err != nil {
			t.Fatalf("scan (native=%v): %v", native, err)
		}
		out[resampleCell{job: extractJobLabel(jobJSON), anchor: ts.UTC().Format(time.RFC3339)}] = v
	}
	if err := tolerantSentinel(rows.Err()); err != nil {
		t.Fatalf("rows.Err (native=%v): %v", native, err)
	}
	if len(out) == 0 {
		t.Fatalf("native=%v produced zero rows — the resets fixture must yield a populated grid", native)
	}
	return out
}

// resetsFnPresent feature-detects timeSeriesResetsToGrid via system.functions
// (the gating fact the native resets path depends on; ABSENT below CH 25.9).
func resetsFnPresent(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count() FROM system.functions WHERE name = 'timeSeriesResetsToGrid'",
	).Scan(&n); err != nil {
		t.Fatalf("feature-detect timeSeriesResetsToGrid: %v", err)
	}
	return n > 0
}

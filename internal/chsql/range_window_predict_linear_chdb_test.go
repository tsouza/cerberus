//go:build chdb

// chDB-backed dual-emit parity pin for the experimental
// timeSeriesPredictLinearToGrid lowering (chplan.RangeWindowNative,
// Func="predict_linear").
//
// The test lowers the SAME `sum by (host) (predict_linear(load_state[5m], 3600))`
// query_range expression TWICE against the SAME seed — once with the native
// predict_linear strategy OFF (the simpleLinearRegression intercept + slope*t
// forecast fan-out, RangeWindow) and once with it ON (the native
// timeSeriesPredictLinearToGrid, RangeWindowNative) — runs BOTH on the same
// ephemeral chDB session, and compares the per-(series, anchor) forecast values.
//
// Why this is the parity proof. The fan-out's per-window forecast is the
// Prometheus-pinned funcPredictLinear value (the spec corpus is
// reference-Prometheus-pinned), so native == fan-out transitively proves
// native == Prometheus on the predict_linear shape FOR WHOLE-SECOND-ALIGNED
// SAMPLES (see the scope note below). We compare the DECODED float64 (never a
// string render) at full precision.
//
// Horizon literal, not computed. The native path only fires when the horizon t
// is a single whole-second literal (nativePredictLinearHorizonEligible): the CH
// aggregate takes the offset as a constant 5th parametric arg, so 3600 here is
// deliberately a bare integer literal — a computed or fractional horizon would
// (correctly) stay on the fan-out and the native half would never engage.
//
// Bit-identical, not tolerant. Both paths do the SAME whole-second regression:
// the fan-out's windowPairsSLRFrag x-axis is dateDiff('second', anchor, ts), and
// the native emit feeds timeSeriesPredictLinearToGrid the matching whole-second
// axis (toDateTime(ts)). On the raw DateTime64(9) column the native forecast only
// diverged by float-order noise (its slope*offset product masks the deriv
// axis-unit bug); on the matching whole-second axis the two independent
// implementations agree to the last bit on this fixture, so the assertion is
// BIT-IDENTICAL (no ULP tolerance) — the same strictness the changes/resets
// count parity tests use.
//
// Substrate: exercised on CI. timeSeriesPredictLinearToGrid shipped in CH 25.8
// (PR #84328) and is floor-pinned to 25.9 in internal/chopt (a uniform
// capability verdict with the 25.9 shared-window fix PR #86588). The chDB
// parity substrate is chdb-core v26.5.0 (CH 26.5.1.1,
// versions.yaml chdb_substrate), so the function is PRESENT and the native half
// fires in the `chdb` CI lane. The feature-detect guard below remains so the
// fan-out half still validates on an older local libchdb (a developer who has
// not run `just chdb-install` at the pinned version); the forbid-skip gate bans
// the test-skip API, so this is a documented runtime conditional that ALWAYS
// executes and never silently loses coverage.
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

// predictLinearSeed mirrors the production OTel-CH default schema. It reuses the
// deriv fixture's two distinct-slope host series (a rising, b falling) so the
// forecast is non-trivial, non-constant, and differs per series — the same clean
// 1-minute grid.
const predictLinearSeed = `
CREATE OR REPLACE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), 0.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 10.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 22.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:03:00', 9), 33.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:04:00', 9), 47.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:05:00', 9), 58.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:00:00', 9), 100.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:01:00', 9), 97.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:02:00', 9), 91.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:03:00', 9), 82.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:04:00', 9), 70.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:05:00', 9), 55.0);
`

// predictLinearQuery forecasts one hour ahead (3600s). The horizon is a bare
// whole-second literal so the native path (which requires a constant horizon
// arg) is eligible; each series is its own `sum by` group so the per-(series,
// anchor) forecast is what both paths must agree on.
const predictLinearQuery = `sum by(host) (predict_linear(load_state[5m], 3600))`

func TestNativeTSGridPredictLinear_DualEmitParity(t *testing.T) {
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
	for _, stmt := range splitSeedStatements(predictLinearSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	fanout := runPredictLinearEmit(t, db, false, false)
	if !predictLinearFnPresent(t, db) {
		t.Logf("NOTICE: timeSeriesPredictLinearToGrid absent on this local chDB substrate " +
			"(older libchdb than the pinned chdb-core v26.5.0) — native parity assertion " +
			"bypassed (fan-out half still validated). Run `just chdb-install` at the pinned " +
			"version to exercise the native half; it runs in the `chdb` CI lane on CH 26.5.")
		return
	}
	native := runPredictLinearEmit(t, db, true, false)

	// Optimizer-narrowed native scan must be BIT-IDENTICAL to the wide native
	// scan (ProjectionPushdown narrows the RangeWindowNative inner Scan to the
	// exact {Attributes, TimeUnix, Value} the emit reads — it must change neither
	// the row set nor a single forecast value at full float64 precision).
	nativeOpt := runPredictLinearEmit(t, db, true, true)
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
				"the scan narrowing changed a forecast (the narrowing is WRONG)", cell, ov, wv)
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
		// predict_linear forecast: the native whole-second axis matches the
		// fan-out's dateDiff('second') regression exactly, so native and fan-out
		// agree to the last bit — assert BIT-IDENTICAL.
		if math.Float64bits(nv) != math.Float64bits(fv) {
			t.Errorf("cell %+v: native=%.20g fanout=%.20g NOT bit-identical — "+
				"the native predict_linear forecast diverged from the fan-out (unit/axis regression?)", cell, nv, fv)
		}
	}
	t.Logf("predict_linear dual-emit parity: %d/%d cells bit-identical. "+
		"native == fan-out == Prometheus on the predict_linear shape.", len(fanout), len(fanout))
}

// runPredictLinearEmit lowers + emits the predict_linear query with the native
// strategy set to `native`, optionally runs the default optimizer pipeline, runs
// the resulting SQL on db, and returns the per-cell forecast values.
func runPredictLinearEmit(t *testing.T, db *sql.DB, native, optimize bool) map[gridCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(predictLinearQuery)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	var lowerers promql.RangeLowerers
	if native {
		lowerers.PredictLinear = promql.NativePredictLinearLowerer{Fallback: promql.FanoutPredictLinearLowerer{}}
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
	wrapped := "SELECT toJSONString(`Attributes`) AS host_json, `TimeUnix`, `Value` FROM (" + sqlStr + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query (native=%v): %v\nSQL: %s", native, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[gridCell]float64)
	for rows.Next() {
		var hostJSON string
		var ts time.Time
		var v float64
		if err := rows.Scan(&hostJSON, &ts, &v); err != nil {
			t.Fatalf("scan (native=%v): %v", native, err)
		}
		out[gridCell{ql: extractHostLabel(hostJSON), anchor: ts.UTC().Format(time.RFC3339)}] = v
	}
	if err := tolerantSentinel(rows.Err()); err != nil {
		t.Fatalf("rows.Err (native=%v): %v", native, err)
	}
	if len(out) == 0 {
		t.Fatalf("native=%v produced zero rows — the predict_linear fixture must yield a populated grid", native)
	}
	return out
}

// predictLinearFnPresent feature-detects timeSeriesPredictLinearToGrid via
// system.functions (the gating fact the native path depends on; ABSENT below
// CH 25.9).
func predictLinearFnPresent(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count() FROM system.functions WHERE name = 'timeSeriesPredictLinearToGrid'",
	).Scan(&n); err != nil {
		t.Fatalf("feature-detect timeSeriesPredictLinearToGrid: %v", err)
	}
	return n > 0
}

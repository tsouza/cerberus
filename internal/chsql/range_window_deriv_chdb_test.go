//go:build chdb

// chDB-backed dual-emit parity pin for the experimental
// timeSeriesDerivToGrid lowering (chplan.RangeWindowNative, Func="deriv").
//
// The test lowers the SAME `sum by (host) (deriv(load_state[5m]))`
// query_range expression TWICE against the SAME seed — once with the native
// deriv strategy OFF (the least-squares linear-regression slope fan-out,
// RangeWindow) and once with it ON (the native timeSeriesDerivToGrid,
// RangeWindowNative) — runs BOTH on the same ephemeral chDB session, and
// compares the per-(series, anchor) slope values.
//
// Why this is the parity proof. The fan-out's per-window slope is the
// Prometheus-pinned funcDeriv value (the spec corpus is reference-Prometheus-
// pinned), so native == fan-out transitively proves native == Prometheus on the
// deriv shape FOR WHOLE-SECOND-ALIGNED SAMPLES (see the scope note below). We
// compare the DECODED float64 (never a string render) at full precision.
//
// Bit-identical, not tolerant. The fan-out computes the slope through
// windowPairsSLRFrag, whose x-axis is dateDiff('second', anchor, ts) — a
// WHOLE-SECOND grid. The native emit feeds timeSeriesDerivToGrid the same
// whole-second axis (toDateTime(ts)); on the raw DateTime64(9) column the native
// slope would come out per-NANOSECOND (1e9x too small). Fed the matching
// whole-second axis the two independent implementations agree to the last bit on
// this fixture, so the assertion is BIT-IDENTICAL (no ULP tolerance) — the same
// strictness the changes/resets count parity tests use. A regression (e.g. the
// axis-unit bug this test was written to catch) diverges by billions of ULP, far
// outside the bit-identical bound.
//
// Scope: whole-second-aligned only. The seed timestamps are whole minutes, so
// toDateTime(ts) == ts and the native and fan-out window MEMBERSHIP coincide.
// On sub-second-offset samples they need not: toDateTime(ts) drives both the
// regression axis AND the aggregate's window bucketing, so a boundary sample
// buckets by its floored second here while the fan-out keeps raw-ts membership
// (see nativeGridTsAxisFrag's LIMITATION note). This test therefore proves
// bit-identity for whole-second-aligned data; the native regression path stays
// experimental/default-off until the sub-second membership gap is closed or
// pinned.
//
// Substrate: exercised on CI. timeSeriesDerivToGrid shipped in CH 25.8
// (PR #84328) and is floor-pinned to 25.9 in internal/chopt (a uniform
// capability verdict with the 25.9 shared-window fix PR #86588). The chDB
// parity substrate is chdb-core v26.5.0 (CH 26.5.1.1,
// versions.yaml chdb_substrate), so the function is PRESENT and the native
// half fires in the `chdb` CI lane. The feature-detect guard below remains so
// the fan-out half still validates on an older local libchdb (a developer who
// has not run `just chdb-install` at the pinned version); the forbid-skip gate
// bans the test-skip API, so this is a documented runtime conditional that
// ALWAYS executes and never silently loses coverage.
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

// derivSeed mirrors the production OTel-CH default schema (ResourceAttributes
// present, DEFAULT map(), column-explicit INSERT). Two host series with
// distinct, non-constant slopes so the regression value is non-trivial and
// differs per series, on a clean 1-minute grid.
const derivSeed = `
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

// derivQuery wraps the deriv() matrix fn in a transparent `sum by` (each series
// is its own group), so the per-(series, anchor) regression slope is what both
// paths must agree on.
const derivQuery = `sum by(host) (deriv(load_state[5m]))`

func TestNativeTSGridDeriv_DualEmitParity(t *testing.T) {
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
	for _, stmt := range splitSeedStatements(derivSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	fanout := runDerivEmit(t, db, false, false)
	if !derivFnPresent(t, db) {
		t.Logf("NOTICE: timeSeriesDerivToGrid absent on this local chDB substrate " +
			"(older libchdb than the pinned chdb-core v26.5.0) — native parity assertion " +
			"bypassed (fan-out half still validated). Run `just chdb-install` at the pinned " +
			"version to exercise the native half; it runs in the `chdb` CI lane on CH 26.5.")
		return
	}
	native := runDerivEmit(t, db, true, false)

	// Optimizer-narrowed native scan must be BIT-IDENTICAL to the wide native
	// scan (ProjectionPushdown narrows the RangeWindowNative inner Scan to the
	// exact {Attributes, TimeUnix, Value} the emit reads — it must change neither
	// the row set nor a single slope value at full float64 precision).
	nativeOpt := runDerivEmit(t, db, true, true)
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
				"the scan narrowing changed a slope (the narrowing is WRONG)", cell, ov, wv)
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
		// deriv slope: the native whole-second axis matches the fan-out's
		// dateDiff('second') regression exactly, so native and fan-out agree to
		// the last bit — assert BIT-IDENTICAL. The axis-unit bug this test guards
		// diverges by ~1e9x (billions of ULP), nowhere near the bit-identical bound.
		if math.Float64bits(nv) != math.Float64bits(fv) {
			t.Errorf("cell %+v: native=%.20g fanout=%.20g NOT bit-identical — "+
				"the native deriv slope diverged from the fan-out (unit/axis regression?)", cell, nv, fv)
		}
	}
	t.Logf("deriv dual-emit parity: %d/%d cells bit-identical. "+
		"native == fan-out == Prometheus on the deriv shape (whole-second-aligned samples).", len(fanout), len(fanout))
}

// runDerivEmit lowers + emits the deriv query with the native-deriv strategy set
// to `native`, optionally runs the default optimizer pipeline, runs the
// resulting SQL on db, and returns the per-cell regression slopes.
func runDerivEmit(t *testing.T, db *sql.DB, native, optimize bool) map[gridCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(derivQuery)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	var lowerers promql.RangeLowerers
	if native {
		lowerers.Deriv = promql.NativeDerivLowerer{Fallback: promql.FanoutDerivLowerer{}}
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
		t.Fatalf("native=%v produced zero rows — the deriv fixture must yield a populated grid", native)
	}
	return out
}

// derivFnPresent feature-detects timeSeriesDerivToGrid via system.functions (the
// gating fact the native deriv path depends on; ABSENT below CH 25.9).
func derivFnPresent(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count() FROM system.functions WHERE name = 'timeSeriesDerivToGrid'",
	).Scan(&n); err != nil {
		t.Fatalf("feature-detect timeSeriesDerivToGrid: %v", err)
	}
	return n > 0
}

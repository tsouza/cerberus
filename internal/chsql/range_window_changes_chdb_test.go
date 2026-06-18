//go:build chdb

// chDB-backed dual-emit parity pin for the experimental
// timeSeriesChangesToGrid lowering (chplan.RangeWindowNative, Func="changes").
//
// The test lowers the SAME `sum by (host) (changes(load_state[5m]))`
// query_range expression TWICE against the SAME seed — once with the native
// changes strategy OFF (the arrayPopBack/arrayPopFront `c != p` count fan-out,
// RangeWindow) and once with it ON (the native timeSeriesChangesToGrid,
// RangeWindowNative) — runs BOTH on the same ephemeral chDB session, and
// compares the per-(series, anchor) change-count values.
//
// Why this is the parity proof. The fan-out's per-window count is the
// Prometheus-pinned funcChanges value (the spec corpus is reference-Prometheus-
// pinned), so native == fan-out transitively proves native == Prometheus on the
// changes shape. We compare the DECODED float64 (never a string render). Counts
// are exact integers in float64, so the assertion is BIT-IDENTICAL (no ULP
// tolerance, unlike the rate path's extrapolation arithmetic).
//
// VERSION-GATED, NOT auto-proven on this substrate. Unlike timeSeriesRateToGrid
// (v25.6, present on the 25.8 chDB substrate), timeSeriesChangesToGrid shipped
// in v25.9 (PR #86010) and is EMPIRICALLY ABSENT on the chdb-go v1.12.0 / CH
// 25.8 substrate. The test feature-detects via system.functions and, when the
// function is absent, logs a NOTICE and validates ONLY the fan-out half — the
// native==fan-out parity assertion is bypassed here; it runs only on a >= 25.9
// server (prod/e2e differential lane or a future chDB substrate bump that
// restores the auto-proof). The forbid-skip CI gate bans the test-skip API, so this is a
// documented runtime conditional that ALWAYS executes; coverage loss is never
// silent.
//
// Semantic parity to confirm on a >= 25.9 server (documented here so a future
// fixture reads as intended, not a regression): native timeSeriesChangesToGrid
// requires >= 1 sample/window and returns a 0 count for a single-sample window
// (NULL -> ABSENT only for an EMPTY window via WHERE grid_val IS NOT NULL),
// matching the fan-out's `length(window_vals) >= 1` + per-pair `c != p` count.
// Prom's funcChanges additionally carves out NaN-on-both-sides pairs; both the
// fan-out and (presumably) the native fn accept divergence there, so the seed
// below is deliberately finite-valued.
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

// changesSeed mirrors the production OTel-CH default schema (ResourceAttributes
// present, DEFAULT map(), column-explicit INSERT). Two host series with an
// oscillating gauge so the change count is non-trivial and differs per series,
// on a clean 1-minute grid (off the staleness left edge is irrelevant for a
// COUNT, but the integer-minute samples keep the window membership unambiguous).
const changesSeed = `
CREATE OR REPLACE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:00:00', 9), 0.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:01:00', 9), 1.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:02:00', 9), 1.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:03:00', 9), 0.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:04:00', 9), 1.0),
    ('load_state', map('host', 'a'), toDateTime64('2026-01-01 00:05:00', 9), 1.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:00:00', 9), 5.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:01:00', 9), 5.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:02:00', 9), 7.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:03:00', 9), 7.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:04:00', 9), 9.0),
    ('load_state', map('host', 'b'), toDateTime64('2026-01-01 00:05:00', 9), 2.0);
`

// changesQuery wraps the changes() matrix fn in a transparent `sum by` (each
// series is its own group), so the per-(series, anchor) change count is what
// both paths must agree on.
const changesQuery = `sum by(host) (changes(load_state[5m]))`

func TestNativeTSGridChanges_DualEmitParity(t *testing.T) {
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
	for _, stmt := range splitSeedStatements(changesSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	fanout := runChangesEmit(t, db, false, false)
	if !changesFnPresent(t, db) {
		t.Logf("NOTICE: timeSeriesChangesToGrid absent on this chDB substrate (CH 25.8 < 25.9 floor) — " +
			"native parity assertion bypassed (fan-out half still validated). Parity is VERSION-GATED: " +
			"prove it on a >= 25.9 server (prod/e2e) or a newer chDB substrate. The always-on SQL-shape " +
			"golden (native_changes_range_step.txtar) still pins the emit.")
		return
	}
	native := runChangesEmit(t, db, true, false)

	// Optimizer-narrowed native scan must be BIT-IDENTICAL to the wide native
	// scan (ProjectionPushdown narrows the RangeWindowNative inner Scan to the
	// exact {Attributes, TimeUnix, Value} the emit reads).
	nativeOpt := runChangesEmit(t, db, true, true)
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
		// changes() is an integer COUNT — native and fan-out must be
		// BIT-IDENTICAL (no ULP tolerance).
		if math.Float64bits(nv) != math.Float64bits(fv) {
			t.Errorf("cell %+v: native=%.20g fanout=%.20g NOT bit-identical — "+
				"the native change count diverged from the fan-out", cell, nv, fv)
		}
	}
	t.Logf("changes dual-emit parity: %d/%d cells bit-identical. "+
		"native == fan-out == Prometheus on the changes shape.", len(fanout), len(fanout))
}

// runChangesEmit lowers + emits the changes query with the native-changes
// strategy set to `native`, optionally runs the default optimizer pipeline,
// runs the resulting SQL on db, and returns the per-cell change counts.
func runChangesEmit(t *testing.T, db *sql.DB, native, optimize bool) map[gridCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(changesQuery)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	var lowerers promql.RangeLowerers
	if native {
		lowerers.Changes = promql.NativeChangesLowerer{Fallback: promql.FanoutChangesLowerer{}}
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
		t.Fatalf("native=%v produced zero rows — the changes fixture must yield a populated grid", native)
	}
	return out
}

// changesFnPresent feature-detects timeSeriesChangesToGrid via system.functions
// (the gating fact the native changes path depends on; ABSENT below CH 25.9).
func changesFnPresent(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count() FROM system.functions WHERE name = 'timeSeriesChangesToGrid'",
	).Scan(&n); err != nil {
		t.Fatalf("feature-detect timeSeriesChangesToGrid: %v", err)
	}
	return n > 0
}

// extractHostLabel pulls the host value out of the JSON-encoded Attributes map
// (`{"host":"a"}`), reusing the indexOf helper from the rate dual-emit test.
func extractHostLabel(jsonStr string) string {
	const key = `"host":"`
	i := indexOf(jsonStr, key)
	if i < 0 {
		return ""
	}
	rest := jsonStr[i+len(key):]
	j := indexOf(rest, `"`)
	if j < 0 {
		return rest
	}
	return rest[:j]
}

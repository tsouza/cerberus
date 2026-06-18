//go:build chdb

// chDB-backed dual-emit parity pin for the experimental
// timeSeriesResampleToGridWithStaleness lowering (chplan.RangeWindowResample).
//
// The test lowers the SAME range-mode bare instant-vector selector (`up` over
// query_range) TWICE against the SAME seed — once with the native-staleness
// strategy OFF (the argMax sample-fan-out, RangeLWR) and once with it ON (the
// native timeSeriesResampleToGridWithStaleness, RangeWindowResample) — runs
// BOTH on the same ephemeral chDB session, and compares the per-(series,
// anchor) selected value.
//
// Why this is the parity proof. The fan-out's per-anchor value is the
// Prometheus-pinned LWR pick (the spec corpus is reference-Prometheus-pinned),
// so native == fan-out transitively proves native == Prometheus on the
// staleness shape. We compare the DECODED float64 (never a string render).
//
// Feature-detect, not a test-skip. timeSeriesResampleToGridWithStaleness is
// gated behind the timeSeries*ToGrid family floor (CH v25.6.0). The chDB
// substrate is probed once per run via system.functions; the native assertion
// only fires when the function is present (true on the current 25.8 substrate).
// When absent, the fan-out half still runs and a notice is logged so the
// coverage loss is never silent. The forbid-skip CI gate bans the test-skip
// API, so this is a documented runtime conditional that always executes.
//
// Window-edge note. RangeLWR uses a half-open `(anchor - lookback, anchor]`
// membership window (strict left edge); the native function uses the closed
// `[anchor - lookback, anchor]`. They diverge ONLY on a sample landing exactly
// on the left boundary — a measure-zero, nanosecond-exact coincidence. The seed
// below is DELIBERATELY off-boundary (samples at :30 / :2:30 against a 1m grid)
// so both paths agree; the divergence is documented on
// chplan.RangeWindowResample, not masked by an epsilon.
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

// resampleSeed mirrors the production OTel-CH default schema (ResourceAttributes
// present, DEFAULT map(), column-explicit INSERT) — see the rate dual-emit seed
// for the rationale. Two series (api, web) with off-boundary samples so the
// closed-vs-half-open left-edge distinction never bites.
const resampleSeed = `
CREATE OR REPLACE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('up', map('job', 'api'), toDateTime64('2026-01-01 00:00:30', 9), 1.0),
    ('up', map('job', 'api'), toDateTime64('2026-01-01 00:02:30', 9), 7.0),
    ('up', map('job', 'api'), toDateTime64('2026-01-01 00:07:10', 9), 9.0),
    ('up', map('job', 'web'), toDateTime64('2026-01-01 00:01:15', 9), 2.0),
    ('up', map('job', 'web'), toDateTime64('2026-01-01 00:05:45', 9), 5.0);
`

// resampleQuery is a bare instant-vector selector over query_range — the
// staleness shape. The grid spans 10 minutes at 1m step so both an active
// staleness carry-forward and a staleness gap (the leading anchors before the
// first sample, and any anchor > 5m past the last sample) are exercised.
const resampleQuery = `up`

// resampleCell keys a selected value by (job-label, anchor timestamp).
type resampleCell struct {
	job    string
	anchor string
}

func TestNativeTSGridResample_DualEmitParity(t *testing.T) {
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
	for _, stmt := range splitSeedStatements(resampleSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	fanout := runResampleEmit(t, db, false, false)
	if !resampleFnPresent(t, db) {
		t.Logf("NOTICE: timeSeriesResampleToGridWithStaleness absent on this chDB substrate — " +
			"native parity assertion bypassed (fan-out half still validated). " +
			"Coverage is reduced but the always-on SQL-shape golden still pins the emit.")
		return
	}
	native := runResampleEmit(t, db, true, false)

	// Optimizer-narrowed native scan must be BIT-IDENTICAL to the wide native
	// scan. ProjectionPushdown narrows the RangeWindowResample inner Scan to the
	// exact {MetricName, Attributes, TimeUnix, Value} the emit reads; dropping
	// any of those identity/grid-input columns would 502 or silently change the
	// grid. This proves the narrowing changes NEITHER the row set NOR a value.
	nativeOpt := runResampleEmit(t, db, true, true)
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
				"the scan narrowing changed a value (the narrowing is WRONG)", cell, ov, wv)
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
		// Resample selects an EXISTING sample value (no extrapolation
		// arithmetic), so the native and fan-out picks must be BIT-IDENTICAL —
		// no ULP tolerance, unlike the rate path.
		if math.Float64bits(nv) != math.Float64bits(fv) {
			t.Errorf("cell %+v: native=%.20g fanout=%.20g NOT bit-identical — "+
				"the native staleness pick diverged from the argMax fan-out", cell, nv, fv)
		}
	}
	t.Logf("resample dual-emit parity: %d/%d cells bit-identical. "+
		"native == fan-out == Prometheus on the staleness shape.", len(fanout), len(fanout))
}

// runResampleEmit lowers + emits the resample query with the native-staleness
// strategy set to `native`, optionally runs the default optimizer pipeline,
// runs the resulting SQL on db, and returns the per-cell selected values.
func runResampleEmit(t *testing.T, db *sql.DB, native, optimize bool) map[resampleCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(resampleQuery)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(10 * time.Minute)
	var lowerers promql.RangeLowerers
	if native {
		lowerers.Staleness = promql.NativeStalenessLowerer{}
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		rangeStart, rangeEnd, time.Minute,
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
		t.Fatalf("native=%v produced zero rows — the resample fixture must yield a populated grid", native)
	}
	return out
}

// resampleFnPresent feature-detects timeSeriesResampleToGridWithStaleness via
// system.functions (the gating fact the native path depends on).
func resampleFnPresent(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count() FROM system.functions WHERE name = 'timeSeriesResampleToGridWithStaleness'",
	).Scan(&n); err != nil {
		t.Fatalf("feature-detect timeSeriesResampleToGridWithStaleness: %v", err)
	}
	return n > 0
}

// extractJobLabel pulls the job value out of the JSON-encoded Attributes map
// (`{"job":"api"}`), reusing the indexOf helper from the rate dual-emit test.
func extractJobLabel(jsonStr string) string {
	const key = `"job":"`
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

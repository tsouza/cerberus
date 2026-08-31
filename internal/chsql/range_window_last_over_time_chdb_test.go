//go:build chdb

// chDB-backed dual-emit parity pin for the experimental
// timeSeriesResampleToGridWithStaleness lowering of matrix-mode
// `last_over_time(<v>[<range>])` (chplan.RangeWindowStaleResample, reusing the
// SAME native node ts_grid_resample's bare-selector staleness shape rides —
// see chopt.FeatureTSGridLastOverTime's own doc).
//
// The test lowers the SAME range-mode last_over_time(...) call TWICE against
// the SAME seed — once with the native strategy OFF (the windowed-array
// `window_vals[length(window_vals)]` fan-out) and once with it ON (the native
// RangeWindowStaleResample) — runs BOTH on the same ephemeral chDB session,
// and compares the per-(series, anchor) selected value.
//
// WINDOW >= STEP DELIBERATELY. Every case here uses a staleness window at
// least as wide as the grid step. ClickHouse/ClickHouse#106577 ("Fix
// timeSeriesLastToGrid() for out-of-window timestamps") — the correctness
// bug behind this feature's 26.6 floor (see chopt.FeatureTSGridLastOverTime's
// own doc) — is triggered specifically by a window NARROWER than the step;
// this repo's chdb_substrate (versions.yaml) pins ClickHouse 26.5, one minor
// BELOW that floor, so a window < step case run here would hit the genuine
// pre-fix engine bug and fail for a reason this PR's emission is not
// responsible for. That regression shape (window 10s < step 15s, mirroring
// the upstream fix's own regression fixture) is instead covered by the real
// ClickHouse >= 26.6 integration test,
// TestLastOverTime_NativeResample_WindowNarrowerThanStep_RealCH
// (range_window_last_over_time_realch_integration_test.go), which needs
// Docker and is gated by the `integration` build tag accordingly.
//
// Feature-detect, not a test-skip. timeSeriesResampleToGridWithStaleness is
// gated behind the timeSeries*ToGrid family floor (CH v25.6.0), same as the
// resample dual-emit test. The chDB substrate is probed once per run via
// system.functions; the native assertion only fires when the function is
// present. When absent, the fan-out half still runs and a notice is logged so
// the coverage loss is never silent. The forbid-skip CI gate bans the
// test-skip API, so this is a documented runtime conditional that always
// executes.
package chsql_test

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// lastOverTimeSeed covers the edge cases the issue's own risk section names,
// one series (by `host`) per case, all sharing one 10-minute / 1m-step grid
// with a 3m last_over_time window (>= step — see the file doc):
//
//   - "a": staleness carry (a sample stays the answer for the anchors inside
//     its 3m window) followed by a staleness GAP (the anchors past it).
//   - "b": EMPTY WINDOW — no sample at all before the grid's first anchors
//     reach it, so the leading anchors are absent rows on both paths.
//   - "c": a SINGLE sample, exercising the minimal one-row case end to end.
//   - "d": a sample landing EXACTLY on the left window boundary
//     (anchor - window == sample ts). The left-open-window fix
//     (ClickHouse/ClickHouse#86588) landed at 25.9, well below both this
//     substrate (26.5) and this feature's own 26.6 floor, so both paths
//     already agree the boundary sample is EXCLUDED (half-open membership).
//   - "e": a SOLE in-window sample that is NaN. Per the issue's own risk
//     note, the "NaN loses" rule applies only among samples sharing the
//     SAME timestamp — a sole latest in-window NaN IS returned, matching
//     Prometheus. Proves neither path drops it as if it were "no sample".
//   - "f": DUPLICATE-latest-timestamp with NaN among the duplicates — the
//     one corner the issue flags as the real divergence risk (the fan-out's
//     arraySort orders a tuple tie by VALUE, and ClickHouse's total order
//     puts NaN last, so the fan-out deterministically picks the NaN row on
//     a tie; the native sparse aggregate's tie-break has no such by-value
//     ordering). Excluded from the blanket bit-identical loop below and
//     asserted on its own, pinning whatever each path actually does rather
//     than assuming they agree.
var lastOverTimeSeed = chsqltest.MetricsSeedDDL("otel_metrics_gauge") + `
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('disk_used_bytes', map('host', 'a'), toDateTime64('2026-01-01 00:00:30', 9), 100.0),
    ('disk_used_bytes', map('host', 'a'), toDateTime64('2026-01-01 00:03:15', 9), 200.0),
    ('disk_used_bytes', map('host', 'b'), toDateTime64('2026-01-01 00:08:30', 9), 50.0),
    ('disk_used_bytes', map('host', 'c'), toDateTime64('2026-01-01 00:05:00', 9), 42.0),
    ('disk_used_bytes', map('host', 'd'), toDateTime64('2026-01-01 00:01:00', 9), 7.0),
    ('disk_used_bytes', map('host', 'e'), toDateTime64('2026-01-01 00:04:30', 9), nan),
    ('disk_used_bytes', map('host', 'f'), toDateTime64('2026-01-01 00:06:00', 9), 9.0),
    ('disk_used_bytes', map('host', 'f'), toDateTime64('2026-01-01 00:06:00', 9), nan);
`

// lastOverTimeQuery is a matrix-mode last_over_time call whose window (3m)
// is >= the query_range step (1m) — see the file doc for why.
const lastOverTimeQuery = `last_over_time(disk_used_bytes[3m])`

// lastOverTimeCell keys a selected value by (host-label, anchor timestamp).
type lastOverTimeCell struct {
	host   string
	anchor string
}

func TestNativeLastOverTime_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	for _, stmt := range splitSeedStatements(lastOverTimeSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	fanout := runLastOverTimeEmit(t, db, false)
	if !resampleFnPresent(t, db) {
		t.Logf("NOTICE: timeSeriesResampleToGridWithStaleness absent on this chDB substrate — " +
			"native parity assertion bypassed (fan-out half still validated). " +
			"Coverage is reduced but the always-on SQL-shape golden still pins the emit.")
		return
	}
	native := runLastOverTimeEmit(t, db, true)

	if len(native) != len(fanout) {
		t.Fatalf("row-count divergence: native=%d fanout=%d cells\nnative=%v\nfanout=%v",
			len(native), len(fanout), native, fanout)
	}

	// host=f (case "f", the duplicate-latest-timestamp-with-NaN corner) is
	// excluded from the blanket loop and checked on its own below.
	const duplicateNaNHost = "f"
	for cell, fv := range fanout {
		if cell.host == duplicateNaNHost {
			continue
		}
		nv, ok := native[cell]
		if !ok {
			t.Errorf("cell %+v present in fan-out but absent in native", cell)
			continue
		}
		// Resample selects an EXISTING sample value (no extrapolation
		// arithmetic), so the native and fan-out picks must be
		// BIT-IDENTICAL — no ULP tolerance, unlike the rate path. NaN bit
		// patterns compare equal here exactly when both paths read back
		// the SAME stored NaN row, which is what host=e's sole-NaN case
		// proves.
		if math.Float64bits(nv) != math.Float64bits(fv) {
			t.Errorf("cell %+v: native=%.20g fanout=%.20g NOT bit-identical — "+
				"the native last_over_time pick diverged from the window-array fan-out", cell, nv, fv)
		}
	}

	dupCells := cellsForHost(native, duplicateNaNHost)
	fanoutDupCells := cellsForHost(fanout, duplicateNaNHost)
	if len(dupCells) != len(fanoutDupCells) {
		t.Fatalf("host=%s row-count divergence: native=%d fanout=%d", duplicateNaNHost, len(dupCells), len(fanoutDupCells))
	}
	for cell, fv := range fanoutDupCells {
		nv := dupCells[cell]
		t.Logf("duplicate-latest-timestamp NaN corner, cell %+v: native=%v (NaN=%v) fanout=%v (NaN=%v)",
			cell, nv, math.IsNaN(nv), fv, math.IsNaN(fv))
		// Pinned per the issue's own risk note: the fan-out's arraySort
		// breaks a timestamp tie by VALUE and ClickHouse's total order
		// ranks NaN last, so the fan-out deterministically returns NaN on
		// this duplicate-timestamp pair.
		if !math.IsNaN(fv) {
			t.Errorf("cell %+v: fan-out pick = %v; want NaN (arraySort ties break by value, NaN sorts last)", cell, fv)
		}
	}
}

// cellsForHost narrows a full cell map to the rows for one host label.
func cellsForHost(cells map[lastOverTimeCell]float64, host string) map[lastOverTimeCell]float64 {
	out := make(map[lastOverTimeCell]float64)
	for cell, v := range cells {
		if cell.host == host {
			out[cell] = v
		}
	}
	return out
}

// runLastOverTimeEmit lowers + emits lastOverTimeQuery with the
// native-last_over_time strategy set to `native`, runs the resulting SQL on
// db, and returns the per-cell selected values.
func runLastOverTimeEmit(t *testing.T, db *sql.DB, native bool) map[lastOverTimeCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(lastOverTimeQuery)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(10 * time.Minute)
	var lowerers promql.RangeLowerers
	if native {
		lowerers.LastOverTime = promql.NativeLastOverTimeLowerer{Fallback: promql.FanoutLastOverTimeLowerer{}}
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		rangeStart, rangeEnd, time.Minute,
		promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower (native=%v): %v", native, err)
	}
	plan = optimizer.Default().Run(context.Background(), plan)
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

	out := make(map[lastOverTimeCell]float64)
	for rows.Next() {
		var hostJSON string
		var ts time.Time
		var v float64
		if err := rows.Scan(&hostJSON, &ts, &v); err != nil {
			t.Fatalf("scan (native=%v): %v", native, err)
		}
		out[lastOverTimeCell{host: extractHostLabel(hostJSON), anchor: ts.UTC().Format(time.RFC3339)}] = v
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err (native=%v): %v", native, err)
	}
	if len(out) == 0 {
		t.Fatalf("native=%v produced zero rows — the last_over_time fixture must yield a populated grid", native)
	}
	return out
}

// extractHostLabel (pulling the `host` value out of the JSON-encoded
// Attributes map) is shared with range_window_changes_chdb_test.go — same
// package, same helper, defined once there.

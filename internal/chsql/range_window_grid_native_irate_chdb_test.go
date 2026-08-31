//go:build chdb

// chDB-backed dual-emit parity pin for the experimental native `irate()`
// lowering (chplan.RangeWindowGridNative, Func="irate"). Mirrors
// range_window_grid_native_delta_chdb_test.go's delta() proof exactly,
// swapping the function: the native path renders timeSeriesInstantRateToGrid
// directly, reducing every window to its trailing pair rather than folding
// the whole window the way rate()/delta() do.
//
// Why this is the parity proof. The fan-out's expected_rows are already
// Prometheus-pinned (test/spec/promql's corpus), so native == fan-out
// transitively proves native == Prometheus. We compare the DECODED float64
// (never a string render) at full precision.
//
// This is the DIFFERENTIAL half of cerberus issue #2746's sweep — the
// standalone semantic probes (counter-reset correction, trailing-pair-only
// selection, NULL threshold, window membership, duplicate-timestamp
// handling) live in chopt.FeatureTSGridIrate's own doc; this test proves
// the two EMITTERS agree on the SAME monotonic-counter-shaped seed
// rate()/increase()/delta() already use, over the full query_range grid.
//
// AggregationTemporalityColumn is CLEARED on the schema, unlike the
// rate()/increase() dual-emit tests. This is load-bearing, not cosmetic:
// irate() (unlike delta()) DOES set RangeWindow.TemporalityColumn whenever
// AggregationTemporalityColumn is configured and the selector routes singly
// to the Sum table (rangeVectorCounterTemporalityColumn) — and
// nativeTSGridMatrixNode's unconditional TemporalityColumn guard would then
// send the WHOLE window to the fan-out fallback, native side never firing
// at all (see NativeIrateLowerer's own doc, and cerberus issue #2803, which
// tracks that this is currently the realistic-schema default). Clearing the
// column here is what makes this test exercise the native path against the
// otel_metrics_sum seed every other dual-emit sibling in this package
// shares.
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

// dualEmitIrateQuery is the irate() sibling of dualEmitQuery (rate()) /
// dualEmitDeltaQuery (delta()), over the SAME seed dualEmitSeed provisions.
const dualEmitIrateQuery = `sum by(cerberus_ql) (irate(cerberus_queries_total[5m]))`

// maxDualEmitIrateUlpDivergentCells / maxDualEmitIrateUlp are the measured
// ceilings on the dual-emit fixture. Measured against the real chDB
// substrate: 18/18 grid cells bit-identical, 0 ULP divergence anywhere —
// timeSeriesInstantRateToGrid's trailing-pair arithmetic (one division, no
// extrapolation/clamp branch) has fewer opportunities to reorder floating
// point than rate()'s whole-window extrapolatedRate, so it lands on the
// SAME divide the fan-out's window_pairs[length]/[length-1] path computes.
// The ceilings are pinned at the measured values (0 divergent cells; 1 ULP
// retained as the per-cell ceiling documenting how much slack would be
// tolerated, though none is used) rather than loosened for headroom — a
// future divergence fails loudly as a real regression.
const (
	maxDualEmitIrateUlp               = 1
	maxDualEmitIrateUlpDivergentCells = 0
)

func TestNativeTSGridIrate_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	for _, stmt := range splitSeedStatements(dualEmitSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	hasFn := tsGridIrateFnPresent(t, db)
	fanout := runDualEmitIrate(t, db, false, false)
	if !hasFn {
		t.Logf("NOTICE: timeSeriesInstantRateToGrid absent on this chDB substrate — " +
			"native parity assertion bypassed (fan-out half still validated). " +
			"Coverage is reduced but the always-on SQL-shape golden still pins the emit.")
		return
	}
	native := runDualEmitIrate(t, db, true, false)

	// Optimizer-narrowed native scan must be BIT-IDENTICAL to the wide native
	// scan — same claim range_window_grid_native_chdb_test.go proves for
	// rate(), re-proven here because irate() reaches ProjectionPushdown
	// through the identical RangeWindowGridNative node type.
	nativeOpt := runDualEmitIrate(t, db, true, true)
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
				"the scan narrowing changed an irate value (the narrowing is WRONG)", cell, ov, wv)
		}
	}
	t.Logf("scan-narrowing parity: %d/%d optimized-native cells bit-identical to wide native.", len(native), len(native))

	if len(native) != len(fanout) {
		t.Fatalf("row-count divergence: native=%d fanout=%d cells", len(native), len(fanout))
	}

	var (
		ulpDivergent int
		maxSeenUlp   uint64
	)
	for cell, fv := range fanout {
		nv, ok := native[cell]
		if !ok {
			t.Errorf("cell %+v present in fan-out but absent in native", cell)
			continue
		}
		if math.Float64bits(nv) == math.Float64bits(fv) {
			continue // bit-identical — allowed, not required
		}
		ulps := ulpDistance(nv, fv)
		if ulps > maxSeenUlp {
			maxSeenUlp = ulps
		}
		if ulps > maxDualEmitIrateUlp {
			t.Errorf("cell %+v: native=%.20g fanout=%.20g differ by %d ULP (> %d — arithmetic bug, not float-order noise)",
				cell, nv, fv, ulps, maxDualEmitIrateUlp)
			continue
		}
		ulpDivergent++
		t.Logf("cell %+v: native=%.20g fanout=%.20g differ by %d ULP (within budget %d)",
			cell, nv, fv, ulps, maxDualEmitIrateUlp)
	}
	if ulpDivergent > maxDualEmitIrateUlpDivergentCells {
		t.Errorf("ULP divergence grew to %d cells; documented bound is %d — "+
			"the native arithmetic drifted further from the fan-out than expected, investigate",
			ulpDivergent, maxDualEmitIrateUlpDivergentCells)
	}
	t.Logf("dual-emit parity: %d/%d cells bit-identical, %d cells differ by at most %d ULP (max seen %d), "+
		"within documented bounds (%d cells / %d ULP). native == fan-out == Prometheus to full observable precision.",
		len(fanout)-ulpDivergent, len(fanout), ulpDivergent, maxDualEmitIrateUlp, maxSeenUlp,
		maxDualEmitIrateUlpDivergentCells, maxDualEmitIrateUlp)
}

// runDualEmitIrate mirrors runDualEmit (range_window_grid_native_chdb_test.go)
// exactly, wiring RangeLowerers.Irate instead of .Rate.
func runDualEmitIrate(t *testing.T, db *sql.DB, native, optimize bool) map[gridCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(dualEmitIrateQuery)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	var lowerers promql.RangeLowerers
	if native {
		lowerers.Irate = promql.NativeIrateLowerer{Fallback: promql.FanoutIrateLowerer{}}
	}
	// AggregationTemporalityColumn cleared — see the file doc for why this
	// one, unlike rate()/delta()'s own dual-emit siblings, is load-bearing
	// for irate(): with it set, rw.TemporalityColumn would be non-empty and
	// nativeTSGridMatrixNode's guard would send every window to the fan-out
	// fallback, so the native=true half would silently prove nothing.
	s := schema.DefaultOTelMetrics()
	s.AggregationTemporalityColumn = ""
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s,
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
	wrapped := "SELECT toJSONString(`Attributes`) AS ql_json, `TimeUnix`, `Value` FROM (" + sqlStr + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query (native=%v): %v\nSQL: %s", native, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[gridCell]float64)
	for rows.Next() {
		var qlJSON string
		var ts time.Time
		var v float64
		if err := rows.Scan(&qlJSON, &ts, &v); err != nil {
			t.Fatalf("scan (native=%v): %v", native, err)
		}
		out[gridCell{ql: extractQLLabel(qlJSON), anchor: ts.UTC().Format(time.RFC3339)}] = v
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err (native=%v): %v", native, err)
	}
	if len(out) == 0 {
		t.Fatalf("native=%v produced zero rows — the dual-emit fixture must yield a populated grid", native)
	}
	return out
}

// tsGridIrateFnPresent feature-detects timeSeriesInstantRateToGrid via
// system.functions (the gating fact the native irate path depends on).
func tsGridIrateFnPresent(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count() FROM system.functions WHERE name = 'timeSeriesInstantRateToGrid'",
	).Scan(&n); err != nil {
		t.Fatalf("feature-detect timeSeriesInstantRateToGrid: %v", err)
	}
	return n > 0
}

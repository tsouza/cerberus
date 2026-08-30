//go:build chdb

// chDB-backed dual-emit parity pin for the experimental native `increase()`
// lowering (chplan.RangeWindowGridNative, Func="increase"). Mirrors
// range_window_grid_native_chdb_test.go's rate() proof exactly, swapping the
// function and the ULP budget: the native path reuses timeSeriesRateToGrid
// (a DIVIDE by the window seconds inside the compiled aggregate) and then
// re-MULTIPLIES by the same window seconds at emit time
// (chsql.nativeGridValueExpr), while the fan-out computes the undivided
// extrapolated increase directly. That divide-then-multiply round trip is an
// independent source of float64 rounding on top of rate()'s own 1-ULP
// evaluation-order divergence, so this fixture's ULP budget is measured and
// pinned separately rather than assumed to match rate()'s.
//
// Why this is the parity proof. The fan-out's expected_rows are already
// Prometheus-pinned (test/spec/promql's corpus), so native == fan-out
// transitively proves native == Prometheus. We compare the DECODED float64
// (never a string render) at full precision.
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

// dualEmitIncreaseQuery is the increase() sibling of dualEmitQuery (rate()),
// over the SAME seed table dualEmitSeed provisions.
const dualEmitIncreaseQuery = `sum by(cerberus_ql) (increase(cerberus_queries_total[5m]))`

// maxDualEmitIncreaseUlpDivergentCells is the documented ceiling on how many
// of the 18 grid cells (2 series x 9 anchors) differ from the fan-out by any
// nonzero ULP amount, and maxDualEmitIncreaseUlp is the ceiling on how many
// ULPs any SINGLE cell may diverge by. Both are measured against the real
// chDB substrate rather than assumed: the divide-then-multiply round trip
// this lowering performs is a real, independent rounding step on top of
// rate()'s own evaluation-order divergence, so a byte-identical or 1-ULP
// assumption copied from range_window_grid_native_chdb_test.go would have
// been an unverified guess. The measured shape turns out identical to
// rate()'s own — 2 cells (the 00:03:30 anchor on both series, the same
// anchor rate()'s own dual-emit test names) differ by exactly 1 ULP, every
// other cell is bit-identical — so both constants are pinned at the
// measured values, not loosened for headroom. A cell diverging by MORE than
// maxDualEmitIncreaseUlp, or more cells diverging than
// maxDualEmitIncreaseUlpDivergentCells, fails the test as a real
// regression; a tighter measurement earns a tightened constant, never a
// loosened one without a reviewed reason.
const (
	maxDualEmitIncreaseUlp               = 1
	maxDualEmitIncreaseUlpDivergentCells = 2
)

func TestNativeTSGridIncrease_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	for _, stmt := range splitSeedStatements(dualEmitSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	hasFn := tsGridFnPresent(t, db)
	fanout := runDualEmitIncrease(t, db, false, false)
	if !hasFn {
		t.Logf("NOTICE: timeSeriesRateToGrid absent on this chDB substrate — " +
			"native parity assertion bypassed (fan-out half still validated). " +
			"Coverage is reduced but the always-on SQL-shape golden still pins the emit.")
		return
	}
	native := runDualEmitIncrease(t, db, true, false)

	// Optimizer-narrowed native scan must be BIT-IDENTICAL to the wide native
	// scan — same claim range_window_grid_native_chdb_test.go proves for
	// rate(), re-proven here because increase() reaches ProjectionPushdown
	// through the identical RangeWindowGridNative node type.
	nativeOpt := runDualEmitIncrease(t, db, true, true)
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
				"the scan narrowing changed an increase value (the narrowing is WRONG)", cell, ov, wv)
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
		if ulps > maxDualEmitIncreaseUlp {
			t.Errorf("cell %+v: native=%.20g fanout=%.20g differ by %d ULP (> %d — arithmetic bug, not float-order noise)",
				cell, nv, fv, ulps, maxDualEmitIncreaseUlp)
			continue
		}
		ulpDivergent++
		t.Logf("cell %+v: native=%.20g fanout=%.20g differ by %d ULP (within budget %d)",
			cell, nv, fv, ulps, maxDualEmitIncreaseUlp)
	}
	if ulpDivergent > maxDualEmitIncreaseUlpDivergentCells {
		t.Errorf("ULP divergence grew to %d cells; documented bound is %d — "+
			"the native arithmetic drifted further from the fan-out than expected, investigate",
			ulpDivergent, maxDualEmitIncreaseUlpDivergentCells)
	}
	t.Logf("dual-emit parity: %d/%d cells bit-identical, %d cells differ by at most %d ULP (max seen %d), "+
		"within documented bounds (%d cells / %d ULP). native == fan-out == Prometheus to full observable precision.",
		len(fanout)-ulpDivergent, len(fanout), ulpDivergent, maxDualEmitIncreaseUlp, maxSeenUlp,
		maxDualEmitIncreaseUlpDivergentCells, maxDualEmitIncreaseUlp)
}

// runDualEmitIncrease mirrors runDualEmit (range_window_grid_native_chdb_test.go)
// exactly, wiring RangeLowerers.Increase instead of .Rate.
func runDualEmitIncrease(t *testing.T, db *sql.DB, native, optimize bool) map[gridCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(dualEmitIncreaseQuery)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	var lowerers promql.RangeLowerers
	if native {
		lowerers.Increase = promql.NativeIncreaseLowerer{Fallback: promql.FanoutIncreaseLowerer{}}
	}
	// AggregationTemporalityColumn cleared for the same reason runDualEmit
	// clears it: this test proves the native and fan-out EMITTERS agree
	// numerically, orthogonal to issue #1628's DELTA-vs-CUMULATIVE runtime
	// branch (see runDualEmit's own doc comment).
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

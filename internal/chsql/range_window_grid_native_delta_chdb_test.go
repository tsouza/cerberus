//go:build chdb

// chDB-backed dual-emit parity pin for the experimental native `delta()`
// lowering (chplan.RangeWindowGridNative, Func="delta"). Mirrors
// range_window_grid_native_chdb_test.go's rate() proof exactly, swapping the
// function: the native path renders timeSeriesDeltaToGrid directly (no
// divide-then-multiply round trip the way increase() rides rate's own
// aggregate), so this fixture's ULP budget is measured and pinned
// independently rather than assumed to match rate()'s or increase()'s.
//
// Why this is the parity proof. The fan-out's expected_rows are already
// Prometheus-pinned (test/spec/promql's corpus), so native == fan-out
// transitively proves native == Prometheus. We compare the DECODED float64
// (never a string render) at full precision.
//
// This is the DIFFERENTIAL half of cerberus issue #2745's sweep — the
// standalone semantic probes (counter-reset non-correction, extrapolation
// boundary/clamp, NaN propagation, duplicate-timestamp handling) live in
// chopt.FeatureTSGridDelta's own doc; this test proves the two EMITTERS
// agree on the SAME monotonic-counter-shaped seed rate()/increase() already
// use, over the full query_range grid.
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

// dualEmitDeltaQuery is the delta() sibling of dualEmitQuery (rate()) /
// dualEmitIncreaseQuery (increase()), over the SAME seed dualEmitSeed
// provisions.
const dualEmitDeltaQuery = `sum by(cerberus_ql) (delta(cerberus_queries_total[5m]))`

// maxDualEmitDeltaUlpDivergentCells / maxDualEmitDeltaUlp are the measured
// ceilings on the dual-emit fixture: how many of the 18 grid cells (2 series
// x 9 anchors) differ from the fan-out by any nonzero ULP amount, and the
// ceiling on how many ULPs any SINGLE cell may diverge by. Measured against
// the real chDB substrate: unlike rate() (1 ULP on 2/18 cells) and increase()
// (the SAME 1 ULP on the SAME 2/18 cells — its divide-then-multiply round
// trip happens to land on the identical rounding), delta()'s
// timeSeriesDeltaToGrid renders NO divide step at all — it is a pure
// extrapolated subtraction, the SAME shape the fan-out's own arithmetic
// takes — and the measured result is 18/18 cells BIT-IDENTICAL, 0 ULP
// divergence anywhere. The ceilings are pinned at the measured values (0
// divergent cells; 1 ULP retained as the per-cell ceiling documenting how
// much slack would be tolerated before failing, though none is used) rather
// than loosened for headroom — a future divergence fails loudly as a real
// regression, not silently inside an unused budget.
const (
	maxDualEmitDeltaUlp               = 1
	maxDualEmitDeltaUlpDivergentCells = 0
)

func TestNativeTSGridDelta_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	for _, stmt := range splitSeedStatements(dualEmitSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	hasFn := tsGridDeltaFnPresent(t, db)
	fanout := runDualEmitDelta(t, db, false, false)
	if !hasFn {
		t.Logf("NOTICE: timeSeriesDeltaToGrid absent on this chDB substrate — " +
			"native parity assertion bypassed (fan-out half still validated). " +
			"Coverage is reduced but the always-on SQL-shape golden still pins the emit.")
		return
	}
	native := runDualEmitDelta(t, db, true, false)

	// Optimizer-narrowed native scan must be BIT-IDENTICAL to the wide native
	// scan — same claim range_window_grid_native_chdb_test.go proves for
	// rate(), re-proven here because delta() reaches ProjectionPushdown
	// through the identical RangeWindowGridNative node type.
	nativeOpt := runDualEmitDelta(t, db, true, true)
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
				"the scan narrowing changed a delta value (the narrowing is WRONG)", cell, ov, wv)
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
		if ulps > maxDualEmitDeltaUlp {
			t.Errorf("cell %+v: native=%.20g fanout=%.20g differ by %d ULP (> %d — arithmetic bug, not float-order noise)",
				cell, nv, fv, ulps, maxDualEmitDeltaUlp)
			continue
		}
		ulpDivergent++
		t.Logf("cell %+v: native=%.20g fanout=%.20g differ by %d ULP (within budget %d)",
			cell, nv, fv, ulps, maxDualEmitDeltaUlp)
	}
	if ulpDivergent > maxDualEmitDeltaUlpDivergentCells {
		t.Errorf("ULP divergence grew to %d cells; documented bound is %d — "+
			"the native arithmetic drifted further from the fan-out than expected, investigate",
			ulpDivergent, maxDualEmitDeltaUlpDivergentCells)
	}
	t.Logf("dual-emit parity: %d/%d cells bit-identical, %d cells differ by at most %d ULP (max seen %d), "+
		"within documented bounds (%d cells / %d ULP). native == fan-out == Prometheus to full observable precision.",
		len(fanout)-ulpDivergent, len(fanout), ulpDivergent, maxDualEmitDeltaUlp, maxSeenUlp,
		maxDualEmitDeltaUlpDivergentCells, maxDualEmitDeltaUlp)
}

// runDualEmitDelta mirrors runDualEmit (range_window_grid_native_chdb_test.go)
// exactly, wiring RangeLowerers.Delta instead of .Rate.
func runDualEmitDelta(t *testing.T, db *sql.DB, native, optimize bool) map[gridCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(dualEmitDeltaQuery)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	var lowerers promql.RangeLowerers
	if native {
		lowerers.Delta = promql.NativeDeltaLowerer{Fallback: promql.FanoutDeltaLowerer{}}
	}
	// AggregationTemporalityColumn cleared for the same reason runDualEmit
	// clears it — though delta() itself never reads it (NativeDeltaLowerer
	// carries no temporality union-split, unlike rate/increase), this keeps
	// the seed schema identical across all three dual-emit siblings.
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

// tsGridDeltaFnPresent feature-detects timeSeriesDeltaToGrid via
// system.functions (the gating fact the native delta path depends on).
func tsGridDeltaFnPresent(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count() FROM system.functions WHERE name = 'timeSeriesDeltaToGrid'",
	).Scan(&n); err != nil {
		t.Fatalf("feature-detect timeSeriesDeltaToGrid: %v", err)
	}
	return n > 0
}

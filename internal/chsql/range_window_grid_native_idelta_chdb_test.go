//go:build chdb

// chDB-backed dual-emit parity pin for the experimental native `idelta()`
// lowering (chplan.RangeWindowGridNative, Func="idelta"). Mirrors
// range_window_grid_native_irate_chdb_test.go's irate() proof exactly,
// swapping the aggregate: the native path renders
// timeSeriesInstantDeltaToGrid, the trailing-pair difference with NO
// counter-reset correction (unlike irate's repaired trailing-pair rate).
//
// Why this is the parity proof. The fan-out's expected_rows are already
// Prometheus-pinned (test/spec/promql's corpus), so native == fan-out
// transitively proves native == Prometheus. We compare the DECODED float64
// (never a string render) at full precision.
//
// This is the DIFFERENTIAL half of cerberus issue #2746's sweep — the
// standalone semantic probes live in chopt.FeatureTSGridIdelta's own doc;
// this test proves the two EMITTERS agree on the SAME seed
// rate()/increase()/delta()/irate() already use, over the full query_range
// grid.
//
// Unlike irate(), idelta() is gauge-only in PromQL and
// counterTemporalityRangeFn never sets RangeWindow.TemporalityColumn for it
// (see that function's own doc) — so, unlike its irate() sibling, this test
// does NOT need to clear AggregationTemporalityColumn for the native path to
// fire; it is cleared anyway purely to keep the seed schema identical across
// every dual-emit sibling in this package.
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

// dualEmitIdeltaQuery is the idelta() sibling of dualEmitIrateQuery, over the
// SAME seed dualEmitSeed provisions.
const dualEmitIdeltaQuery = `sum by(cerberus_ql) (idelta(cerberus_queries_total[5m]))`

// maxDualEmitIdeltaUlpDivergentCells / maxDualEmitIdeltaUlp are the measured
// ceilings on the dual-emit fixture, pinned at the measured values rather
// than loosened for headroom — a future divergence fails loudly as a real
// regression, not silently inside an unused budget.
const (
	maxDualEmitIdeltaUlp               = 1
	maxDualEmitIdeltaUlpDivergentCells = 0
)

func TestNativeTSGridIdelta_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec("SET " + chclient.SettingExperimentalTSGridAggregate + " = 1"); err != nil {
		t.Fatalf("enable experimental ts-grid: %v", err)
	}
	for _, stmt := range splitSeedStatements(dualEmitSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	hasFn := tsGridIdeltaFnPresent(t, db)
	fanout := runDualEmitIdelta(t, db, false, false)
	if !hasFn {
		t.Logf("NOTICE: timeSeriesInstantDeltaToGrid absent on this chDB substrate — " +
			"native parity assertion bypassed (fan-out half still validated). " +
			"Coverage is reduced but the always-on SQL-shape golden still pins the emit.")
		return
	}
	native := runDualEmitIdelta(t, db, true, false)

	nativeOpt := runDualEmitIdelta(t, db, true, true)
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
				"the scan narrowing changed an idelta value (the narrowing is WRONG)", cell, ov, wv)
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
		if ulps > maxDualEmitIdeltaUlp {
			t.Errorf("cell %+v: native=%.20g fanout=%.20g differ by %d ULP (> %d — arithmetic bug, not float-order noise)",
				cell, nv, fv, ulps, maxDualEmitIdeltaUlp)
			continue
		}
		ulpDivergent++
		t.Logf("cell %+v: native=%.20g fanout=%.20g differ by %d ULP (within budget %d)",
			cell, nv, fv, ulps, maxDualEmitIdeltaUlp)
	}
	if ulpDivergent > maxDualEmitIdeltaUlpDivergentCells {
		t.Errorf("ULP divergence grew to %d cells; documented bound is %d — "+
			"the native arithmetic drifted further from the fan-out than expected, investigate",
			ulpDivergent, maxDualEmitIdeltaUlpDivergentCells)
	}
	t.Logf("dual-emit parity: %d/%d cells bit-identical, %d cells differ by at most %d ULP (max seen %d), "+
		"within documented bounds (%d cells / %d ULP). native == fan-out == Prometheus to full observable precision.",
		len(fanout)-ulpDivergent, len(fanout), ulpDivergent, maxDualEmitIdeltaUlp, maxSeenUlp,
		maxDualEmitIdeltaUlpDivergentCells, maxDualEmitIdeltaUlp)
}

// runDualEmitIdelta mirrors runDualEmitIrate exactly, wiring
// RangeLowerers.Idelta instead of .Irate.
func runDualEmitIdelta(t *testing.T, db *sql.DB, native, optimize bool) map[gridCell]float64 {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(dualEmitIdeltaQuery)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	var lowerers promql.RangeLowerers
	if native {
		lowerers.Idelta = promql.NativeIdeltaLowerer{Fallback: promql.FanoutIdeltaLowerer{}}
	}
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

// tsGridIdeltaFnPresent feature-detects timeSeriesInstantDeltaToGrid via
// system.functions (the gating fact the native idelta path depends on).
func tsGridIdeltaFnPresent(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count() FROM system.functions WHERE name = 'timeSeriesInstantDeltaToGrid'",
	).Scan(&n); err != nil {
		t.Fatalf("feature-detect timeSeriesInstantDeltaToGrid: %v", err)
	}
	return n > 0
}

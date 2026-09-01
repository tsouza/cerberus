//go:build chdb

package chsql

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file is range_window_fixed_accumulator_chdb_test.go's own sibling for
// cerberus issue #2797's third proposal item: the useAggregateDeltaPrefix
// variant (issue #2389's exact, retention-independent DELTA-prefix aggregate
// mechanism) of the fixed-accumulator decomposition. It lives in package
// chsql, like range_window_delta_prefix_aggregate_matrix_chdb_test.go, rather
// than package chsql_test like range_window_fixed_accumulator_chdb_test.go:
// it needs deltaPrefixCanonicalizedMatrixWindow's direct RangeWindow
// construction (setting DeltaPrefixAggregateInput), which only a
// promql-level DELTA-prefix-aware lowering pass would otherwise reach —
// no such pass exists yet, mirroring why the OTHER #2389 dual-mechanism
// tests (TestDeltaMatrixAggregateSource_*) also bypass promql lowering.
//
// The comparison discipline mirrors TestFixedAccumulatorRateIncrease_DualEmitParity
// (range_window_fixed_accumulator_chdb_test.go): the SAME RangeWindow shape
// (DeltaPrefixAggregateInput set, deltaPrefixReadEnabled=true, so BOTH arms
// resolve useAggregateDeltaPrefix=true) is emitted TWICE — once with
// FixedAccumulatorExtrapolated=false (array-fold,
// deltaMatrixLevelSourceAggregate fed "window_pairs") and once with it true
// (this cut's own emitFixedAccumulatorExtrapolatedMatrix, feeding the SAME
// deltaMatrixLevelSourceAggregate its own scalar accumulator columns
// instead) — proving fixedAccumRegroupLayer's useAggregateDeltaPrefix wiring
// reconstructs the identical DELTA level and rate() value. The fixture's
// window samples are a monotonic (no counter-reset) walk, so reset_sum is
// exactly 0 on both arms and this assertion can stay a tight absolute
// tolerance — mirroring TestDeltaMatrixAggregateSource_UniformLayout's own
// 1e-9 convention in this same package — rather than needing an
// engine-measured ULP budget the way the reset-correction-bearing path in
// range_window_fixed_accumulator_chdb_test.go does.
func TestFixedAccumulatorDeltaPrefixAggregate_DualEmitParity(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	deltaPrefixAggregateSeedDDL(t, db)

	insertDeltaMatrixWindowSamples(t, db, `map('job', 'fx-agg')`, "fx-agg")
	// History strictly before the grid's own day, backfilled into BOTH the
	// base table (the oracle/never-backfilled tests' own raw-remainder
	// source) and the aggregate table (deltaMatrixLevelSourceAggregate's
	// own day-bucket source) — the uniform-layout shape
	// TestDeltaMatrixAggregateSource_UniformLayout already proves matches
	// the unbounded-raw-scan oracle exactly, so it is large enough here to
	// move the reconstructed level (and therefore the counter zero-clamp)
	// away from the window's own tiny raw first sample.
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, map('job', 'fx-agg'), toDateTime64('2026-01-10 00:00:00', 9), 100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum_delta_prefix VALUES
		(map('job', 'fx-agg'), toDateTime64('2026-01-10 00:00:00', 9), 100)`); err != nil {
		t.Fatal(err)
	}

	fanoutWindow := deltaPrefixCanonicalizedMatrixWindow(
		deltaMatrixTestStart, deltaMatrixTestEnd, deltaMatrixTestStep, deltaMatrixTestRange,
		"otel_metrics_sum_delta_prefix",
	)
	fixedWindow := deltaPrefixCanonicalizedMatrixWindow(
		deltaMatrixTestStart, deltaMatrixTestEnd, deltaMatrixTestStep, deltaMatrixTestRange,
		"otel_metrics_sum_delta_prefix",
	)
	fixedWindow.FixedAccumulatorExtrapolated = true

	fanout := runDeltaMatrixPrefixQuery(t, db, fanoutWindow, 0, true)["fx-agg"]
	fixed := runDeltaMatrixPrefixQuery(t, db, fixedWindow, 0, true)["fx-agg"]

	if len(fanout) == 0 {
		t.Fatal("array-fold (useAggregateDeltaPrefix) produced zero rows — fixture must yield a populated grid")
	}
	if len(fixed) != len(fanout) {
		t.Fatalf("row-count divergence: fanout=%d fixed=%d\nfanout=%v\nfixed=%v", len(fanout), len(fixed), fanout, fixed)
	}
	for anchorMs, fv := range fanout {
		xv, ok := fixed[anchorMs]
		if !ok {
			t.Errorf("anchor %d: present in array-fold but absent in fixed-accumulator", anchorMs)
			continue
		}
		if math.IsNaN(fv) || math.IsNaN(xv) {
			t.Errorf("anchor %d: non-finite value: fanout=%v fixed=%v", anchorMs, fv, xv)
			continue
		}
		if math.Abs(fv-xv) > 1e-9 {
			t.Errorf("anchor %d: array-fold=%v, fixed-accumulator=%v — useAggregateDeltaPrefix reconstruction must match exactly", anchorMs, fv, xv)
		}
	}

	// Belt-and-suspenders: the reconstructed level itself (first_val), not
	// just the post-clamp rate() value — see runDeltaMatrixPrefixLevelQuery's
	// own doc on why a Value-only match alone can coincidentally agree
	// despite a genuinely different reconstruction (clamp saturation).
	//
	// The array-fold arm reads runDeltaMatrixPrefixLevelQuery unchanged — its
	// own "extrap" layer's first_val IS the fully DELTA-reconstructed value
	// (range_window.go's mid-layer deltaFirstValFrag already folds
	// delta_anchor_levels into it). The fixed-accumulator arm needs its own
	// reader (runFixedAccumMatrixPrefixLevelQuery below): its "extrap" layer
	// deliberately keeps first_val RAW — fixedAccumCounterDeltaFrag's
	// CUMULATIVE telescoping term (last_val - first_val) needs the raw value
	// too, so the reconstruction is folded into the final Value expression
	// inline (fixedAccumClampFirstValFrag) rather than materialized as its
	// own first_val column. Reading raw first_val here would silently strip
	// the reconstruction back out and defeat this exact check.
	fanoutLevel := runDeltaMatrixPrefixLevelQuery(t, db, fanoutWindow, 0, true)["fx-agg"]
	fixedLevel := runFixedAccumMatrixPrefixLevelQuery(t, db, fixedWindow, 0, true)["fx-agg"]
	if len(fixedLevel) != len(fanoutLevel) {
		t.Fatalf("level row-count divergence: fanout=%d fixed=%d\nfanout=%v\nfixed=%v", len(fanoutLevel), len(fixedLevel), fanoutLevel, fixedLevel)
	}
	for anchorMs, fv := range fanoutLevel {
		xv, ok := fixedLevel[anchorMs]
		if !ok {
			t.Errorf("anchor %d: level present in array-fold but absent in fixed-accumulator", anchorMs)
			continue
		}
		if math.Abs(fv-xv) > 1e-9 {
			t.Errorf("anchor %d: array-fold first_val=%v, fixed-accumulator first_val=%v — reconstructed DELTA level must match exactly", anchorMs, fv, xv)
		}
	}
	t.Logf("useAggregateDeltaPrefix dual-emit parity: %d/%d anchors match (array-fold == fixed-accumulator, both Value and reconstructed first_val)", len(fanout), len(fanout))
}

// runFixedAccumMatrixPrefixLevelQuery is runDeltaMatrixPrefixLevelQuery's
// fixed-accumulator sibling. It cannot reuse that helper unchanged: the
// array-fold emitter's own "extrap" layer already carries the fully
// DELTA-reconstructed value under the name first_val (range_window.go's
// mid-layer deltaFirstValFrag folds delta_anchor_levels into it before
// extrap ever sees it), but the fixed-accumulator emitter's "extrap" layer
// (range_window_fixed_accumulator.go's emitFixedAccumulatorExtrapolatedMatrix)
// deliberately keeps first_val RAW — fixedAccumCounterDeltaFrag's own
// CUMULATIVE telescoping term (last_val - first_val) reads that same column
// and needs the RAW window-first value, not the reconstructed level — so the
// reconstruction is instead folded into the final Value expression inline,
// via fixedAccumClampFirstValFrag, and never materialized as its own column.
// This helper reproduces that exact fold (temporality, delta_anchor_levels,
// first_val — all three already pass through extrap, see
// emitFixedAccumulatorExtrapolatedMatrix's needsDeltaFirstLevel branch) in
// the wrapping SQL so it reads the SAME reconstructed quantity
// runDeltaMatrixPrefixLevelQuery reads off the array-fold arm.
func runFixedAccumMatrixPrefixLevelQuery(
	t *testing.T, db *sql.DB, r *chplan.RangeWindow, lookback time.Duration, readEnabled bool,
) map[string]map[int64]float64 {
	t.Helper()
	ctx := WithDeltaPrefixLookback(context.Background(), lookback)
	ctx = WithDeltaPrefixReadEnabled(ctx, readEnabled)
	sqlText, args, err := Emit(ctx, r)
	if err != nil {
		t.Fatalf("Emit(lookback=%s, readEnabled=%v): %v", lookback, readEnabled, err)
	}
	inner := strings.TrimSuffix(strings.TrimSpace(sqlText), ";")
	const outerFromMarker = " FROM ("
	idx := strings.Index(inner, outerFromMarker)
	if idx == -1 {
		t.Fatalf("runFixedAccumMatrixPrefixLevelQuery: no outer FROM found in:\n%s", inner)
	}
	rest := inner[idx+len(outerFromMarker):]
	wrapped := fmt.Sprintf(
		"SELECT Attributes['job'] AS job, toUnixTimestamp64Milli(anchor_ts) AS anchor_ms, "+
			"if(%s = %d, %s + %s, %s) AS first_val FROM (%s",
		windowTemporalityAlias, schema.AggregationTemporalityDelta,
		deltaAnchorLevelsAlias, fixedAccumFirstValAlias, fixedAccumFirstValAlias,
		rest,
	)
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("level query(lookback=%s, readEnabled=%v): %v\n%s", lookback, readEnabled, err, wrapped)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[int64]float64{}
	for rows.Next() {
		var job string
		var anchorMs int64
		var firstVal float64
		if err := rows.Scan(&job, &anchorMs, &firstVal); err != nil {
			t.Fatalf("level scan(lookback=%s, readEnabled=%v): %v", lookback, readEnabled, err)
		}
		if out[job] == nil {
			out[job] = map[int64]float64{}
		}
		out[job][anchorMs] = firstVal
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("level rows(lookback=%s, readEnabled=%v): %v", lookback, readEnabled, err)
	}
	return out
}

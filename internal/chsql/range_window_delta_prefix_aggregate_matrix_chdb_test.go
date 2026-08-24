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
)

// This file is the query_range (matrix) sibling of
// range_window_delta_prefix_aggregate_chdb_test.go — cerberus issue #2389's
// remaining "matrix-path equivalent alongside deltaMatrixLevelSource" task.
// It reuses that file's fixture helpers (deltaPrefixAggregateSeedDDL,
// deltaPrefixCanonicalizedWindow, explainEstimateRowsForTable,
// stripPresenceGuard, mapKeys — same package, same build tag) so both
// mechanisms are pinned against the identical table shapes and
// canonicalisation tower.
//
// The comparison discipline mirrors the instant file exactly: an "oracle"
// run reads deltaMatrixLevelSource's OWN (pre-#2389) approximation with
// WithDeltaPrefixLookback(ctx, 0) — the lookback bound disabled, i.e. an
// UNBOUNDED scan of otel_metrics_sum for DELTA history, which is exact
// because it is unbounded. deltaMatrixLevelSourceAggregate is exercised by
// pointing r.DeltaPrefixAggregateInput at a real otel_metrics_sum_delta_prefix
// table and enabling DeltaPrefixReadEnabled.

// deltaPrefixCanonicalizedMatrixWindow is deltaPrefixCanonicalizedWindow's
// query_range sibling: the SAME canonicalised-Attributes rate() window, with
// Start/Step/OuterRange set so the emitter takes the matrix
// (emitWindowedArrayExtrapolatedMatrix) path instead of the instant one.
func deltaPrefixCanonicalizedMatrixWindow(start, end time.Time, step, rng time.Duration, aggTable string) *chplan.RangeWindow {
	r := deltaPrefixCanonicalizedWindow(end, rng, aggTable)
	r.Start = start
	r.Step = step
	r.OuterRange = end.Sub(start)
	return r
}

// runDeltaMatrixPrefixQuery is runDeltaPrefixQuery's query_range sibling:
// returns job -> (anchor unix millis -> rate() value) for every row.
func runDeltaMatrixPrefixQuery(
	t *testing.T, db *sql.DB, r *chplan.RangeWindow, lookback time.Duration, readEnabled bool, settings ...string,
) map[string]map[int64]float64 {
	t.Helper()
	ctx := WithDeltaPrefixLookback(context.Background(), lookback)
	ctx = WithDeltaPrefixReadEnabled(ctx, readEnabled)
	sqlText, args, err := Emit(ctx, r)
	if err != nil {
		t.Fatalf("Emit(lookback=%s, readEnabled=%v): %v", lookback, readEnabled, err)
	}
	inner := strings.TrimSuffix(strings.TrimSpace(sqlText), ";")
	wrapped := "SELECT Attributes['job'] AS job, toUnixTimestamp64Milli(anchor_ts) AS anchor_ms, Value " +
		"FROM (" + inner + ")"
	if len(settings) > 0 {
		wrapped += " SETTINGS " + strings.Join(settings, ", ")
	}
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query(lookback=%s, readEnabled=%v): %v\n%s", lookback, readEnabled, err, sqlText)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[int64]float64{}
	for rows.Next() {
		var job string
		var anchorMs int64
		var value float64
		if err := rows.Scan(&job, &anchorMs, &value); err != nil {
			t.Fatalf("scan(lookback=%s, readEnabled=%v): %v", lookback, readEnabled, err)
		}
		if out[job] == nil {
			out[job] = map[int64]float64{}
		}
		out[job][anchorMs] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows(lookback=%s, readEnabled=%v): %v", lookback, readEnabled, err)
	}
	return out
}

// runDeltaMatrixPrefixLevelQuery peels the outermost extrapolation-value
// projection off r's emitted matrix SQL and reads the "extrap" layer's own
// first_val column directly — the reconstructed DELTA level
// (deltaAnchorLevelsAlias) plus the window's own raw first sample value,
// exactly what deltaFirstValFrag substitutes for a DELTA series — instead
// of the final rate() Value.
//
// This exists because extrapolatedValueExpr's counter zero-crossing clamp
// (`least(duration_to_start, sampled_interval*first_val/counter_delta)`)
// can SATURATE: once the comparand it computes from first_val exceeds the
// raw duration_to_start, the clamp picks the SAME raw value regardless of
// exactly how much bigger first_val is. Two runs whose reconstructed
// levels genuinely differ (say, one missing 999 units of un-backfilled
// history) can therefore land on the identical unclamped branch and emit
// the IDENTICAL final Value — a real coincidence, not evidence the
// mechanism is broken, but also not evidence a test asserting "these must
// differ" can rely on. first_val is not subject to that saturation: it is
// read straight off the reconstruction, before the clamp's own
// least()/comparand arithmetic runs.
func runDeltaMatrixPrefixLevelQuery(
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
		t.Fatalf("runDeltaMatrixPrefixLevelQuery: no outer FROM found in:\n%s", inner)
	}
	rest := inner[idx+len(outerFromMarker):]
	wrapped := "SELECT Attributes['job'] AS job, toUnixTimestamp64Milli(anchor_ts) AS anchor_ms, first_val " +
		"FROM (" + rest
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

// deltaMatrixTestGrid is the shared 2-anchor grid every scenario below
// uses: anchors at 2026-01-15 11:59:30 and 2026-01-15 12:00:00 (step 30s,
// range 1m), landing squarely inside deltaPrefixTestStart's day so
// toStartOfDay(rangeStart) for both anchors is 2026-01-15 00:00:00.
var (
	deltaMatrixTestEnd   = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	deltaMatrixTestStart = deltaMatrixTestEnd.Add(-30 * time.Second)
	deltaMatrixTestStep  = 30 * time.Second
	deltaMatrixTestRange = time.Minute
)

// insertDeltaMatrixWindowSamples inserts the three evenly 30s-spaced
// in-window DELTA samples every scenario shares, giving BOTH grid anchors
// exactly 2 in-window samples with a 30s gap to their own window's left
// edge: anchor1 (11:59:30, window (11:58:30,11:59:30]) sees
// 11:59:00/11:59:30; anchor2 (12:00:00, window (11:59:00,12:00:00]) sees
// 11:59:30/12:00:00. Both anchors' raw duration_to_start (30s) therefore
// sit BELOW the extrapolation-threshold clamp (avg 30s * 1.1 = 33s), so —
// unlike an uneven spacing, which can saturate that clamp regardless of the
// reconstructed level and mask a real discrepancy — the counter
// zero-crossing clamp (extrapolatedValueExpr's own `least(duration_to_start,
// sampled_interval*first_val/counter_delta)`) stays the one thing a
// mismatched reconstructed level can move, at every anchor.
func insertDeltaMatrixWindowSamples(t *testing.T, db *sql.DB, attrsSQL, job string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, ` + attrsSQL + `, toDateTime64('2026-01-15 11:59:00', 9), 2),
		(1, ` + attrsSQL + `, toDateTime64('2026-01-15 11:59:30', 9), 3),
		(1, ` + attrsSQL + `, toDateTime64('2026-01-15 12:00:00', 9), 4)`); err != nil {
		t.Fatalf("insert matrix window samples for %s: %v", job, err)
	}
}

// TestDeltaMatrixAggregateSource_UniformLayout is the matrix-path control:
// history fully, uniformly backfilled into both tables, no Map key-order
// variance, both grid anchors landing on the SAME day as the window. Every
// anchor's reconstructed level must match the oracle exactly.
func TestDeltaMatrixAggregateSource_UniformLayout(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	deltaPrefixAggregateSeedDDL(t, db)

	insertDeltaMatrixWindowSamples(t, db, `map('job', 'uniform')`, "uniform")
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, map('job', 'uniform'), toDateTime64('2026-01-10 00:00:00', 9), 100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum_delta_prefix VALUES
		(map('job', 'uniform'), toDateTime64('2026-01-10 00:00:00', 9), 100)`); err != nil {
		t.Fatal(err)
	}

	window := deltaPrefixCanonicalizedMatrixWindow(
		deltaMatrixTestStart, deltaMatrixTestEnd, deltaMatrixTestStep, deltaMatrixTestRange, "",
	)
	oracle := runDeltaMatrixPrefixQuery(t, db, window, 0, false)["uniform"]

	gotWindow := deltaPrefixCanonicalizedMatrixWindow(
		deltaMatrixTestStart, deltaMatrixTestEnd, deltaMatrixTestStep, deltaMatrixTestRange,
		"otel_metrics_sum_delta_prefix",
	)
	got := runDeltaMatrixPrefixQuery(t, db, gotWindow, 0, true)["uniform"]

	if len(oracle) != 2 {
		t.Fatalf("oracle: got %d anchors, want 2 (%v)", len(oracle), oracle)
	}
	if len(got) != 2 {
		t.Fatalf("new mechanism: got %d anchors, want 2 (%v)", len(got), got)
	}
	for anchorMs, oracleVal := range oracle {
		gotVal, ok := got[anchorMs]
		if !ok {
			t.Fatalf("new mechanism: no row for anchor %d", anchorMs)
		}
		if math.Abs(oracleVal-gotVal) > 1e-9 {
			t.Errorf("anchor %d: new mechanism = %v, oracle (unbounded raw scan) = %v — must match exactly", anchorMs, gotVal, oracleVal)
		}
	}
}

// TestDeltaMatrixAggregateSource_NeverBackfilledSeries is
// TestDeltaPrefixAggregateSource_NeverBackfilledSeries's matrix sibling: a
// series with real history in otel_metrics_sum but NO row ever copied into
// otel_metrics_sum_delta_prefix must still answer at every anchor — a
// finite, sane (documented) under-count relative to the oracle, never a
// dropped anchor, crash, or NaN/Inf.
func TestDeltaMatrixAggregateSource_NeverBackfilledSeries(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	deltaPrefixAggregateSeedDDL(t, db)

	insertDeltaMatrixWindowSamples(t, db, `map('job', 'neverbackfilled')`, "neverbackfilled")
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, map('job', 'neverbackfilled'), toDateTime64('2026-01-10 00:00:00', 9), 999)`); err != nil {
		t.Fatal(err)
	}

	// Compare via runDeltaMatrixPrefixLevelQuery (first_val), not the final
	// rate() Value: extrapolatedValueExpr's counter zero-crossing clamp can
	// saturate two genuinely different reconstructed levels onto the SAME
	// unclamped branch (both landing on the raw duration_to_start), so a
	// Value-only comparison can coincidentally match even though the
	// under-count is real — see runDeltaMatrixPrefixLevelQuery's own doc.
	oracle := runDeltaMatrixPrefixLevelQuery(t, db, deltaPrefixCanonicalizedMatrixWindow(
		deltaMatrixTestStart, deltaMatrixTestEnd, deltaMatrixTestStep, deltaMatrixTestRange, "",
	), 0, false)["neverbackfilled"]
	got := runDeltaMatrixPrefixLevelQuery(t, db, deltaPrefixCanonicalizedMatrixWindow(
		deltaMatrixTestStart, deltaMatrixTestEnd, deltaMatrixTestStep, deltaMatrixTestRange,
		"otel_metrics_sum_delta_prefix",
	), 0, true)["neverbackfilled"]

	if len(oracle) != 2 {
		t.Fatalf("oracle: got %d anchors, want 2 (%v)", len(oracle), oracle)
	}
	if len(got) != 2 {
		t.Fatalf("new mechanism: got %d anchors, want 2 (%v) — a never-backfilled series must not lose an anchor", len(got), got)
	}
	// The 999-unit Jan-10 sample is old enough (well before either anchor's
	// own day, and every window sample here stays on Jan 15, so the raw
	// day-reset term's own same-day contribution is identical between
	// oracle and got) that it can ONLY reach the reconstructed level via the
	// aggregate table — which never-backfilled leaves empty. The gap
	// between oracle's and got's first_val must therefore be EXACTLY 999 at
	// every anchor: not less (the aggregate term silently seeing history it
	// should not), and not more (the raw day-reset term under-counting
	// today's own same-day contribution too).
	const missingHistory = 999.0
	for anchorMs, oracleVal := range oracle {
		gotVal, ok := got[anchorMs]
		if !ok {
			t.Fatalf("new mechanism: no row for anchor %d", anchorMs)
		}
		if math.IsNaN(gotVal) || math.IsInf(gotVal, 0) {
			t.Fatalf("anchor %d: new mechanism produced a non-finite reconstructed level for a never-backfilled series: %v", anchorMs, gotVal)
		}
		if gap := oracleVal - gotVal; math.Abs(gap-missingHistory) > 1e-9 {
			t.Errorf("anchor %d: new mechanism first_val=%v, oracle first_val=%v (gap=%v) — want gap == %v exactly "+
				"(the un-backfilled Jan-10 history alone); any other gap means either the aggregate term saw history "+
				"it should not, or the raw day-reset term under- or over-counts today's own same-day contribution",
				anchorMs, gotVal, oracleVal, gap, missingHistory)
		}
	}
}

// TestDeltaMatrixAggregateSource_JoinUseNulls1DoesNotPropagateNull is
// TestDeltaPrefixAggregateSource_JoinUseNulls1DoesNotPropagateNull's matrix
// sibling. deltaMatrixLevelSourceAggregate's ONLY join is the final LEFT
// JOIN onto the sparse aggLevels relation (the raw side is a plain window
// function, never a join, so it cannot NULL-miss) — this proves that one
// join's ifNull guard holds under join_use_nulls=1 for a series that misses
// it (never backfilled).
func TestDeltaMatrixAggregateSource_JoinUseNulls1DoesNotPropagateNull(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	deltaPrefixAggregateSeedDDL(t, db)

	insertDeltaMatrixWindowSamples(t, db, `map('job', 'nulljoin_agg_miss')`, "nulljoin_agg_miss")
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, map('job', 'nulljoin_agg_miss'), toDateTime64('2026-01-10 00:00:00', 9), 999)`); err != nil {
		t.Fatal(err)
	}

	window := deltaPrefixCanonicalizedMatrixWindow(
		deltaMatrixTestStart, deltaMatrixTestEnd, deltaMatrixTestStep, deltaMatrixTestRange,
		"otel_metrics_sum_delta_prefix",
	)
	underJoinUseNulls := runDeltaMatrixPrefixQuery(t, db, window, 0, true, "join_use_nulls = 1")["nulljoin_agg_miss"]
	defaultSettings := runDeltaMatrixPrefixQuery(t, db, window, 0, true)["nulljoin_agg_miss"]

	if len(underJoinUseNulls) != 2 {
		t.Fatalf("join_use_nulls=1: got %d anchors, want 2 (%v) — NULL likely propagated through the join and dropped a row", len(underJoinUseNulls), underJoinUseNulls)
	}
	for anchorMs, want := range defaultSettings {
		got, ok := underJoinUseNulls[anchorMs]
		if !ok {
			t.Fatalf("anchor %d: missing under join_use_nulls=1", anchorMs)
		}
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("anchor %d: non-finite value under join_use_nulls=1: %v", anchorMs, got)
		}
		if math.Abs(want-got) > 1e-9 {
			t.Errorf("anchor %d: join_use_nulls=1 changed the answer: got %v, join_use_nulls=0 got %v", anchorMs, got, want)
		}
	}
}

// TestDeltaMatrixAggregateSource_PresenceGuardPrunesCumulativeOnlyScan is
// TestDeltaPrefixAggregateSource_PresenceGuardPrunesCumulativeOnlyScan's
// matrix sibling: for a CUMULATIVE-only series the aggregate stream's own
// day-bucket scan (deltaMatrixLevelSourceAggregate's aggDaily query) must be
// pruned to near-zero read_rows, while the SAME emitted query with only the
// guard clause(s) removed (stripPresenceGuard) reads the full seeded corpus.
func TestDeltaMatrixAggregateSource_PresenceGuardPrunesCumulativeOnlyScan(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	deltaPrefixAggregateSeedDDL(t, db)

	// CUMULATIVE (AggregationTemporality=2) in-window samples on the SAME
	// 2-anchor grid every other scenario uses.
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(2, map('job', 'cumonly'), toDateTime64('2026-01-15 11:59:00', 9), 2),
		(2, map('job', 'cumonly'), toDateTime64('2026-01-15 11:59:30', 9), 3),
		(2, map('job', 'cumonly'), toDateTime64('2026-01-15 11:59:45', 9), 3.5),
		(2, map('job', 'cumonly'), toDateTime64('2026-01-15 12:00:00', 9), 4)`); err != nil {
		t.Fatal(err)
	}

	seedSQL := fmt.Sprintf(`INSERT INTO otel_metrics_sum_delta_prefix
SELECT
    map('job', concat('s', toString(number %% %d))) AS Attributes,
    toDateTime64('2026-01-15 00:00:00', 9) - toIntervalDay(number %% 30 + 1) AS BucketStart,
    toFloat64(number) AS PartialSum
FROM numbers(%d)`, deltaPrefixCumulativeOnlyAggregateSeriesCount, deltaPrefixCumulativeOnlyAggregateRows)
	if _, err := db.Exec(seedSQL); err != nil {
		t.Fatalf("seed aggregate corpus: %v", err)
	}

	r := deltaPrefixCanonicalizedMatrixWindow(
		deltaMatrixTestStart, deltaMatrixTestEnd, deltaMatrixTestStep, deltaMatrixTestRange,
		"otel_metrics_sum_delta_prefix",
	)
	ctx := WithDeltaPrefixReadEnabled(context.Background(), true)
	guardedSQL, _, err := Emit(ctx, r)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	guardedSQL = strings.TrimSuffix(strings.TrimSpace(guardedSQL), ";")
	unguardedSQL := stripPresenceGuard(t, guardedSQL)
	if unguardedSQL == guardedSQL {
		t.Fatal("stripPresenceGuard left the SQL unchanged — no guard clause found; fixture/marker drifted from the real matrix emit shape")
	}

	guardedRows := explainEstimateRowsForTable(t, db, guardedSQL, "otel_metrics_sum_delta_prefix")
	unguardedRows := explainEstimateRowsForTable(t, db, unguardedSQL, "otel_metrics_sum_delta_prefix")
	t.Logf("otel_metrics_sum_delta_prefix read_rows: guarded=%d unguarded=%d (seeded %d)",
		guardedRows, unguardedRows, deltaPrefixCumulativeOnlyAggregateRows)

	if unguardedRows < deltaPrefixCumulativeOnlyAggregateRows {
		t.Fatalf("unguarded (guard-stripped) read_rows=%d, want >= the seeded %d rows — corpus/EXPLAIN setup did not reproduce a full scan to compare against",
			unguardedRows, deltaPrefixCumulativeOnlyAggregateRows)
	}
	const maxGuardedFraction = 10 // guarded must read < 1/10th of unguarded
	if guardedRows*maxGuardedFraction >= unguardedRows {
		t.Errorf("guarded read_rows=%d not far below unguarded read_rows=%d (want guarded*%d < unguarded) — the matrix presence guard is not pruning the CUMULATIVE-only scan",
			guardedRows, unguardedRows, maxGuardedFraction)
	}
}

// TestDeltaMatrixAggregateSource_CrossDayAnchorAssignmentDoesNotDoubleCount
// is this PR's own adversarial case (not a port of an instant-path test —
// the matrix path's day-reset raw stream has no instant-path analogue to
// mirror): the bug this test is written to catch is a raw sample whose
// first-eligible anchor (deltaPrefixAnchorArrayFrag's plain
// "first anchor at or after ts" assignment, with NO same-day restriction)
// lands on a DIFFERENT, LATER day than the sample's own — which would count
// that sample TWICE once its own day is also fully backfilled into the
// aggregate table: once via the (wrongly unrestricted) raw stream at the
// later-day anchor, and again via the aggregate stream's own day-bucket
// term for that same later-day anchor.
//
// Fixture: a DELTA sample sits at 2026-01-14 23:59:59, with NO anchor on
// 2026-01-14 whose rangeStart is at or after it (the grid's only
// Jan-14 anchor sits at 23:59:30, rangeStart 23:58:30 — before the sample).
// The sample's whole day (Jan 14, value 500) IS correctly backfilled into
// otel_metrics_sum_delta_prefix. deltaPrefixAggregateRawAnchorArrayFrag's
// same-day restriction must therefore DROP this sample from the raw stream
// entirely (its contribution reaches every anchor whose day is > Jan 14
// exclusively through the aggregate stream) — a bug that used
// deltaPrefixAnchorArrayFrag unrestricted here would instead assign it to
// the grid's OTHER (Jan 15) anchor, doubling that anchor's reconstructed
// level to 1000 instead of the correct 500.
func TestDeltaMatrixAggregateSource_CrossDayAnchorAssignmentDoesNotDoubleCount(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	deltaPrefixAggregateSeedDDL(t, db)

	const job = "dayboundary"
	attrs := `map('job', '` + job + `')`
	insertDeltaMatrixWindowSamples(t, db, attrs, job)

	// The adversarial sample: last moment of Jan 14, no Jan-14 anchor can
	// claim it as a prefix contribution.
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, ` + attrs + `, toDateTime64('2026-01-14 23:59:59', 9), 500)`); err != nil {
		t.Fatal(err)
	}
	// Correctly backfilled: the WHOLE of Jan 14 (this sample's entire
	// contribution) copied into the aggregate table's Jan-14 bucket.
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum_delta_prefix VALUES
		(` + attrs + `, toDateTime64('2026-01-14 00:00:00', 9), 500)`); err != nil {
		t.Fatal(err)
	}

	// Grid: anchor1 = Jan 14 23:59:30 (rangeStart 23:58:30, day Jan 14),
	// anchor2 = Jan 15 12:00:00 (rangeStart Jan 15 11:59:00, day Jan 15) —
	// a deliberately wide step so the ONLY anchor on Jan 14 does not reach
	// the adversarial sample, forcing the "first eligible anchor" search
	// past the day boundary. Only anchor2 is the target of this test:
	// insertDeltaMatrixWindowSamples' three window samples all sit on
	// Jan 15, so anchor1 has zero in-window samples and is dropped by
	// windowLenAtLeastFrag(2) — exactly what makes this fixture isolate
	// the adversarial sample's effect on anchor2 alone.
	end := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	start := time.Date(2026, 1, 14, 23, 59, 30, 0, time.UTC)
	step := end.Sub(start)

	oracle := runDeltaMatrixPrefixQuery(t, db, deltaPrefixCanonicalizedMatrixWindow(
		start, end, step, deltaMatrixTestRange, "",
	), 0, false)[job]
	got := runDeltaMatrixPrefixQuery(t, db, deltaPrefixCanonicalizedMatrixWindow(
		start, end, step, deltaMatrixTestRange, "otel_metrics_sum_delta_prefix",
	), 0, true)[job]

	if len(oracle) != 1 {
		t.Fatalf("oracle: got %d anchors, want 1 (only anchor2 has in-window samples) (%v)", len(oracle), oracle)
	}
	if len(got) != 1 {
		t.Fatalf("new mechanism: got %d anchors, want 1 (%v)", len(got), got)
	}
	for anchorMs, oracleVal := range oracle {
		gotVal, ok := got[anchorMs]
		if !ok {
			t.Fatalf("new mechanism: no row for anchor %d", anchorMs)
		}
		if math.Abs(oracleVal-gotVal) > 1e-9 {
			t.Errorf("anchor %d: new mechanism = %v, oracle = %v — a mismatch here means the adversarial "+
				"Jan-14-tail sample was double-counted (or dropped) across the day boundary", anchorMs, gotVal, oracleVal)
		}
	}

	// Belt-and-suspenders: probe the reconstructed level directly too (see
	// runDeltaMatrixPrefixLevelQuery's doc on why a final-Value match alone
	// isn't fully conclusive against a clamp-saturation coincidence — with
	// counter_delta and rawFirst both small integers here and the
	// adversarial contribution a full order of magnitude larger (500), that
	// coincidence is implausible for THIS fixture, but asserting the level
	// directly removes the question entirely). A double-count would show up
	// here as 1000 instead of 500 (an under-drop would show up as 0).
	oracleLevel := runDeltaMatrixPrefixLevelQuery(t, db, deltaPrefixCanonicalizedMatrixWindow(
		start, end, step, deltaMatrixTestRange, "",
	), 0, false)[job]
	gotLevel := runDeltaMatrixPrefixLevelQuery(t, db, deltaPrefixCanonicalizedMatrixWindow(
		start, end, step, deltaMatrixTestRange, "otel_metrics_sum_delta_prefix",
	), 0, true)[job]
	for anchorMs, oracleVal := range oracleLevel {
		gotVal, ok := gotLevel[anchorMs]
		if !ok {
			t.Fatalf("new mechanism: no level row for anchor %d", anchorMs)
		}
		if math.Abs(oracleVal-gotVal) > 1e-9 {
			t.Errorf("anchor %d: new mechanism first_val=%v, oracle first_val=%v — a mismatch here means the "+
				"adversarial Jan-14-tail sample's 500-unit contribution was double-counted (or dropped) across "+
				"the day boundary in the RECONSTRUCTED LEVEL itself", anchorMs, gotVal, oracleVal)
		}
	}
}

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

// This file is Task 6's belt-and-suspenders empirical check (cerberus issue
// #2389, design comment "§4.2 revision", task 6 item 4): the structural
// plan-equality guard lives in internal/promql's
// TestDeltaPrefixAggregateArm_AttributesExprMatchesInput (no chDB needed);
// this file proves the CONSEQUENCE of that identity against real ClickHouse
// — the exact scenario Task 1's chDB spike found NO-GO for the naive
// raw-column join (real Map key-order variance across "redeploys" of one
// logical series) is now GO with deltaPrefixAggregateSource.
//
// Every scenario below builds its RangeWindow with
// deltaPrefixCanonicalizedWindow, which — unlike
// range_window_delta_prefix_lookback_chdb_test.go's instantDeltaPrefixWindow
// — wraps BOTH otel_metrics_sum and (when present)
// otel_metrics_sum_delta_prefix in a Project rebinding Attributes to
// chplan.CanonicalAttributesExpr(Attributes) (mapSort), mirroring
// production's augmentSelectorAttributes / augmentDeltaPrefixAggregateAttributes
// wrap at a reduced scale (no ResourceAttributes merge / ServiceName overlay
// — irrelevant to what this file tests, and independently pinned by
// TestDeltaPrefixAggregateArm_AttributesExprMatchesInput). Both terms
// canonicalising the SAME way, independently, is exactly the property the
// spike's NO-GO finding turned on.
//
// The comparison discipline throughout: an "oracle" run reads ONLY
// otel_metrics_sum with CERBERUS_DELTA_PREFIX_LOOKBACK effectively disabled
// (WithDeltaPrefixLookback(ctx, 0) — the exact pre-#2390 unbounded scan),
// which is allowed to see the table's ENTIRE history because these are tiny
// fixture tables, not production retention. Because old history stays in
// otel_metrics_sum in a real deployment too (backfill COPIES into the
// aggregate table, never deletes from the base one), the oracle and the new
// mechanism read the SAME underlying otel_metrics_sum rows for their
// raw-remainder term — only the SOURCE of the "before today" contribution
// differs (an unbounded raw scan vs. the bucketed aggregate table).
func deltaPrefixCanonicalizedWindow(end time.Time, rng time.Duration, aggTable string) *chplan.RangeWindow {
	r := &chplan.RangeWindow{
		Input: shapedDeltaPrefixInput(
			&chplan.Scan{Table: "otel_metrics_sum"},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "TimeUnix"},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "AggregationTemporality"}, Alias: "AggregationTemporality"},
		),
		Func:              "rate",
		Range:             rng,
		End:               end,
		TimestampColumn:   "TimeUnix",
		ValueColumn:       "Value",
		TemporalityColumn: "AggregationTemporality",
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	if aggTable != "" {
		r.DeltaPrefixAggregateInput = shapedDeltaPrefixInput(
			&chplan.Scan{Table: aggTable},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "BucketStart"}, Alias: "BucketStart"},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "PartialSum"}, Alias: "PartialSum"},
		)
	}
	return r
}

// shapedDeltaPrefixInput wraps scan in a Project rebinding Attributes to its
// canonical (mapSort'd) form, plus whatever extra passthrough columns the
// caller names — see deltaPrefixCanonicalizedWindow's doc for why this
// reduced tower (vs. production's full selectorAttributesExpr) is enough
// for what this file tests.
func shapedDeltaPrefixInput(scan *chplan.Scan, extra ...chplan.Projection) *chplan.Project {
	projections := append([]chplan.Projection{
		{Expr: chplan.CanonicalAttributesExpr(&chplan.ColumnRef{Name: "Attributes"}), Alias: "Attributes"},
	}, extra...)
	return &chplan.Project{Input: scan, Projections: projections}
}

// deltaPrefixAggregateSeedDDL creates the two tables every scenario below
// seeds: the base Sum table (same minimal shape
// range_window_delta_prefix_lookback_chdb_test.go already uses) and the
// DELTA-prefix aggregate table. A plain MergeTree stands in for production's
// AggregatingMergeTree + SimpleAggregateFunction(sum, Float64) — this file
// tests the READ mechanism's join/collapse behaviour, not the DDL's
// background-merge semantics (covered separately by internal/schema/ddl's
// own tests); an ordinary MergeTree's sum() read is byte-identical for that
// purpose.
func deltaPrefixAggregateSeedDDL(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE otel_metrics_sum (
		AggregationTemporality Int32,
		Attributes Map(String, String),
		TimeUnix DateTime64(9),
		Value Float64
	) ENGINE = MergeTree ORDER BY (Attributes, TimeUnix)`); err != nil {
		t.Fatalf("create otel_metrics_sum: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE otel_metrics_sum_delta_prefix (
		Attributes Map(String, String),
		BucketStart DateTime64(9),
		PartialSum Float64
	) ENGINE = MergeTree ORDER BY (Attributes, BucketStart)`); err != nil {
		t.Fatalf("create otel_metrics_sum_delta_prefix: %v", err)
	}
}

// runDeltaPrefixQuery emits r under the given lookback / read-enable
// context values, wraps it to expose the `job` label as a plain string
// column, and returns job -> rate() value for every row. settings, when
// non-empty, appends a query-scoped `SETTINGS <settings>` clause — used by
// TestDeltaPrefixAggregateSource_JoinUseNulls1DoesNotPropagateNull to
// exercise join_use_nulls=1 without mutating the shared chDB session's
// default (which would leak into every later test in this process — chDB's
// session is process-global, see chsqltest.OpenIsolatedChDB's doc).
func runDeltaPrefixQuery(t *testing.T, db *sql.DB, r *chplan.RangeWindow, lookback time.Duration, readEnabled bool, settings ...string) map[string]float64 {
	t.Helper()
	ctx := WithDeltaPrefixLookback(context.Background(), lookback)
	ctx = WithDeltaPrefixReadEnabled(ctx, readEnabled)
	sqlText, args, err := Emit(ctx, r)
	if err != nil {
		t.Fatalf("Emit(lookback=%s, readEnabled=%v): %v", lookback, readEnabled, err)
	}
	inner := strings.TrimSuffix(strings.TrimSpace(sqlText), ";")
	wrapped := "SELECT Attributes['job'] AS job, Value FROM (" + inner + ")"
	if len(settings) > 0 {
		wrapped += " SETTINGS " + strings.Join(settings, ", ")
	}
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query(lookback=%s, readEnabled=%v): %v\n%s", lookback, readEnabled, err, sqlText)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]float64{}
	for rows.Next() {
		var job string
		var value float64
		if err := rows.Scan(&job, &value); err != nil {
			t.Fatalf("scan(lookback=%s, readEnabled=%v): %v", lookback, readEnabled, err)
		}
		out[job] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows(lookback=%s, readEnabled=%v): %v", lookback, readEnabled, err)
	}
	return out
}

// deltaPrefixTestStart / deltaPrefixTestRange are the shared instant-eval
// anchor and rate() window every scenario below uses. rangeStart lands at
// noon so `toStartOfDay(rangeStart)` (2026-01-15 00:00:00) sits comfortably
// between the window's own samples (just before rangeStart) and any
// "several days ago" history a scenario seeds.
var (
	deltaPrefixTestStart = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	deltaPrefixTestRange = time.Minute
)

// insertDeltaPrefixWindowSamples inserts the two in-window DELTA samples
// every scenario shares (job, 11:59:30 value=1, 12:00:00 value=2) — at
// least 2 in-window samples so emitWindowedArrayExtrapolated's
// windowLenAtLeastFrag(2) guard doesn't drop the series.
func insertDeltaPrefixWindowSamples(t *testing.T, db *sql.DB, attrsSQL, job string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, ` + attrsSQL + `, toDateTime64('2026-01-15 11:59:30', 9), 1),
		(1, ` + attrsSQL + `, toDateTime64('2026-01-15 12:00:00', 9), 2)`); err != nil {
		t.Fatalf("insert window samples for %s: %v", job, err)
	}
}

// TestDeltaPrefixAggregateSource_UniformLayout is the simple control: one
// raw-tuple per bucket, no Map key-order variance. The new mechanism's
// term1 (aggregate, "otel_metrics_sum_delta_prefix", days before today) +
// term2 (raw remainder, today's window samples) must match the oracle's
// single unbounded scan of otel_metrics_sum (which sees the SAME history,
// just not split across two tables) exactly.
func TestDeltaPrefixAggregateSource_UniformLayout(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	deltaPrefixAggregateSeedDDL(t, db)

	insertDeltaPrefixWindowSamples(t, db, `map('job', 'uniform')`, "uniform")
	// Old history: present in BOTH otel_metrics_sum (so the oracle's
	// unbounded scan sees it) and otel_metrics_sum_delta_prefix (so the new
	// mechanism's aggregate term sees it) — a correctly-completed backfill
	// COPIES, never deletes, from the base table.
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, map('job', 'uniform'), toDateTime64('2026-01-10 00:00:00', 9), 100)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum_delta_prefix VALUES
		(map('job', 'uniform'), toDateTime64('2026-01-10 00:00:00', 9), 100)`); err != nil {
		t.Fatal(err)
	}

	oracle := runDeltaPrefixQuery(t, db, deltaPrefixCanonicalizedWindow(deltaPrefixTestStart, deltaPrefixTestRange, ""), 0, false)
	got := runDeltaPrefixQuery(t, db, deltaPrefixCanonicalizedWindow(deltaPrefixTestStart, deltaPrefixTestRange, "otel_metrics_sum_delta_prefix"), 0, true)

	oracleVal, ok := oracle["uniform"]
	if !ok {
		t.Fatal("oracle: no row for job=uniform")
	}
	gotVal, ok := got["uniform"]
	if !ok {
		t.Fatal("new mechanism: no row for job=uniform")
	}
	if math.Abs(oracleVal-gotVal) > 1e-9 {
		t.Errorf("new mechanism = %v, oracle (unbounded raw scan) = %v — must match exactly", gotVal, oracleVal)
	}
}

// TestDeltaPrefixAggregateSource_MapKeyOrderCollapse reproduces the Task 1
// chDB spike's own NO-GO scenario verbatim (the design comment's spike
// table, middle row): one logical series written by two collector
// "redeploys" that insert the SAME two Map keys in a DIFFERENT order —
// `map('job','collision','region','eu')` vs `map('region','eu','job',
// 'collision')`. A raw ClickHouse GROUP BY over the un-canonicalised Map
// treats these as two distinct groups; deltaPrefixAggregateSource must
// still collapse them into ONE series whose aggregate term sums BOTH
// contributions, matching the oracle exactly.
func TestDeltaPrefixAggregateSource_MapKeyOrderCollapse(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	deltaPrefixAggregateSeedDDL(t, db)

	insertDeltaPrefixWindowSamples(t, db, `map('job', 'collision', 'region', 'eu')`, "collision")
	// Old history for the SAME logical series, split across two buckets and
	// written with SWAPPED Map key-insertion order — the exact collision
	// shape the spike measured (10 raw tuples -> 7 read-time series, this
	// being one of the collapsing groups).
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, map('job', 'collision', 'region', 'eu'), toDateTime64('2026-01-10 00:00:00', 9), 100),
		(1, map('region', 'eu', 'job', 'collision'), toDateTime64('2026-01-14 00:00:00', 9), 50)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum_delta_prefix VALUES
		(map('job', 'collision', 'region', 'eu'), toDateTime64('2026-01-10 00:00:00', 9), 100),
		(map('region', 'eu', 'job', 'collision'), toDateTime64('2026-01-14 00:00:00', 9), 50)`); err != nil {
		t.Fatal(err)
	}

	oracle := runDeltaPrefixQuery(t, db, deltaPrefixCanonicalizedWindow(deltaPrefixTestStart, deltaPrefixTestRange, ""), 0, false)
	got := runDeltaPrefixQuery(t, db, deltaPrefixCanonicalizedWindow(deltaPrefixTestStart, deltaPrefixTestRange, "otel_metrics_sum_delta_prefix"), 0, true)

	// Both maps must report EXACTLY ONE series for "collision" — two would
	// mean the aggregate term's GROUP BY failed to canonicalise and the
	// Map-key-order variance split it back into two read-time series.
	if n := len(oracle); n != 1 {
		t.Fatalf("oracle: got %d series, want 1 (keys: %v)", n, mapKeys(oracle))
	}
	if n := len(got); n != 1 {
		t.Fatalf("new mechanism: got %d series, want 1 (keys: %v) — Map key-order variance was NOT collapsed", n, mapKeys(got))
	}
	oracleVal := oracle["collision"]
	gotVal, ok := got["collision"]
	if !ok {
		t.Fatal("new mechanism: no row for job=collision")
	}
	if math.Abs(oracleVal-gotVal) > 1e-9 {
		t.Errorf("new mechanism = %v, oracle (unbounded raw scan, also collapsed via canonicalisation) = %v — "+
			"must match exactly; a mismatch means the aggregate term silently dropped one of the two "+
			"Map-key-order variants instead of summing both", gotVal, oracleVal)
	}
}

// TestDeltaPrefixAggregateSource_GapBeforeRawRemainder pins that a large
// temporal gap between midnight and the raw-remainder term's own earliest
// in-range sample doesn't corrupt the LEFT JOIN arithmetic: the series has
// correctly-backfilled aggregate history, but otel_metrics_sum carries NO
// samples between toStartOfDay(rangeStart) and the window itself — the
// term2 sub-select's sum(Value) over zero matching rows must resolve to
// ClickHouse's ordinary numeric LEFT JOIN default (0), not NULL or an
// error, and the aggregate term alone must carry the whole answer.
func TestDeltaPrefixAggregateSource_GapBeforeRawRemainder(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	deltaPrefixAggregateSeedDDL(t, db)

	insertDeltaPrefixWindowSamples(t, db, `map('job', 'gappy')`, "gappy")
	// Old history, correctly backfilled, several days before today — but
	// NOTHING inserted for "gappy" between 2026-01-15 00:00:00 and the
	// window samples above, so term2's own scan window
	// [toStartOfDay(rangeStart), rangeStart] is empty except the two window
	// rows themselves.
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, map('job', 'gappy'), toDateTime64('2026-01-05 00:00:00', 9), 42)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum_delta_prefix VALUES
		(map('job', 'gappy'), toDateTime64('2026-01-05 00:00:00', 9), 42)`); err != nil {
		t.Fatal(err)
	}

	oracle := runDeltaPrefixQuery(t, db, deltaPrefixCanonicalizedWindow(deltaPrefixTestStart, deltaPrefixTestRange, ""), 0, false)
	got := runDeltaPrefixQuery(t, db, deltaPrefixCanonicalizedWindow(deltaPrefixTestStart, deltaPrefixTestRange, "otel_metrics_sum_delta_prefix"), 0, true)

	oracleVal, ok := oracle["gappy"]
	if !ok {
		t.Fatal("oracle: no row for job=gappy")
	}
	gotVal, ok := got["gappy"]
	if !ok {
		t.Fatal("new mechanism: no row for job=gappy — the multi-day gap before the raw remainder's own samples must not drop the series")
	}
	if math.IsNaN(gotVal) || math.IsInf(gotVal, 0) {
		t.Fatalf("new mechanism produced a non-finite value across the gap: %v", gotVal)
	}
	if math.Abs(oracleVal-gotVal) > 1e-9 {
		t.Errorf("new mechanism = %v, oracle = %v — the raw-remainder gap must not change the reconstructed value", gotVal, oracleVal)
	}
}

// TestDeltaPrefixAggregateSource_NeverBackfilledSeries confirms the
// documented graceful-degrade behaviour (chsql's deltaPrefixAggregateSource
// doc, config.Config.DeltaPrefixReadEnabled's doc) for a series that has
// real, long-lived history in otel_metrics_sum but was NEVER copied into
// otel_metrics_sum_delta_prefix: the aggregate term contributes 0 via
// ClickHouse's ordinary LEFT JOIN default (no COALESCE, no crash), so the
// reconstruction degrades to "raw remainder only" — under-counting relative
// to the oracle's true value (which the OLD lookback-bounded approximation
// would also miss, for the same underlying reason: the old accumulation
// predates whatever bound is in effect), but producing a FINITE, sane
// value, never an error or NaN/Inf.
func TestDeltaPrefixAggregateSource_NeverBackfilledSeries(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	deltaPrefixAggregateSeedDDL(t, db)

	insertDeltaPrefixWindowSamples(t, db, `map('job', 'neverbackfilled')`, "neverbackfilled")
	// Real old history in the base table — but otel_metrics_sum_delta_prefix
	// stays EMPTY for this job: simulates a deployment that turned on
	// CERBERUS_DELTA_PREFIX_READ_ENABLED before completing (or without ever
	// running) the backfill CLI for this series.
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, map('job', 'neverbackfilled'), toDateTime64('2026-01-10 00:00:00', 9), 999)`); err != nil {
		t.Fatal(err)
	}

	oracle := runDeltaPrefixQuery(t, db, deltaPrefixCanonicalizedWindow(deltaPrefixTestStart, deltaPrefixTestRange, ""), 0, false)
	got := runDeltaPrefixQuery(t, db, deltaPrefixCanonicalizedWindow(deltaPrefixTestStart, deltaPrefixTestRange, "otel_metrics_sum_delta_prefix"), 0, true)

	oracleVal, ok := oracle["neverbackfilled"]
	if !ok {
		t.Fatal("oracle: no row for job=neverbackfilled")
	}
	gotVal, ok := got["neverbackfilled"]
	if !ok {
		t.Fatal("new mechanism: no row for job=neverbackfilled — a never-backfilled series must still answer, not disappear")
	}
	if math.IsNaN(gotVal) || math.IsInf(gotVal, 0) {
		t.Fatalf("new mechanism produced a non-finite value for a never-backfilled series: %v", gotVal)
	}
	if math.Abs(oracleVal-gotVal) < 1e-9 {
		t.Errorf("new mechanism (%v) unexpectedly matched the oracle (%v) for a NEVER-backfilled series — "+
			"this test's own fixture is supposed to reproduce the documented under-count hazard "+
			"(999 units of real history missing from the aggregate term); if this now passes because "+
			"the mechanism changed, update the assertion deliberately, don't just widen the tolerance", gotVal, oracleVal)
	}
}

// TestDeltaPrefixAggregateSource_JoinUseNulls1DoesNotPropagateNull re-runs
// the NeverBackfilledSeries (aggregate-term LEFT JOIN miss) and
// GapBeforeRawRemainder (raw-remainder-term LEFT JOIN miss) scenarios above
// under `SETTINGS join_use_nulls = 1` — a legitimate ClickHouse deployment
// setting cerberus does not itself pin. Under that setting a LEFT JOIN miss
// returns NULL, not the query's Float64 default; NULL + x is NULL in
// ClickHouse, so without the ifNull guard on both summed terms (see
// deltaPrefixAggregateSource's doc) the row's Value comes back NULL — which
// this test's plain float64 Scan turns into a hard error rather than a
// silently wrong number, making a regression here fail loud. Cross-checked
// against the default-settings (join_use_nulls=0) run to confirm the
// deployment setting changes nothing about the answer, only how a miss is
// represented before the ifNull guard resolves it.
func TestDeltaPrefixAggregateSource_JoinUseNulls1DoesNotPropagateNull(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	deltaPrefixAggregateSeedDDL(t, db)

	// Aggregate-term miss: real base history, but otel_metrics_sum_delta_prefix
	// stays empty for this job (same fixture as NeverBackfilledSeries).
	insertDeltaPrefixWindowSamples(t, db, `map('job', 'nulljoin_agg_miss')`, "nulljoin_agg_miss")
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, map('job', 'nulljoin_agg_miss'), toDateTime64('2026-01-10 00:00:00', 9), 999)`); err != nil {
		t.Fatal(err)
	}

	// Raw-remainder-term miss: correctly-backfilled aggregate history, but
	// nothing in otel_metrics_sum between toStartOfDay(rangeStart) and the
	// window itself (same fixture as GapBeforeRawRemainder).
	insertDeltaPrefixWindowSamples(t, db, `map('job', 'nulljoin_raw_miss')`, "nulljoin_raw_miss")
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(1, map('job', 'nulljoin_raw_miss'), toDateTime64('2026-01-05 00:00:00', 9), 42)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum_delta_prefix VALUES
		(map('job', 'nulljoin_raw_miss'), toDateTime64('2026-01-05 00:00:00', 9), 42)`); err != nil {
		t.Fatal(err)
	}

	window := deltaPrefixCanonicalizedWindow(deltaPrefixTestStart, deltaPrefixTestRange, "otel_metrics_sum_delta_prefix")
	underJoinUseNulls := runDeltaPrefixQuery(t, db, window, 0, true, "join_use_nulls = 1")
	defaultSettings := runDeltaPrefixQuery(t, db, window, 0, true)

	for _, job := range []string{"nulljoin_agg_miss", "nulljoin_raw_miss"} {
		got, ok := underJoinUseNulls[job]
		if !ok {
			t.Fatalf("%s: missing under join_use_nulls=1 — NULL likely propagated through the summed join and dropped the row", job)
		}
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("%s: non-finite value under join_use_nulls=1: %v", job, got)
		}
		want, ok := defaultSettings[job]
		if !ok {
			t.Fatalf("%s: missing under the default (join_use_nulls=0) settings — fixture bug", job)
		}
		if math.Abs(want-got) > 1e-9 {
			t.Errorf("%s: join_use_nulls=1 changed the answer: got %v, join_use_nulls=0 got %v — the ifNull guard must make both deployments answer identically", job, got, want)
		}
	}
}

// deltaPrefixCumulativeOnlyAggregateRows is the row count seeded into
// otel_metrics_sum_delta_prefix by
// TestDeltaPrefixAggregateSource_PresenceGuardPrunesCumulativeOnlyScan —
// large enough that a full (unguarded) scan and a guard-pruned scan report
// visibly different EXPLAIN ESTIMATE read_rows.
const deltaPrefixCumulativeOnlyAggregateRows = 20_000

// deltaPrefixCumulativeOnlyAggregateSeriesCount is the distinct-series
// fan-out for that same seed, keeping seriesRows^-1 * total rows per bucket
// realistic rather than one enormous single-series history.
const deltaPrefixCumulativeOnlyAggregateSeriesCount = 500

// explainEstimateRowsForTable returns the read_rows EXPLAIN ESTIMATE reports
// for table specifically (EXPLAIN ESTIMATE emits one row per (database,
// table) the plan touches), or -1 if table never appears — distinguishing
// "genuinely pruned to zero" (present, 0) from "not in the plan at all"
// would need a third state this test doesn't need: both mean "not read".
func explainEstimateRowsForTable(t *testing.T, db *sql.DB, query, table string) int64 {
	t.Helper()
	rows, err := db.Query("EXPLAIN ESTIMATE " + query)
	if err != nil {
		t.Fatalf("EXPLAIN ESTIMATE: %v\nquery: %s", err, query)
	}
	defer func() { _ = rows.Close() }()
	var total int64
	found := false
	for rows.Next() {
		var database, tbl string
		var parts, r, marks int64
		if err := rows.Scan(&database, &tbl, &parts, &r, &marks); err != nil {
			t.Fatalf("scan estimate: %v", err)
		}
		if tbl == table {
			total += r
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !found {
		return -1
	}
	return total
}

// stripPresenceGuard removes every ` AND (SELECT max(...)...)` presence-guard
// clause deltaPresenceGuardFrag renders from sqlText via balanced-paren
// scanning, reconstructing the exact SQL shape the pre-fix
// deltaPrefixAggregateSource emitted (the guard entirely absent) from the
// REAL, current emitter's own output — a precise, code-derived "before"
// baseline rather than a hand-written approximation of it.
func stripPresenceGuard(t *testing.T, sqlText string) string {
	t.Helper()
	const marker = " AND (SELECT max("
	for {
		idx := strings.Index(sqlText, marker)
		if idx == -1 {
			return sqlText
		}
		openIdx := idx + len(" AND ")
		depth := 0
		end := -1
		for i := openIdx; i < len(sqlText); i++ {
			switch sqlText[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					end = i
				}
			}
			if end != -1 {
				break
			}
		}
		if end == -1 {
			t.Fatalf("stripPresenceGuard: unbalanced parens in:\n%s", sqlText)
		}
		sqlText = sqlText[:idx] + sqlText[end+1:]
	}
}

// TestDeltaPrefixAggregateSource_PresenceGuardPrunesCumulativeOnlyScan is the
// empirical (EXPLAIN ESTIMATE) counterpart to
// TestDeltaPrefixAggregateSource_PresenceGuardWiredIntoBothTerms's structural
// check: for a CUMULATIVE-only series — the ~99.99% common case
// deltaPresenceGuardFrag's own doc measures — the aggregate-table term's
// read_rows with the guard present must be near zero (ClickHouse constant-
// folds the guard's scalar subquery to `false` and prunes the whole table
// read), while the SAME emitted query with ONLY the guard clause removed
// (stripPresenceGuard, reconstructing the pre-fix shape) reads the full
// seeded corpus. This is the exact class of evidence (EXPLAIN ESTIMATE
// against a real chDB session) an adversarial review of PR #2514 used to
// find the guard missing from deltaPrefixAggregateSource in the first place.
func TestDeltaPrefixAggregateSource_PresenceGuardPrunesCumulativeOnlyScan(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	deltaPrefixAggregateSeedDDL(t, db)

	// The in-window samples are CUMULATIVE (AggregationTemporality=2), not
	// DELTA — the guard's own scalar subquery reads exactly these two rows
	// and must fold to `false`.
	if _, err := db.Exec(`INSERT INTO otel_metrics_sum VALUES
		(2, map('job', 'cumonly'), toDateTime64('2026-01-15 11:59:30', 9), 1),
		(2, map('job', 'cumonly'), toDateTime64('2026-01-15 12:00:00', 9), 2)`); err != nil {
		t.Fatal(err)
	}

	// A realistically large aggregate-table corpus, entirely BEFORE the
	// query window's own day (BucketStart < toStartOfDay(rangeStart) is
	// deltaPrefixAggregateSource's own aggregate-term bound), fanned across
	// many distinct series so this isn't one series' single-partition
	// history.
	seedSQL := fmt.Sprintf(`INSERT INTO otel_metrics_sum_delta_prefix
SELECT
    map('job', concat('s', toString(number %% %d))) AS Attributes,
    toDateTime64('2026-01-15 00:00:00', 9) - toIntervalDay(number %% 30 + 1) AS BucketStart,
    toFloat64(number) AS PartialSum
FROM numbers(%d)`, deltaPrefixCumulativeOnlyAggregateSeriesCount, deltaPrefixCumulativeOnlyAggregateRows)
	if _, err := db.Exec(seedSQL); err != nil {
		t.Fatalf("seed aggregate corpus: %v", err)
	}

	r := deltaPrefixCanonicalizedWindow(deltaPrefixTestStart, deltaPrefixTestRange, "otel_metrics_sum_delta_prefix")
	ctx := WithDeltaPrefixReadEnabled(context.Background(), true)
	guardedSQL, _, err := Emit(ctx, r)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	guardedSQL = strings.TrimSuffix(strings.TrimSpace(guardedSQL), ";")
	unguardedSQL := stripPresenceGuard(t, guardedSQL)
	if unguardedSQL == guardedSQL {
		t.Fatal("stripPresenceGuard left the SQL unchanged — the guard clause was not found; fixture/marker drifted from the real emit shape")
	}

	guardedRows := explainEstimateRowsForTable(t, db, guardedSQL, "otel_metrics_sum_delta_prefix")
	unguardedRows := explainEstimateRowsForTable(t, db, unguardedSQL, "otel_metrics_sum_delta_prefix")
	t.Logf("otel_metrics_sum_delta_prefix read_rows: guarded=%d unguarded=%d (seeded %d)",
		guardedRows, unguardedRows, deltaPrefixCumulativeOnlyAggregateRows)

	if unguardedRows < deltaPrefixCumulativeOnlyAggregateRows {
		t.Fatalf("unguarded (pre-fix shape) read_rows=%d, want >= the seeded %d rows — corpus/EXPLAIN setup did not reproduce a full scan to compare against",
			unguardedRows, deltaPrefixCumulativeOnlyAggregateRows)
	}
	// A guarded read of exactly 0 is the ideal (ClickHouse's constant-false
	// short-circuit skips the table read outright); this asserts the
	// weaker, robust property — pruned to a small fraction of the
	// unguarded scan — so the test isn't coupled to that specific
	// optimizer behaviour.
	const maxGuardedFraction = 10 // guarded must read < 1/10th of unguarded
	if guardedRows*maxGuardedFraction >= unguardedRows {
		t.Errorf("guarded read_rows=%d not far below unguarded read_rows=%d (want guarded*%d < unguarded) — the presence guard is not pruning the CUMULATIVE-only scan",
			guardedRows, unguardedRows, maxGuardedFraction)
	}
}

// mapKeys returns m's keys, for a readable failure message.
func mapKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

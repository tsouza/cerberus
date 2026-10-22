package deltaprefix

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/schema"
)

// testColumns builds a Columns value the way a deployment that has opted
// into CERBERUS_SCHEMA_DELTA_PREFIX_ENABLED would resolve one — schema.Metrics
// .DeltaPrefixTable and friends are empty on schema.DefaultOTelMetrics()
// until that flag is set (see internal/schema/env.go's
// DefaultOTelMetricsFrom), so this fills them in directly rather than
// coupling this package's SQL-rendering tests to that env-gating mechanism.
func testColumns() Columns {
	m := schema.DefaultOTelMetrics()
	m.DeltaPrefixTable = "otel_metrics_sum_delta_prefix"
	m.DeltaPrefixBucketColumn = "BucketStart"
	m.DeltaPrefixSumColumn = "PartialSum"
	return FromSchema("otel", m)
}

var testBefore = time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)

// testCutover is the cutover day the scripted Conns below report for
// testBefore over an unzoned TimeUnix on a UTC server: its UTC midnight.
var testCutover = time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

// TestBackfillSQL pins the rendered INSERT ... SELECT shape: the target
// column list, the DELTA-only + exact-instant-before WHERE (NOT rounded to
// a day boundary — see the package doc comment for why), and the
// toStartOfDay bucket GROUP BY matching internal/schema/ddl's MV exactly.
// The cutoff must be a bound positional arg (Lit), never inlined — it is
// operator-supplied data, not part of the statement shape.
func TestBackfillSQL(t *testing.T) {
	sql, args := BackfillSQL(testColumns(), testBefore)
	want := "INSERT INTO otel.otel_metrics_sum_delta_prefix " +
		"(`MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, `BucketStart`, `PartialSum`) " +
		"SELECT `MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, " +
		"toStartOfDay(`TimeUnix`) AS `BucketStart`, sum(`Value`) AS `PartialSum` " +
		"FROM `otel`.`otel_metrics_sum` WHERE `AggregationTemporality` = 1 AND `TimeUnix` < ? " +
		"GROUP BY `MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, toStartOfDay(`TimeUnix`)"
	if sql != want {
		t.Errorf("BackfillSQL sql =\n%s\nwant\n%s", sql, want)
	}
	if len(args) != 1 || args[0] != testBefore {
		t.Errorf("BackfillSQL args = %v; want [%v]", args, testBefore)
	}
}

// TestAggregateAndBaseTotalsSQL pins the two verify reads: both group by
// MetricName only, both bound strictly below the START OF the cutover day
// (NOT the exact --before instant BackfillSQL uses), and the base read
// additionally filters to DELTA temporality.
//
// The SAME bound on both sides is the whole point. BucketStart is
// calendar-day granularity, so the cutover day's aggregate bucket holds
// both the backfilled morning and every row the live MV has captured since,
// while the base table can only be filtered by the instant — bounding the
// aggregate side at the instant therefore compares a whole day against a
// morning, and FAILS on any deployment still taking DELTA traffic no matter
// how complete the backfill was. See aggregateTotalsSQL's own comment for
// why excluding that one day costs no coverage.
func TestAggregateAndBaseTotalsSQL(t *testing.T) {
	c := testColumns()
	// The cutover is the same instant literal on both sides.
	day := "parseDateTime64BestEffort('2026-08-20 00:00:00.000000000Z', 9)"

	aggSQL, aggArgs := aggregateTotalsSQL(c, testCutover, time.Time{}, false)
	wantAgg := "SELECT `MetricName`, sum(`PartialSum`) FROM `otel`.`otel_metrics_sum_delta_prefix` " +
		"WHERE `BucketStart` < " + day + " GROUP BY `MetricName`"
	if aggSQL != wantAgg || len(aggArgs) != 0 {
		t.Errorf("aggregateTotalsSQL =\n%s %v\nwant\n%s []", aggSQL, aggArgs, wantAgg)
	}

	baseSQL, baseArgs := baseTotalsSQL(c, testCutover, time.Time{}, false)
	wantBase := "SELECT `MetricName`, sum(`Value`) FROM `otel`.`otel_metrics_sum` " +
		"WHERE `AggregationTemporality` = 1 AND `TimeUnix` < " + day + " GROUP BY `MetricName`"
	if baseSQL != wantBase || len(baseArgs) != 0 {
		t.Errorf("baseTotalsSQL =\n%s %v\nwant\n%s []", baseSQL, baseArgs, wantBase)
	}
}

// TestAggregateAndBaseTotalsSQL_RetentionActive pins the additional lower
// bound both totals reads gain when a retention boundary is active
// (cerberus issue #2652): aggregateTotalsSQL bounds
// DeltaPrefixBucketColumn >= boundary directly (it is already
// day-granularity storage), baseTotalsSQL bounds
// toStartOfDay(TimestampColumn) >= boundary (day-truncated), so the two
// exclude exactly the same set of days — as their upper bounds also do.
func TestAggregateAndBaseTotalsSQL_RetentionActive(t *testing.T) {
	c := testColumns()
	boundary := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	day := "parseDateTime64BestEffort('2026-08-20 00:00:00.000000000Z', 9)"

	aggSQL, aggArgs := aggregateTotalsSQL(c, testCutover, boundary, true)
	wantAgg := "SELECT `MetricName`, sum(`PartialSum`) FROM `otel`.`otel_metrics_sum_delta_prefix` " +
		"WHERE `BucketStart` < " + day + " AND `BucketStart` >= ? GROUP BY `MetricName`"
	if aggSQL != wantAgg {
		t.Errorf("aggregateTotalsSQL sql =\n%s\nwant\n%s", aggSQL, wantAgg)
	}
	if len(aggArgs) != 1 || aggArgs[0] != boundary {
		t.Errorf("aggregateTotalsSQL args = %v; want [%v]", aggArgs, boundary)
	}

	baseSQL, baseArgs := baseTotalsSQL(c, testCutover, boundary, true)
	wantBase := "SELECT `MetricName`, sum(`Value`) FROM `otel`.`otel_metrics_sum` " +
		"WHERE `AggregationTemporality` = 1 AND `TimeUnix` < " + day + " AND toStartOfDay(`TimeUnix`) >= ? GROUP BY `MetricName`"
	if baseSQL != wantBase {
		t.Errorf("baseTotalsSQL sql =\n%s\nwant\n%s", baseSQL, wantBase)
	}
	if len(baseArgs) != 1 || baseArgs[0] != boundary {
		t.Errorf("baseTotalsSQL args = %v; want [%v]", baseArgs, boundary)
	}
}

// TestOutsideRetentionDaysSQL pins the DISTINCT-day read used both by
// Backfill (pre-INSERT check against the base table) and Verify (to
// populate Report.OutsideRetentionDays): DELTA-only, bounded by the same
// exact-instant `before` BackfillSQL uses, plus the day-bucket `<
// boundary` cut that defines "already outside retention" (cerberus issue
// #2652), grouped (i.e. deduplicated) and ordered by day ascending.
func TestOutsideRetentionDaysSQL(t *testing.T) {
	c := testColumns()
	boundary := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	sql, args := outsideRetentionDaysSQL(c, testBefore, boundary)
	want := "SELECT toStartOfDay(`TimeUnix`) FROM `otel`.`otel_metrics_sum` " +
		"WHERE `AggregationTemporality` = 1 AND `TimeUnix` < ? AND toStartOfDay(`TimeUnix`) < ? " +
		"GROUP BY toStartOfDay(`TimeUnix`) ORDER BY toStartOfDay(`TimeUnix`)"
	if sql != want {
		t.Errorf("outsideRetentionDaysSQL sql =\n%s\nwant\n%s", sql, want)
	}
	if len(args) != 2 || args[0] != testBefore || args[1] != boundary {
		t.Errorf("outsideRetentionDaysSQL args = %v; want [%v %v]", args, testBefore, boundary)
	}
}

// TestRetentionBoundary pins retentionBoundary's pure day-math: a
// non-positive retention (no TTL clause emitted at all — see
// internal/schema/ddl's ttlExpr) always reports inactive with a zero
// boundary regardless of now; a positive retention reports active with
// boundary = now - retention, exactly.
func TestRetentionBoundary(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	for _, retention := range []time.Duration{0, -time.Hour} {
		boundary, active := retentionBoundary(now, retention)
		if active {
			t.Errorf("retentionBoundary(%v, %v) active = true; want false (no TTL)", now, retention)
		}
		if !boundary.IsZero() {
			t.Errorf("retentionBoundary(%v, %v) boundary = %v; want zero time", now, retention, boundary)
		}
	}

	retention := 90 * 24 * time.Hour
	boundary, active := retentionBoundary(now, retention)
	if !active {
		t.Fatalf("retentionBoundary(%v, %v) active = false; want true", now, retention)
	}
	wantBoundary := now.Add(-retention)
	if !boundary.Equal(wantBoundary) {
		t.Errorf("retentionBoundary(%v, %v) boundary = %v; want %v", now, retention, boundary, wantBoundary)
	}
}

// TestBackfillMidDayCutover_NoGap is a regression test for the mid-day
// cutover data-loss bug: an earlier revision rounded BackfillSQL's --before
// bound down to toStartOfDay before comparing it against row timestamps.
// Since a real deployment's MV is almost always created mid-day (not at
// exactly midnight), that day-truncated bound excluded every row between
// midnight and the MV's real creation instant from BOTH sides — the
// backfill (whose bound had already been rounded down past those rows) and
// the live MV (which never fires for INSERTs older than its own creation
// instant) — a permanent, silent under-count with no query error.
//
// This confirms BackfillSQL's rendered cutoff arg is the exact `before`
// instant, never day-truncated, so a row lands on exactly one side of the
// backfill/MV split: captured by the backfill (TimeUnix < cutoff) iff NOT
// captured by the live MV (which only fires for TimeUnix >= its own
// creation instant) — across a full day straddling a mid-day cutover,
// including the previously-lost midnight-to-creation-instant window.
func TestBackfillMidDayCutover_NoGap(t *testing.T) {
	mvCreatedAt := time.Date(2026, 8, 20, 14, 32, 10, 0, time.UTC) // mid-day, not midnight
	_, args := BackfillSQL(testColumns(), mvCreatedAt)
	if len(args) != 1 {
		t.Fatalf("BackfillSQL args = %v; want exactly 1", args)
	}
	cutoff, ok := args[0].(time.Time)
	if !ok || !cutoff.Equal(mvCreatedAt) {
		t.Fatalf("BackfillSQL cutoff = %v; want the exact MV creation instant %v "+
			"(not rounded down to a day boundary)", args[0], mvCreatedAt)
	}

	dayStart := mvCreatedAt.Truncate(24 * time.Hour) // midnight of the cutover day
	samples := []time.Time{
		dayStart,                      // midnight: start of the previously-lost window
		dayStart.Add(1 * time.Hour),   // squarely inside the previously-lost window
		mvCreatedAt.Add(-time.Second), // just before the MV's creation instant
		mvCreatedAt,                   // exactly the MV's creation instant
		mvCreatedAt.Add(time.Second),  // just after
		dayStart.Add(23 * time.Hour),  // late in the cutover day
	}
	for _, ts := range samples {
		backfillCaptures := ts.Before(cutoff)
		mvCaptures := !ts.Before(mvCreatedAt) // the live MV fires only for INSERTs from its own creation instant onward
		if backfillCaptures == mvCaptures {
			t.Errorf("row at %v: backfillCaptures=%v mvCaptures=%v — want exactly one true (no gap, no double-count)",
				ts, backfillCaptures, mvCaptures)
		}
	}
}

// TestDiff pins the comparison logic: an exact match, a mismatch beyond
// tolerance, a mismatch exactly AT the tolerance boundary (not reported —
// ">" not ">="), and a metric present on only one side (reads as a full
// mismatch against an implicit zero on the other side).
func TestDiff(t *testing.T) {
	agg := map[string]float64{
		"exact_match":    100,
		"over_tolerance": 100,
		"at_boundary":    100.5,
		"only_aggregate": 50,
	}
	base := map[string]float64{
		"exact_match":    100,
		"over_tolerance": 105,
		"at_boundary":    100,
		"only_base":      25,
	}
	rep := Diff(agg, base, testBefore, 0.5)

	if rep.Pass() {
		t.Fatal("expected mismatches, got Pass()=true")
	}
	if rep.Before != testBefore || rep.Tolerance != 0.5 {
		t.Errorf("Report metadata not carried through: %+v", rep)
	}

	byName := map[string]Mismatch{}
	for _, m := range rep.Mismatches {
		byName[m.MetricName] = m
	}
	if _, ok := byName["exact_match"]; ok {
		t.Error("exact_match must not be reported as a mismatch")
	}
	if _, ok := byName["at_boundary"]; ok {
		t.Error("a diff exactly AT tolerance (0.5) must not be reported (strictly greater-than only)")
	}
	if m, ok := byName["over_tolerance"]; !ok {
		t.Error("over_tolerance must be reported")
	} else if m.Diff() != -5 {
		t.Errorf("over_tolerance Diff() = %v; want -5", m.Diff())
	}
	if m, ok := byName["only_aggregate"]; !ok {
		t.Error("only_aggregate must be reported (base implicitly 0)")
	} else if m.Base != 0 || m.Aggregate != 50 {
		t.Errorf("only_aggregate mismatch = %+v; want Aggregate=50 Base=0", m)
	}
	if m, ok := byName["only_base"]; !ok {
		t.Error("only_base must be reported (aggregate implicitly 0)")
	} else if m.Aggregate != 0 || m.Base != 25 {
		t.Errorf("only_base mismatch = %+v; want Aggregate=0 Base=25", m)
	}
	if len(rep.Mismatches) != 3 {
		t.Errorf("len(Mismatches) = %d; want 3, got %+v", len(rep.Mismatches), rep.Mismatches)
	}
}

// TestDiff_Pass confirms an all-matching pair of maps reports Pass()=true
// with zero mismatches.
func TestDiff_Pass(t *testing.T) {
	agg := map[string]float64{"m": 10}
	base := map[string]float64{"m": 10}
	rep := Diff(agg, base, testBefore, 0.001)
	if !rep.Pass() {
		t.Errorf("expected Pass()=true, got mismatches: %+v", rep.Mismatches)
	}
}

// fakeRows is a canned driver.Rows over fixed (name, total) tuples, mirroring
// internal/routerrules' source_ch_test.go fakeRows: driver.Rows is embedded
// so the wide interface's unused methods are satisfied for free, and only
// the four methods this package's scanTotals calls are overridden.
type fakeRows struct {
	driver.Rows
	data [][2]any
	pos  int
}

func (r *fakeRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	if len(dest) != 2 {
		return fmt.Errorf("fakeRows: scan arity %d != 2", len(dest))
	}
	np, ok := dest[0].(*string)
	if !ok {
		return fmt.Errorf("fakeRows: dest[0] is %T, want *string", dest[0])
	}
	*np = row[0].(string)
	tp, ok := dest[1].(*float64)
	if !ok {
		return fmt.Errorf("fakeRows: dest[1] is %T, want *float64", dest[1])
	}
	*tp = row[1].(float64)
	return nil
}

func (r *fakeRows) Err() error   { return nil }
func (r *fakeRows) Close() error { return nil }

// fakeConn is a scripted Conn: Query returns rows keyed by a substring match
// against the SQL, Exec records every statement + args it received. Mirrors
// internal/routerrules' recordingConn.
type fakeConn struct {
	execSQL  []string
	execArgs [][]any
	execErr  error

	queries []string
	respond map[string][][2]any
	rowsErr error
}

func (c *fakeConn) Exec(_ context.Context, query string, args ...any) error {
	c.execSQL = append(c.execSQL, query)
	c.execArgs = append(c.execArgs, args)
	return c.execErr
}

func (c *fakeConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	c.queries = append(c.queries, query)
	if rows, ok := cutoverRows(query); ok {
		return rows, nil
	}
	if c.rowsErr != nil {
		return nil, c.rowsErr
	}
	for match, rows := range c.respond {
		if contains(query, match) {
			return &fakeRows{data: rows}, nil
		}
	}
	return &fakeRows{}, nil
}

// cutoverRows answers the two reads queryCutoverDay issues the way a UTC
// server answers them for an unzoned DateTime64(9) TimeUnix: the column
// type, then testCutover as Unix seconds in UTC. ok is false for any other
// query.
func cutoverRows(query string) (rows driver.Rows, ok bool) {
	switch {
	case contains(query, "`system`.`columns`"):
		return &valueRows{data: [][]any{{"DateTime64(9)"}}}, true
	case contains(query, "timezoneOf("):
		return &valueRows{data: [][]any{{testCutover.Unix(), "UTC", int64(0)}}}, true
	}
	return nil, false
}

// valueRows is a canned driver.Rows over rows of any arity, scanning each
// value into a destination of its own type.
type valueRows struct {
	driver.Rows
	data [][]any
	pos  int
}

func (r *valueRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *valueRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	if len(dest) != len(row) {
		return fmt.Errorf("valueRows: scan arity %d != %d", len(dest), len(row))
	}
	for i, v := range row {
		switch d := dest[i].(type) {
		case *string:
			*d = v.(string)
		case *int64:
			*d = v.(int64)
		default:
			return fmt.Errorf("valueRows: dest[%d] is %T", i, dest[i])
		}
	}
	return nil
}

func (r *valueRows) Err() error   { return nil }
func (r *valueRows) Close() error { return nil }

func contains(s, substr string) bool {
	return len(substr) == 0 || (len(s) >= len(substr) && indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// erroringRows is a driver.Rows that fails on either Scan or Err (never
// both) instead of yielding real data, for exercising scanTotals' two
// per-row failure branches directly — fakeRows never fails either, so
// neither is otherwise reachable.
type erroringRows struct {
	driver.Rows
	hasRow   bool
	scanErr  error
	finalErr error
	yielded  bool
}

func (r *erroringRows) Next() bool {
	if !r.hasRow || r.yielded {
		return false
	}
	r.yielded = true
	return true
}

func (r *erroringRows) Scan(...any) error { return r.scanErr }
func (r *erroringRows) Err() error        { return r.finalErr }
func (r *erroringRows) Close() error      { return nil }

// singleRowsConn is a Conn whose Query always returns one fixed driver.Rows,
// for pairing with erroringRows.
type singleRowsConn struct{ rows driver.Rows }

func (c *singleRowsConn) Exec(context.Context, string, ...any) error { return nil }

func (c *singleRowsConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return c.rows, nil
}

// twoCallConn answers the cutover-day reads, then succeeds on its first
// totals Query and fails on its second — the shape Verify's own two totals
// reads (aggregate table, then base table) need to reach its second
// error-wrap branch. TestVerify_PropagatesQueryError already covers the
// first (fakeConn.rowsErr fails immediately).
type twoCallConn struct {
	calls int
	err   error
}

func (c *twoCallConn) Exec(context.Context, string, ...any) error { return nil }

func (c *twoCallConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	if rows, ok := cutoverRows(query); ok {
		return rows, nil
	}
	c.calls++
	if c.calls == 2 {
		return nil, c.err
	}
	return &fakeRows{}, nil
}

// TestBackfill_ExecutesRenderedSQL confirms Backfill hands conn EXACTLY the
// statement + args BackfillSQL renders, and wraps a real Exec failure with
// enough context to identify which table failed. retention=0 (no TTL)
// keeps the new outside-retention pre-check inactive, so it issues no
// extra query — TestBackfill_DetectsOutsideRetentionDays below covers the
// active path.
func TestBackfill_ExecutesRenderedSQL(t *testing.T) {
	c := testColumns()
	conn := &fakeConn{}
	result, err := Backfill(context.Background(), conn, c, testBefore, 0)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if len(result.OutsideRetentionDays) != 0 {
		t.Errorf("OutsideRetentionDays = %v; want none with retention=0", result.OutsideRetentionDays)
	}
	wantSQL, wantArgs := BackfillSQL(c, testBefore)
	if len(conn.execSQL) != 1 || conn.execSQL[0] != wantSQL {
		t.Errorf("Exec sql = %v; want [%q]", conn.execSQL, wantSQL)
	}
	if len(conn.execArgs) != 1 || len(conn.execArgs[0]) != len(wantArgs) {
		t.Errorf("Exec args = %v; want %v", conn.execArgs, wantArgs)
	}

	failing := &fakeConn{execErr: errors.New("boom")}
	_, err = Backfill(context.Background(), failing, c, testBefore, 0)
	if err == nil {
		t.Fatal("expected error from a failing Exec")
	}
	if got := err.Error(); !contains(got, c.DeltaPrefixTable) {
		t.Errorf("Backfill error %q does not name the target table %q", got, c.DeltaPrefixTable)
	}
}

// withFrozenNow pins nowFunc to now for the duration of the test, restoring
// the real clock via t.Cleanup — the mechanism deltaprefix_test.go uses to
// exercise the retention-boundary logic (cerberus issue #2652)
// deterministically, per the package's "unit testable without a live
// ClickHouse connection" doc-comment promise.
func withFrozenNow(t *testing.T, now time.Time) {
	t.Helper()
	prev := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prev })
}

// TestBackfill_DetectsOutsideRetentionDays confirms Backfill queries the
// base table for outside-retention days BEFORE issuing its own INSERT
// (order matters: the base table is not subject to this TTL and so cannot
// race with it), still executes the INSERT regardless (a doomed day is a
// warning, not a reason to abort — see BackfillResult's doc comment), and
// surfaces the detected days on the returned BackfillResult.
func TestBackfill_DetectsOutsideRetentionDays(t *testing.T) {
	c := testColumns()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	withFrozenNow(t, now)
	retention := 24 * time.Hour // boundary = 2026-08-25T12:00:00Z
	doomedDay := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

	// outsideRetentionDaysSQL scans a single time.Time column, not
	// (string, float64) like scanTotals — fakeConn/fakeRows above are
	// wired for totals reads, so this test drives queryOutsideRetentionDays
	// through a dedicated day-scanning fake rather than reusing fakeConn.
	dconn := &dayFakeConn{days: []time.Time{doomedDay}}
	result, err := Backfill(context.Background(), dconn, c, testBefore, retention)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if len(dconn.execSQL) != 1 {
		t.Fatalf("expected exactly 1 Exec (the INSERT), got %d: %v", len(dconn.execSQL), dconn.execSQL)
	}
	if len(dconn.queries) != 1 {
		t.Fatalf("expected exactly 1 Query (the outside-retention-days check), got %d: %v", len(dconn.queries), dconn.queries)
	}
	if len(result.OutsideRetentionDays) != 1 || !result.OutsideRetentionDays[0].Equal(doomedDay) {
		t.Errorf("OutsideRetentionDays = %v; want [%v]", result.OutsideRetentionDays, doomedDay)
	}
}

// TestBackfill_NoRetentionCheckWhenRetentionZero confirms Backfill issues
// NO retention-boundary query at all when retention is 0 (no TTL clause is
// ever emitted, so nothing can ever be "outside retention" — see
// retentionBoundary) — a deployment without CERBERUS_SCHEMA_TTL_METRICS
// set should not pay for a check that can never find anything.
func TestBackfill_NoRetentionCheckWhenRetentionZero(t *testing.T) {
	c := testColumns()
	dconn := &dayFakeConn{days: []time.Time{time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)}}
	result, err := Backfill(context.Background(), dconn, c, testBefore, 0)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if len(dconn.queries) != 0 {
		t.Errorf("expected no retention-boundary Query with retention=0, got %d: %v", len(dconn.queries), dconn.queries)
	}
	if len(result.OutsideRetentionDays) != 0 {
		t.Errorf("OutsideRetentionDays = %v; want none with retention=0", result.OutsideRetentionDays)
	}
}

// dayFakeConn is a scripted Conn whose Query always returns the configured
// days as single-column time.Time rows (queryOutsideRetentionDays' scan
// shape) and whose Exec always succeeds, recording every statement.
type dayFakeConn struct {
	days    []time.Time
	queries []string
	execSQL []string
}

func (c *dayFakeConn) Exec(_ context.Context, query string, _ ...any) error {
	c.execSQL = append(c.execSQL, query)
	return nil
}

func (c *dayFakeConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	c.queries = append(c.queries, query)
	return &dayFakeRows{days: c.days}, nil
}

// dayFakeRows is a canned driver.Rows over a single time.Time column,
// mirroring fakeRows' shape but for outsideRetentionDaysSQL's one-column
// result instead of scanTotals' (name, total) pair.
type dayFakeRows struct {
	driver.Rows
	days []time.Time
	pos  int
}

func (r *dayFakeRows) Next() bool {
	if r.pos >= len(r.days) {
		return false
	}
	r.pos++
	return true
}

func (r *dayFakeRows) Scan(dest ...any) error {
	tp, ok := dest[0].(*time.Time)
	if !ok {
		return fmt.Errorf("dayFakeRows: dest[0] is %T, want *time.Time", dest[0])
	}
	*tp = r.days[r.pos-1]
	return nil
}

func (r *dayFakeRows) Err() error   { return nil }
func (r *dayFakeRows) Close() error { return nil }

// TestVerify_ReadsBothTotalsAndDiffs runs Verify end to end against a
// scripted Conn returning canned per-metric totals for each of the two
// reads, confirming the resulting Report reflects both.
func TestVerify_ReadsBothTotalsAndDiffs(t *testing.T) {
	c := testColumns()
	// Match keys are backtick-quoted: c.SumTable ("otel_metrics_sum") is a
	// PREFIX of c.DeltaPrefixTable ("otel_metrics_sum_delta_prefix"), so an
	// unquoted substring match would ambiguously match both queries — the
	// closing backtick disambiguates.
	conn := &fakeConn{
		respond: map[string][][2]any{
			"`" + c.DeltaPrefixTable + "`": {{"http_requests_total", 100.0}},
			"`" + c.SumTable + "`":         {{"http_requests_total", 100.0}, {"missed_metric", 5.0}},
		},
	}
	rep, err := Verify(context.Background(), conn, c, testBefore, 0.01, 0)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(rep.OutsideRetentionDays) != 0 {
		t.Errorf("OutsideRetentionDays = %v; want none with retention=0", rep.OutsideRetentionDays)
	}
	if len(conn.queries) != 4 {
		t.Fatalf("expected 4 queries (column type + cutover day + aggregate + base), got %d: %v", len(conn.queries), conn.queries)
	}
	if rep.Pass() {
		t.Fatal("expected a mismatch (missed_metric absent from the aggregate table)")
	}
	if len(rep.Mismatches) != 1 || rep.Mismatches[0].MetricName != "missed_metric" {
		t.Errorf("Mismatches = %+v; want exactly missed_metric", rep.Mismatches)
	}
}

// TestVerify_PropagatesQueryError confirms a Query failure on either read
// surfaces as an error rather than a silently-empty Report.
func TestVerify_PropagatesQueryError(t *testing.T) {
	c := testColumns()
	conn := &fakeConn{rowsErr: errors.New("connection reset")}
	_, err := Verify(context.Background(), conn, c, testBefore, 0.01, 0)
	if !errors.Is(err, conn.rowsErr) || !strings.Contains(err.Error(), c.DeltaPrefixTable) {
		t.Fatalf("Verify error = %v; want the aggregate read's failure naming %s", err, c.DeltaPrefixTable)
	}
}

// TestVerify_PropagatesBaseTableQueryError confirms the SECOND read (the
// base/SumTable one) wraps its own failure with c.SumTable, not just the
// first (aggregate/DeltaPrefixTable) read TestVerify_PropagatesQueryError
// already covers.
func TestVerify_PropagatesBaseTableQueryError(t *testing.T) {
	c := testColumns()
	conn := &twoCallConn{err: errors.New("connection reset")}
	_, err := Verify(context.Background(), conn, c, testBefore, 0.01, 0)
	if err == nil {
		t.Fatal("expected an error from the base-table read failing")
	}
	if !contains(err.Error(), c.SumTable) {
		t.Errorf("Verify error %q does not name the base table %q", err.Error(), c.SumTable)
	}
}

// TestVerify_PropagatesRetentionCheckQueryError confirms a Query failure on
// the retention pre-check (the FIRST query Verify issues when retention is
// active) surfaces as an error naming the base table, mirroring
// TestVerify_PropagatesQueryError / TestVerify_PropagatesBaseTableQueryError's
// per-read error-wrap coverage.
func TestVerify_PropagatesRetentionCheckQueryError(t *testing.T) {
	c := testColumns()
	withFrozenNow(t, time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC))
	conn := &fakeConn{rowsErr: errors.New("connection reset")}
	_, err := Verify(context.Background(), conn, c, testBefore, 0.01, 24*time.Hour)
	if err == nil {
		t.Fatal("expected an error from the retention-check Query failing")
	}
	if !contains(err.Error(), c.SumTable) {
		t.Errorf("Verify error %q does not name the base table %q", err.Error(), c.SumTable)
	}
}

// mixedFakeConn extends fakeConn's (MetricName, total) totals-response
// mechanism with a separate day-response for outsideRetentionDaysSQL's
// distinct single-column shape, letting one Conn drive Verify's full
// three-query path (day-scan + both totals reads) end to end.
type mixedFakeConn struct {
	fakeConn
	days []time.Time
}

func (c *mixedFakeConn) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	if contains(query, "ORDER BY toStartOfDay") {
		c.queries = append(c.queries, query)
		return &dayFakeRows{days: c.days}, nil
	}
	return c.fakeConn.Query(ctx, query, args...)
}

// TestVerify_ExcludesOutsideRetentionDaysFromMismatches is the core
// cerberus issue #2652 regression: a day already past the aggregate
// table's own retention is EXCLUDED from AggregateTotals / BaseTotals /
// Mismatches (both totals responses agree once that day's rows are
// dropped from both sides), and reported separately — with the resolved
// Retention + RetentionBoundary — on Report.OutsideRetentionDays, never as
// an unexplained FAIL.
func TestVerify_ExcludesOutsideRetentionDaysFromMismatches(t *testing.T) {
	c := testColumns()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	withFrozenNow(t, now)
	retention := 24 * time.Hour // boundary = 2026-08-25T12:00:00Z
	doomedDay := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

	conn := &mixedFakeConn{days: []time.Time{doomedDay}}
	conn.respond = map[string][][2]any{
		"`" + c.DeltaPrefixTable + "`": {{"http_requests_total", 100.0}},
		"`" + c.SumTable + "`":         {{"http_requests_total", 100.0}},
	}

	rep, err := Verify(context.Background(), conn, c, testBefore, 0.01, retention)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.Pass() {
		t.Fatalf("expected Pass()=true (doomed day excluded, remaining totals match), got Mismatches=%+v", rep.Mismatches)
	}
	if rep.Retention != retention {
		t.Errorf("Retention = %v; want %v", rep.Retention, retention)
	}
	wantBoundary := now.Add(-retention)
	if !rep.RetentionBoundary.Equal(wantBoundary) {
		t.Errorf("RetentionBoundary = %v; want %v", rep.RetentionBoundary, wantBoundary)
	}
	if len(rep.OutsideRetentionDays) != 1 || !rep.OutsideRetentionDays[0].Equal(doomedDay) {
		t.Errorf("OutsideRetentionDays = %v; want [%v]", rep.OutsideRetentionDays, doomedDay)
	}
	if len(conn.queries) != 5 {
		t.Fatalf("expected 5 queries (day-scan + column type + cutover day + aggregate + base), got %d: %v", len(conn.queries), conn.queries)
	}
}

// TestVerify_RealMismatchSurvivesRetentionExclusion confirms a GENUINE
// completeness gap (missed_metric, unrelated to the excluded day) is still
// reported as a Mismatch even while an unrelated day is excluded for
// being outside retention — the two mechanisms are independent, so
// exclusion never masks a real bug.
func TestVerify_RealMismatchSurvivesRetentionExclusion(t *testing.T) {
	c := testColumns()
	withFrozenNow(t, time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC))
	retention := 24 * time.Hour
	doomedDay := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

	conn := &mixedFakeConn{days: []time.Time{doomedDay}}
	conn.respond = map[string][][2]any{
		"`" + c.DeltaPrefixTable + "`": {{"http_requests_total", 100.0}},
		"`" + c.SumTable + "`":         {{"http_requests_total", 100.0}, {"missed_metric", 5.0}},
	}

	rep, err := Verify(context.Background(), conn, c, testBefore, 0.01, retention)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.Pass() {
		t.Fatal("expected a real mismatch (missed_metric) even with retention exclusion active")
	}
	if len(rep.Mismatches) != 1 || rep.Mismatches[0].MetricName != "missed_metric" {
		t.Errorf("Mismatches = %+v; want exactly missed_metric", rep.Mismatches)
	}
	if len(rep.OutsideRetentionDays) != 1 {
		t.Errorf("OutsideRetentionDays = %v; want exactly 1 excluded day", rep.OutsideRetentionDays)
	}
}

// TestBackfill_PropagatesRetentionCheckQueryError confirms a Query failure
// on Backfill's own pre-INSERT retention check surfaces as an error naming
// the base table, and — critically — that Backfill does NOT proceed to
// INSERT when the pre-check itself fails (an unknown retention state must
// never be treated as "safe to write").
func TestBackfill_PropagatesRetentionCheckQueryError(t *testing.T) {
	c := testColumns()
	withFrozenNow(t, time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC))
	conn := &fakeConn{rowsErr: errors.New("connection reset")}
	_, err := Backfill(context.Background(), conn, c, testBefore, 24*time.Hour)
	if err == nil {
		t.Fatal("expected an error from the retention-check Query failing")
	}
	if !contains(err.Error(), c.SumTable) {
		t.Errorf("Backfill error %q does not name the base table %q", err.Error(), c.SumTable)
	}
	if len(conn.execSQL) != 0 {
		t.Errorf("Backfill must not INSERT when the retention pre-check fails, got Exec calls: %v", conn.execSQL)
	}
}

// TestScanTotals_PropagatesScanError confirms a Scan failure on a row
// surfaces as an error rather than a silently-partial map.
func TestScanTotals_PropagatesScanError(t *testing.T) {
	conn := &singleRowsConn{rows: &erroringRows{hasRow: true, scanErr: errors.New("bad column type")}}
	if _, err := scanTotals(context.Background(), conn, "SELECT 1", nil); err == nil {
		t.Fatal("expected an error from a failing Scan")
	}
}

// TestScanTotals_PropagatesRowsErr confirms a trailing rows.Err() — the
// driver's way of reporting a mid-stream failure Next()'s own bool return
// can't distinguish from ordinary exhaustion — surfaces as an error too.
func TestScanTotals_PropagatesRowsErr(t *testing.T) {
	conn := &singleRowsConn{rows: &erroringRows{finalErr: errors.New("stream reset")}}
	if _, err := scanTotals(context.Background(), conn, "SELECT 1", nil); err == nil {
		t.Fatal("expected an error from a failing rows.Err()")
	}
}

// TestCutoverDaySQL pins the two reads queryCutoverDay issues: the sum
// table's timestamp column type from system.columns, then the day start of
// before split by chsql.SumDayStart — with no timezone argument for an
// unzoned column (the server's day, as the view's toStartOfDay(TimeUnix)
// runs) and with the column's timezone for a zoned one.
func TestCutoverDaySQL(t *testing.T) {
	typeSQL, typeArgs := sumTimestampTypeSQL(testColumns())
	wantType := "SELECT `type` FROM `system`.`columns` WHERE `database` = ? AND `table` = ? AND `name` = ?"
	if typeSQL != wantType {
		t.Errorf("sumTimestampTypeSQL sql =\n%s\nwant\n%s", typeSQL, wantType)
	}
	if fmt.Sprint(typeArgs) != "[otel otel_metrics_sum TimeUnix]" {
		t.Errorf("sumTimestampTypeSQL args = %v", typeArgs)
	}

	lit := "parseDateTime64BestEffort('2026-08-20 12:30:00.000000000Z', 9)"
	for _, tc := range []struct{ timezone, day string }{
		{"", "toStartOfDay(" + lit + ")"},
		{"Asia/Kolkata", "toStartOfDay(" + lit + ", 'Asia/Kolkata')"},
	} {
		sql, args := cutoverDaySQL(testBefore, tc.timezone)
		want := "SELECT toInt64(toUnixTimestamp(" + tc.day + ")), timezoneOf(" + tc.day + "), " +
			"toInt64(timeZoneOffset(" + tc.day + "))"
		if sql != want || len(args) != 0 {
			t.Errorf("cutoverDaySQL(%q) = %s %v\nwant %s []", tc.timezone, sql, args, want)
		}
	}
}

// TestQueryCutoverDay_NamesTheZonesDay pins that the reported cutover day
// is the instant ClickHouse returned, in the timezone it was split in, so
// it formats as that zone's calendar day: Kolkata's 2026-01-15 starts at
// 2026-01-14T18:30:00Z, which a UTC rendering would name as Jan 14.
func TestQueryCutoverDay_NamesTheZonesDay(t *testing.T) {
	start := time.Date(2026, 1, 14, 18, 30, 0, 0, time.UTC)
	conn := &scriptedConn{answers: [][]any{
		{"DateTime64(9, 'Asia/Kolkata')"},
		{start.Unix(), "Asia/Kolkata", int64(19800)},
	}}
	got, err := queryCutoverDay(context.Background(), conn, testColumns(), time.Date(2026, 1, 15, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("queryCutoverDay: %v", err)
	}
	if !got.Equal(start) || got.Format(time.DateOnly) != "2026-01-15" {
		t.Errorf("cutover = %v (%s); want %v, named 2026-01-15", got, got.Format(time.DateOnly), start)
	}
	if !contains(conn.queries[1], "'Asia/Kolkata')") {
		t.Errorf("the day split does not name the column's timezone: %s", conn.queries[1])
	}
}

// TestQueryCutoverDay_NoColumn confirms a sum table whose timestamp column
// system.columns does not list fails Verify rather than splitting at a
// guessed timezone.
func TestQueryCutoverDay_NoColumn(t *testing.T) {
	_, err := queryCutoverDay(context.Background(), &scriptedConn{}, testColumns(), testBefore)
	if !errors.Is(err, errNoRow) {
		t.Fatalf("queryCutoverDay error = %v; want errNoRow", err)
	}
}

// scriptedConn answers its Nth Query with the single row answers[N], or no
// row once they run out.
type scriptedConn struct {
	answers [][]any
	queries []string
}

func (c *scriptedConn) Exec(context.Context, string, ...any) error { return nil }

func (c *scriptedConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	c.queries = append(c.queries, query)
	if len(c.queries) > len(c.answers) {
		return &valueRows{}, nil
	}
	return &valueRows{data: [][]any{c.answers[len(c.queries)-1]}}, nil
}

// TestRowReads_PropagateDriverErrors pins that every single-row and day-list
// read surfaces the driver's own error — from Scan, and from Err either after
// the last row or in place of any row — rather than a zero value.
func TestRowReads_PropagateDriverErrors(t *testing.T) {
	scanErr, finalErr := errors.New("scan failed"), errors.New("stream failed")
	for _, tc := range []struct {
		name string
		rows *erroringRows
		want error
	}{
		{"scan error", &erroringRows{hasRow: true, scanErr: scanErr}, scanErr},
		{"stream error after the row", &erroringRows{hasRow: true, finalErr: finalErr}, finalErr},
		{"stream error with no row", &erroringRows{finalErr: finalErr}, finalErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dest string
			if err := scanRow(context.Background(), &singleRowsConn{rows: tc.rows}, "SELECT 1", nil, &dest); !errors.Is(err, tc.want) {
				t.Errorf("scanRow error = %v; want %v", err, tc.want)
			}
			tc.rows.yielded = false
			if err := scanRow(context.Background(), &fakeConn{rowsErr: tc.want}, "SELECT 1", nil, &dest); !errors.Is(err, tc.want) {
				t.Errorf("scanRow over a failed Query: error = %v; want %v", err, tc.want)
			}
			if _, err := queryOutsideRetentionDays(
				context.Background(), &singleRowsConn{rows: tc.rows}, testColumns(), testBefore, testBefore,
			); !errors.Is(err, tc.want) {
				t.Errorf("queryOutsideRetentionDays error = %v; want %v", err, tc.want)
			}
		})
	}
}

// TestVerify_PropagatesCutoverDayError pins that Verify stops, with the
// cause, when ClickHouse cannot compute the cutover day: the column type
// reads but the day query returns no row.
func TestVerify_PropagatesCutoverDayError(t *testing.T) {
	conn := &scriptedConn{answers: [][]any{{"DateTime64(9)"}}}
	_, err := Verify(context.Background(), conn, testColumns(), testBefore, 0, 0)
	if !errors.Is(err, errNoRow) || !strings.Contains(err.Error(), "compute the cutover day") {
		t.Fatalf("Verify error = %v; want the cutover-day read's errNoRow", err)
	}
	if len(conn.queries) != 2 {
		t.Errorf("Verify issued %d queries; want the two cutover reads and no totals read", len(conn.queries))
	}
}

package downsampletier

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/schema"
)

// testColumns builds a Columns value the way a deployment resolving
// schema.DefaultOTelMetrics() would — mirrors internal/deltaprefix's own
// testColumns.
func testColumns() Columns {
	return FromSchema("otel", schema.DefaultOTelMetrics())
}

var testBefore = time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)

// TestBackfillSQL pins the rendered INSERT ... SELECT shape: ONE statement
// per downsampleTierSources(c) entry (Sum, then Gauge — cerberus issue
// #2858), the target column list, the exact-instant-before WHERE bound (no
// temporality filter, unlike internal/deltaprefix — see this package's
// doc), and the ceiling-bucket GROUP BY matching internal/schema/ddl's MVs
// exactly. The Sum statement carries a real any(AggregationTemporality);
// the Gauge statement carries the fixed sentinel literal in its place —
// see schema.DownsampleTierGaugeTemporalitySentinel's own doc.
func TestBackfillSQL(t *testing.T) {
	stmts := BackfillSQL(testColumns(), testBefore)
	if len(stmts) != 2 {
		t.Fatalf("BackfillSQL returned %d statements; want 2 (Sum, Gauge)", len(stmts))
	}
	wantSum := "INSERT INTO otel.otel_metrics_sum_downsample_tier " +
		"(`MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, `BucketEnd`, `LastTwoSamples`, `Temporality`) " +
		"SELECT `MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, " +
		"toStartOfInterval(`TimeUnix` - toIntervalNanosecond(1), toIntervalSecond(300)) + toIntervalSecond(300) AS `BucketEnd`, " +
		"timeSeriesLastTwoSamplesState(`TimeUnix`, `Value`) AS `LastTwoSamples`, " +
		"any(`AggregationTemporality`) AS `Temporality` " +
		"FROM `otel`.`otel_metrics_sum` WHERE `TimeUnix` < ? " +
		"GROUP BY `MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, " +
		"toStartOfInterval(`TimeUnix` - toIntervalNanosecond(1), toIntervalSecond(300)) + toIntervalSecond(300)"
	if stmts[0].SQL != wantSum {
		t.Errorf("BackfillSQL[0] sql =\n%s\nwant\n%s", stmts[0].SQL, wantSum)
	}
	if len(stmts[0].Args) != 1 || stmts[0].Args[0] != testBefore {
		t.Errorf("BackfillSQL[0] args = %v; want [%v]", stmts[0].Args, testBefore)
	}
	wantGauge := "INSERT INTO otel.otel_metrics_sum_downsample_tier " +
		"(`MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, `BucketEnd`, `LastTwoSamples`, `Temporality`) " +
		"SELECT `MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, " +
		"toStartOfInterval(`TimeUnix` - toIntervalNanosecond(1), toIntervalSecond(300)) + toIntervalSecond(300) AS `BucketEnd`, " +
		"timeSeriesLastTwoSamplesState(`TimeUnix`, `Value`) AS `LastTwoSamples`, " +
		"-1 AS `Temporality` " +
		"FROM `otel`.`otel_metrics_gauge` WHERE `TimeUnix` < ? " +
		"GROUP BY `MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, " +
		"toStartOfInterval(`TimeUnix` - toIntervalNanosecond(1), toIntervalSecond(300)) + toIntervalSecond(300)"
	if stmts[1].SQL != wantGauge {
		t.Errorf("BackfillSQL[1] sql =\n%s\nwant\n%s", stmts[1].SQL, wantGauge)
	}
	if len(stmts[1].Args) != 1 || stmts[1].Args[0] != testBefore {
		t.Errorf("BackfillSQL[1] args = %v; want [%v]", stmts[1].Args, testBefore)
	}
}

// TestBackfillSQL_GaugeEqualsSumSkipsSecondStatement pins
// downsampleTierSources' collapse guard (cerberus issue #2858's own doc): a
// deployment schema pointing GaugeTable at the SAME physical table as
// SumTable gets exactly ONE statement, not two — a second MV/backfill over
// the identical source would double-write every bucket.
func TestBackfillSQL_GaugeEqualsSumSkipsSecondStatement(t *testing.T) {
	c := testColumns()
	c.GaugeTable = c.SumTable
	stmts := BackfillSQL(c, testBefore)
	if len(stmts) != 1 {
		t.Fatalf("BackfillSQL with GaugeTable==SumTable returned %d statements; want 1", len(stmts))
	}
}

// TestSourceTables pins the cmd/cerberus-facing helper: both configured
// sources by default, collapsing to just the Sum table when Gauge is
// configured identically to it (mirroring downsampleTierSources' own
// collapse guard).
func TestSourceTables(t *testing.T) {
	c := testColumns()
	got := SourceTables(c)
	want := []string{c.SumTable, c.GaugeTable}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("SourceTables = %v; want %v", got, want)
	}

	c.GaugeTable = c.SumTable
	got = SourceTables(c)
	if len(got) != 1 || got[0] != c.SumTable {
		t.Errorf("SourceTables with GaugeTable==SumTable = %v; want [%s]", got, c.SumTable)
	}
}

// TestRebuildSQL pins the SAME shape as BackfillSQL but with NO `before`
// bound at all — the full-history rebuild this package's doc explains is
// the recovery path for a suspected stranded persisted state.
func TestRebuildSQL(t *testing.T) {
	stmts := RebuildSQL(testColumns())
	if len(stmts) != 2 {
		t.Fatalf("RebuildSQL returned %d statements; want 2 (Sum, Gauge)", len(stmts))
	}
	for i, stmt := range stmts {
		if len(stmt.Args) != 0 {
			t.Errorf("RebuildSQL[%d] args = %v; want none (no --before bound)", i, stmt.Args)
		}
		if containsSubstr(stmt.SQL, "WHERE") {
			t.Errorf("RebuildSQL[%d] sql = %q; want no WHERE clause at all", i, stmt.SQL)
		}
	}
	backfillStmts := BackfillSQL(testColumns(), testBefore)
	if stmts[0].SQL == backfillStmts[0].SQL {
		t.Fatal("RebuildSQL[0] rendered identically to a bounded BackfillSQL[0] — the WHERE clause is missing")
	}
	wantPrefix := "INSERT INTO otel.otel_metrics_sum_downsample_tier "
	for i, stmt := range stmts {
		if len(stmt.SQL) < len(wantPrefix) || stmt.SQL[:len(wantPrefix)] != wantPrefix {
			t.Errorf("RebuildSQL[%d] sql = %q; want prefix %q", i, stmt.SQL, wantPrefix)
		}
	}
}

// TestTruncateSQL pins the TRUNCATE statement Rebuild issues before its
// full re-populate.
func TestTruncateSQL(t *testing.T) {
	got := TruncateSQL(testColumns())
	want := "TRUNCATE TABLE otel.otel_metrics_sum_downsample_tier"
	if got != want {
		t.Errorf("TruncateSQL = %q; want %q", got, want)
	}
}

// TestOutsideRetentionDaysSQL pins the DISTINCT bucket-boundary read used
// both by Backfill/Rebuild's pre-check and Verify's OutsideRetentionDays —
// mirrors internal/deltaprefix's own TestOutsideRetentionDaysSQL, adapted
// to this package's ceiling bucket expression and lack of a temporality
// filter.
func TestOutsideRetentionDaysSQL(t *testing.T) {
	c := testColumns()
	boundary := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	bucketExpr := "toStartOfInterval(`TimeUnix` - toIntervalNanosecond(1), toIntervalSecond(300)) + toIntervalSecond(300)"
	for _, tc := range []struct {
		source string
		table  string
	}{
		{c.SumTable, "otel_metrics_sum"},
		{c.GaugeTable, "otel_metrics_gauge"},
	} {
		sql, args := outsideRetentionDaysSQL(c, tc.source, testBefore, boundary)
		want := "SELECT " + bucketExpr + " FROM `otel`.`" + tc.table + "` " +
			"WHERE `TimeUnix` < ? AND " + bucketExpr + " < ? " +
			"GROUP BY " + bucketExpr + " ORDER BY " + bucketExpr
		if sql != want {
			t.Errorf("outsideRetentionDaysSQL(%s) sql =\n%s\nwant\n%s", tc.table, sql, want)
		}
		if len(args) != 2 || args[0] != testBefore || args[1] != boundary {
			t.Errorf("outsideRetentionDaysSQL(%s) args = %v; want [%v %v]", tc.table, args, testBefore, boundary)
		}
	}
}

// TestBaseAndTierBucketsSQL pins Verify's two completeness reads: DISTINCT
// bucket counts per metric, base table vs. tier table, both bounded by the
// exact-instant `before`.
func TestBaseAndTierBucketsSQL(t *testing.T) {
	c := testColumns()

	baseSQL, baseArgs := baseBucketsSQL(c, c.SumTable, testBefore, time.Time{}, false)
	bucketExpr := "toStartOfInterval(`TimeUnix` - toIntervalNanosecond(1), toIntervalSecond(300)) + toIntervalSecond(300)"
	wantBase := "SELECT `MetricName`, uniqExact(" + bucketExpr + ") AS `n` " +
		"FROM `otel`.`otel_metrics_sum` WHERE `TimeUnix` < ? GROUP BY `MetricName`"
	if baseSQL != wantBase {
		t.Errorf("baseBucketsSQL sql =\n%s\nwant\n%s", baseSQL, wantBase)
	}
	if len(baseArgs) != 1 || baseArgs[0] != testBefore {
		t.Errorf("baseBucketsSQL args = %v; want [%v]", baseArgs, testBefore)
	}

	gaugeSQL, _ := baseBucketsSQL(c, c.GaugeTable, testBefore, time.Time{}, false)
	wantGauge := "SELECT `MetricName`, uniqExact(" + bucketExpr + ") AS `n` " +
		"FROM `otel`.`otel_metrics_gauge` WHERE `TimeUnix` < ? GROUP BY `MetricName`"
	if gaugeSQL != wantGauge {
		t.Errorf("baseBucketsSQL(gauge) sql =\n%s\nwant\n%s", gaugeSQL, wantGauge)
	}

	tierSQL, tierArgs := tierBucketsSQL(c, testBefore, time.Time{}, false)
	wantTier := "SELECT `MetricName`, uniqExact(`BucketEnd`) AS `n` " +
		"FROM `otel`.`otel_metrics_sum_downsample_tier` WHERE `BucketEnd` < ? GROUP BY `MetricName`"
	if tierSQL != wantTier {
		t.Errorf("tierBucketsSQL sql =\n%s\nwant\n%s", tierSQL, wantTier)
	}
	if len(tierArgs) != 1 || tierArgs[0] != testBefore {
		t.Errorf("tierBucketsSQL args = %v; want [%v]", tierArgs, testBefore)
	}
}

// TestBaseAndTierBucketsSQL_RetentionActive pins the additional lower
// bound both reads gain when a retention boundary is active — mirrors
// internal/deltaprefix's own retention-active SQL test.
func TestBaseAndTierBucketsSQL_RetentionActive(t *testing.T) {
	c := testColumns()
	boundary := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

	baseSQL, baseArgs := baseBucketsSQL(c, c.SumTable, testBefore, boundary, true)
	bucketExpr := "toStartOfInterval(`TimeUnix` - toIntervalNanosecond(1), toIntervalSecond(300)) + toIntervalSecond(300)"
	wantBase := "SELECT `MetricName`, uniqExact(" + bucketExpr + ") AS `n` " +
		"FROM `otel`.`otel_metrics_sum` WHERE `TimeUnix` < ? AND " + bucketExpr + " >= ? GROUP BY `MetricName`"
	if baseSQL != wantBase {
		t.Errorf("baseBucketsSQL sql =\n%s\nwant\n%s", baseSQL, wantBase)
	}
	if len(baseArgs) != 2 || baseArgs[0] != testBefore || baseArgs[1] != boundary {
		t.Errorf("baseBucketsSQL args = %v; want [%v %v]", baseArgs, testBefore, boundary)
	}

	tierSQL, tierArgs := tierBucketsSQL(c, testBefore, boundary, true)
	wantTier := "SELECT `MetricName`, uniqExact(`BucketEnd`) AS `n` " +
		"FROM `otel`.`otel_metrics_sum_downsample_tier` WHERE `BucketEnd` < ? AND `BucketEnd` >= ? GROUP BY `MetricName`"
	if tierSQL != wantTier {
		t.Errorf("tierBucketsSQL sql =\n%s\nwant\n%s", tierSQL, wantTier)
	}
	if len(tierArgs) != 2 || tierArgs[0] != testBefore || tierArgs[1] != boundary {
		t.Errorf("tierBucketsSQL args = %v; want [%v %v]", tierArgs, testBefore, boundary)
	}
}

func TestRetentionBoundary(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	if boundary, active := retentionBoundary(now, 0); active || !boundary.IsZero() {
		t.Errorf("retentionBoundary(now, 0) = (%v, %v); want (zero, false)", boundary, active)
	}
	retention := 30 * 24 * time.Hour
	boundary, active := retentionBoundary(now, retention)
	if !active {
		t.Fatal("retentionBoundary: expected active=true for a positive retention")
	}
	want := now.Add(-retention)
	if !boundary.Equal(want) {
		t.Errorf("retentionBoundary boundary = %v; want %v", boundary, want)
	}
}

func TestDiff(t *testing.T) {
	base := map[string]int64{"m1": 10, "m2": 5, "m3": 3}
	tier := map[string]int64{"m1": 10, "m2": 4, "m4": 1}
	rep := Diff(base, tier, testBefore)
	if rep.Pass() {
		t.Fatal("expected Diff to report mismatches")
	}
	if len(rep.Mismatches) != 3 {
		t.Fatalf("Mismatches = %v; want 3 entries (m2, m3, m4)", rep.Mismatches)
	}
	byName := map[string]Mismatch{}
	for _, m := range rep.Mismatches {
		byName[m.MetricName] = m
	}
	if m, ok := byName["m2"]; !ok || m.BaseBuckets != 5 || m.TierBuckets != 4 || m.Missing() != 1 {
		t.Errorf("m2 mismatch = %+v; want BaseBuckets=5 TierBuckets=4 Missing=1", m)
	}
	if m, ok := byName["m3"]; !ok || m.BaseBuckets != 3 || m.TierBuckets != 0 || m.Missing() != 3 {
		t.Errorf("m3 mismatch = %+v; want BaseBuckets=3 TierBuckets=0 Missing=3", m)
	}
	if m, ok := byName["m4"]; !ok || m.BaseBuckets != 0 || m.TierBuckets != 1 || m.Missing() != 0 {
		t.Errorf("m4 mismatch = %+v; want BaseBuckets=0 TierBuckets=1 Missing=0 (tier ahead of base is not a completeness gap)", m)
	}
	if _, ok := byName["m1"]; ok {
		t.Error("m1 (equal counts) must not appear in Mismatches")
	}
}

func TestDiff_Pass(t *testing.T) {
	base := map[string]int64{"m1": 10}
	tier := map[string]int64{"m1": 10}
	rep := Diff(base, tier, testBefore)
	if !rep.Pass() {
		t.Errorf("expected Pass; mismatches = %v", rep.Mismatches)
	}
}

// --- fake Conn plumbing, mirroring internal/deltaprefix's own shape ---

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

// Scan handles BOTH row shapes this package's queries use: scanCounts'
// (MetricName string, n int64) pair (dest arity 2) and
// queryOutsideRetentionDays' single time.Time column (dest arity 1, reads
// only row[0]) — Verify's own retention-exclusion test needs both shapes
// dispatched off the SAME conn, unlike internal/deltaprefix's split
// fakeConn/dayFakeConn (whose Backfill/Verify tests never combine both
// reads on one scripted conn).
func (r *fakeRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	switch len(dest) {
	case 1:
		tp, ok := dest[0].(*time.Time)
		if !ok {
			return fmt.Errorf("fakeRows: dest[0] is %T, want *time.Time", dest[0])
		}
		*tp = row[0].(time.Time)
		return nil
	case 2:
		np, ok := dest[0].(*string)
		if !ok {
			return fmt.Errorf("fakeRows: dest[0] is %T, want *string", dest[0])
		}
		*np = row[0].(string)
		tp, ok := dest[1].(*int64)
		if !ok {
			return fmt.Errorf("fakeRows: dest[1] is %T, want *int64", dest[1])
		}
		*tp = row[1].(int64)
		return nil
	default:
		return fmt.Errorf("fakeRows: unsupported scan arity %d", len(dest))
	}
}

func (r *fakeRows) Err() error   { return nil }
func (r *fakeRows) Close() error { return nil }

// fakeConn is a scripted Conn: Query returns rows keyed by a substring
// match against the SQL, Exec records every statement + args it received —
// mirrors internal/deltaprefix's own fakeConn.
type fakeConn struct {
	execSQL  []string
	execArgs [][]any
	execErr  error
	// execErrOn, when non-zero, fails only the execErrOn-th Exec call
	// (1-indexed) rather than every one — used to reach a LATER statement's
	// own error-wrap branch (e.g. Rebuild's second, Gauge-sourced INSERT)
	// without also failing every earlier one.
	execErrOn int

	queries []string
	respond map[string][][2]any
	rowsErr error
}

func (c *fakeConn) Exec(_ context.Context, query string, args ...any) error {
	c.execSQL = append(c.execSQL, query)
	c.execArgs = append(c.execArgs, args)
	if c.execErrOn != 0 {
		if len(c.execSQL) == c.execErrOn {
			return c.execErr
		}
		return nil
	}
	return c.execErr
}

func (c *fakeConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	c.queries = append(c.queries, query)
	if c.rowsErr != nil {
		return nil, c.rowsErr
	}
	for match, rows := range c.respond {
		if containsSubstr(query, match) {
			return &fakeRows{data: rows}, nil
		}
	}
	return &fakeRows{}, nil
}

func containsSubstr(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// nthCallConn succeeds on every Query call except the failOn-th (1-indexed),
// which fails — used to reach a specific error-wrap branch among Verify's
// several sequential reads (retention check x2 sources, base-bucket reads x2
// sources, tier-bucket read x1 — cerberus issue #2858 widened the base read
// from one call to one per downsampleTierSources(c) entry).
type nthCallConn struct {
	calls  int
	failOn int
	err    error
}

func (c *nthCallConn) Exec(context.Context, string, ...any) error { return nil }

func (c *nthCallConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	c.calls++
	if c.calls == c.failOn {
		return nil, c.err
	}
	return &fakeRows{}, nil
}

func withFrozenNow(t *testing.T, now time.Time) {
	t.Helper()
	prev := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prev })
}

// TestBackfill_ExecutesRenderedSQL confirms Backfill hands conn EXACTLY the
// statements + args BackfillSQL renders — one per downsampleTierSources(c)
// entry (Sum, then Gauge — cerberus issue #2858) — and wraps a real Exec
// failure with enough context to identify the target table.
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
	wantStmts := BackfillSQL(c, testBefore)
	if len(conn.execSQL) != len(wantStmts) {
		t.Fatalf("Exec calls = %d; want %d (one per source)", len(conn.execSQL), len(wantStmts))
	}
	for i, want := range wantStmts {
		if conn.execSQL[i] != want.SQL {
			t.Errorf("Exec[%d] sql = %q; want %q", i, conn.execSQL[i], want.SQL)
		}
		if len(conn.execArgs[i]) != len(want.Args) {
			t.Errorf("Exec[%d] args = %v; want %v", i, conn.execArgs[i], want.Args)
		}
	}

	// A failing FIRST (Sum) Exec must stop before ever attempting the
	// second (Gauge) statement.
	failing := &fakeConn{execErr: errors.New("boom")}
	_, err = Backfill(context.Background(), failing, c, testBefore, 0)
	if err == nil {
		t.Fatal("expected error from a failing Exec")
	}
	if got := err.Error(); !containsSubstr(got, schema.DownsampleTierTable) {
		t.Errorf("Backfill error %q does not name the target table %q", got, schema.DownsampleTierTable)
	}
	if len(failing.execSQL) != 1 {
		t.Errorf("expected Backfill to stop after the first failing Exec; got %d Exec calls", len(failing.execSQL))
	}
}

func TestBackfill_DetectsOutsideRetentionDays(t *testing.T) {
	c := testColumns()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	withFrozenNow(t, now)
	retention := 30 * 24 * time.Hour
	outsideDay := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	conn := &fakeConn{
		respond: map[string][][2]any{
			"GROUP BY": {{outsideDay, int64(0)}},
		},
	}
	result, err := Backfill(context.Background(), conn, c, testBefore, retention)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	// The outside-retention check must run BEFORE the INSERT — one Query
	// per source (Sum, Gauge), both matching "GROUP BY" and both reporting
	// the SAME outsideDay, deduplicated by queryOutsideRetentionDays.
	if len(conn.queries) != 2 {
		t.Fatalf("expected exactly 2 Query calls (the retention check, one per source); got %d", len(conn.queries))
	}
	if len(conn.execSQL) != 2 {
		t.Fatalf("expected exactly 2 Exec calls (the INSERTs, one per source); got %d", len(conn.execSQL))
	}
	if len(result.OutsideRetentionDays) != 1 || !result.OutsideRetentionDays[0].Equal(outsideDay) {
		t.Errorf("OutsideRetentionDays = %v; want [%v] (deduplicated across both sources)", result.OutsideRetentionDays, outsideDay)
	}
}

func TestBackfill_PropagatesRetentionCheckQueryError(t *testing.T) {
	c := testColumns()
	withFrozenNow(t, time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC))
	conn := &fakeConn{rowsErr: errors.New("query failed")}
	_, err := Backfill(context.Background(), conn, c, testBefore, 30*24*time.Hour)
	if err == nil {
		t.Fatal("expected error from a failing retention-check Query")
	}
}

// TestQueryOutsideRetentionDays_PropagatesSecondSourceQueryError confirms
// the per-source loop (cerberus issue #2858) surfaces a failure from the
// SECOND source's own Query call — not just the first, which every other
// retention-check test above already covers via a conn that fails
// unconditionally.
func TestQueryOutsideRetentionDays_PropagatesSecondSourceQueryError(t *testing.T) {
	c := testColumns()
	conn := &nthCallConn{failOn: 2, err: errors.New("gauge retention query failed")}
	_, err := queryOutsideRetentionDays(context.Background(), conn, c, testBefore, time.Time{})
	if err == nil {
		t.Fatal("expected an error from the second (Gauge) source's Query")
	}
	if !containsSubstr(err.Error(), c.GaugeTable) {
		t.Errorf("error %q does not name the Gauge source table", err.Error())
	}
}

// TestQueryOutsideRetentionDays_PropagatesScanError confirms a Scan failure
// on the retention-day read is wrapped with the failing source's table name,
// mirroring scanCounts' own PropagatesScanError coverage for the sibling
// bucket-count reads.
func TestQueryOutsideRetentionDays_PropagatesScanError(t *testing.T) {
	c := testColumns()
	conn := &singleRowsConn{rows: &erroringRows{hasRow: true, scanErr: errors.New("scan failed")}}
	_, err := queryOutsideRetentionDays(context.Background(), conn, c, testBefore, time.Time{})
	if err == nil {
		t.Fatal("expected an error from a failing Scan")
	}
	if !containsSubstr(err.Error(), c.SumTable) {
		t.Errorf("error %q does not name the source table", err.Error())
	}
}

// TestRebuild_TruncatesThenInserts confirms Rebuild issues TRUNCATE before
// the unbounded INSERT ... SELECT statements (one per source — cerberus
// issue #2858), in that order, and surfaces outside-retention days computed
// against `now` rather than any --before bound.
func TestRebuild_TruncatesThenInserts(t *testing.T) {
	c := testColumns()
	conn := &fakeConn{}
	result, err := Rebuild(context.Background(), conn, c, 0)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if len(result.OutsideRetentionDays) != 0 {
		t.Errorf("OutsideRetentionDays = %v; want none with retention=0", result.OutsideRetentionDays)
	}
	wantInserts := RebuildSQL(c)
	if len(conn.execSQL) != 1+len(wantInserts) {
		t.Fatalf("Exec calls = %d; want %d (TRUNCATE then one INSERT per source)", len(conn.execSQL), 1+len(wantInserts))
	}
	if conn.execSQL[0] != TruncateSQL(c) {
		t.Errorf("first Exec = %q; want TRUNCATE %q", conn.execSQL[0], TruncateSQL(c))
	}
	for i, want := range wantInserts {
		if conn.execSQL[1+i] != want.SQL {
			t.Errorf("Exec[%d] = %q; want %q", 1+i, conn.execSQL[1+i], want.SQL)
		}
	}

	failingTruncate := &fakeConn{execErr: errors.New("truncate failed")}
	_, err = Rebuild(context.Background(), failingTruncate, c, 0)
	if err == nil {
		t.Fatal("expected error from a failing TRUNCATE")
	}
	if len(failingTruncate.execSQL) != 1 {
		t.Errorf("expected Rebuild to stop after a failing TRUNCATE, not attempt any INSERT; got %d Exec calls", len(failingTruncate.execSQL))
	}
}

// TestRebuild_StopsAfterFirstFailingInsert confirms Rebuild stops issuing
// further INSERT statements (cerberus issue #2858: now one per source) as
// soon as one fails, rather than attempting every remaining source's own
// statement regardless. execErrOn=3 fails the SECOND insert (Exec call 3:
// TRUNCATE, Sum insert, Gauge insert).
func TestRebuild_StopsAfterFirstFailingInsert(t *testing.T) {
	c := testColumns()
	conn := &fakeConn{execErrOn: 3, execErr: errors.New("gauge insert failed")}
	_, err := Rebuild(context.Background(), conn, c, 0)
	if err == nil {
		t.Fatal("expected error from the second (Gauge) INSERT failing")
	}
	if !containsSubstr(err.Error(), schema.DownsampleTierTable) {
		t.Errorf("Rebuild error %q does not name the target table", err.Error())
	}
	if len(conn.execSQL) != 3 {
		t.Errorf("Exec calls = %d; want 3 (TRUNCATE, Sum insert, the failing Gauge insert) — "+
			"no statement past the failure should run", len(conn.execSQL))
	}
}

func TestRebuild_PropagatesRetentionCheckQueryError(t *testing.T) {
	c := testColumns()
	withFrozenNow(t, time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC))
	conn := &fakeConn{rowsErr: errors.New("query failed")}
	_, err := Rebuild(context.Background(), conn, c, 30*24*time.Hour)
	if err == nil {
		t.Fatal("expected error from a failing retention-check Query")
	}
	if len(conn.execSQL) != 0 {
		t.Error("expected Rebuild to never TRUNCATE when the retention check itself fails")
	}
}

// TestVerify_ReadsBothBucketCountsAndDiffs confirms baseBucketCounts SUMS
// per-metric-name counts across BOTH sources (cerberus issue #2858: "m2"
// only exists in the Gauge table's response here, proving its count is not
// silently dropped just because it's absent from the Sum table's read) and
// still diffs correctly against the tier's own combined count.
func TestVerify_ReadsBothBucketCountsAndDiffs(t *testing.T) {
	c := testColumns()
	// Match keys must be MUTUALLY EXCLUSIVE substrings — a bare
	// c.SumTable ("otel_metrics_sum") is itself a substring of
	// schema.DownsampleTierTable ("otel_metrics_sum_downsample_tier"), so
	// containsSubstr would match BOTH queries against whichever key map
	// iteration (random order) happens to check first. The closing
	// backtick right after the table name disambiguates: it appears in
	// the base tables' own quoted references but not inside the tier
	// table's (longer) one.
	conn := &fakeConn{
		respond: map[string][][2]any{
			"`" + schema.DownsampleTierTable + "`": {{"m1", int64(4)}, {"m2", int64(2)}},
			"`" + c.SumTable + "`":                 {{"m1", int64(5)}},
			"`" + c.GaugeTable + "`":               {{"m2", int64(2)}},
		},
	}
	rep, err := Verify(context.Background(), conn, c, testBefore, 0)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.Pass() {
		t.Fatal("expected a mismatch (base=5, tier=4 for m1)")
	}
	if len(rep.Mismatches) != 1 || rep.Mismatches[0].MetricName != "m1" {
		t.Errorf("Mismatches = %v; want one entry for m1 (m2's Gauge-sourced base/tier counts agree at 2)", rep.Mismatches)
	}
}

func TestVerify_PropagatesBaseTableQueryError(t *testing.T) {
	c := testColumns()
	// retention=0 skips the retention-day check entirely, so Verify's own
	// read order is: base-Sum (call 1), base-Gauge (call 2), tier (call 3).
	// failOn=3 reaches the tier read's own error-wrap branch.
	conn := &nthCallConn{failOn: 3, err: errors.New("boom")}
	_, err := Verify(context.Background(), conn, c, testBefore, 0)
	if err == nil {
		t.Fatal("expected an error from the third (tier table) Query")
	}
	if !containsSubstr(err.Error(), schema.DownsampleTierTable) {
		t.Errorf("Verify error %q does not name the tier table", err.Error())
	}
}

// TestVerify_PropagatesGaugeBaseTableQueryError mirrors
// TestVerify_PropagatesBaseTableQueryError for the SECOND base read (the
// Gauge source, cerberus issue #2858) — proving baseBucketCounts surfaces a
// failure from either configured source, not just the first.
func TestVerify_PropagatesGaugeBaseTableQueryError(t *testing.T) {
	c := testColumns()
	conn := &nthCallConn{failOn: 2, err: errors.New("boom")}
	_, err := Verify(context.Background(), conn, c, testBefore, 0)
	if err == nil {
		t.Fatal("expected an error from the second (Gauge base) Query")
	}
	if !containsSubstr(err.Error(), c.GaugeTable) {
		t.Errorf("Verify error %q does not name the Gauge source table", err.Error())
	}
}

func TestVerify_PropagatesRetentionCheckQueryError(t *testing.T) {
	c := testColumns()
	withFrozenNow(t, time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC))
	conn := &fakeConn{rowsErr: errors.New("query failed")}
	_, err := Verify(context.Background(), conn, c, testBefore, 30*24*time.Hour)
	if err == nil {
		t.Fatal("expected error from a failing retention-check Query")
	}
}

func TestVerify_ExcludesOutsideRetentionDaysFromReport(t *testing.T) {
	c := testColumns()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	withFrozenNow(t, now)
	retention := 30 * 24 * time.Hour
	outsideDay := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	boundary, _ := retentionBoundary(now, retention)
	// Five distinct queries land on this one conn (the retention-day check
	// and the base-bucket read, each once per source, plus tierBucketsSQL)
	// — key the Sum source's two responses by their OWN exact rendered SQL
	// (derived from the real renderers, not a hand-guessed substring) so
	// none can accidentally match another; the Gauge source's two queries
	// fall through to the default empty response, contributing nothing.
	dayQuery, _ := outsideRetentionDaysSQL(c, c.SumTable, testBefore, boundary)
	baseQuery, _ := baseBucketsSQL(c, c.SumTable, testBefore, boundary, true)
	conn := &fakeConn{
		respond: map[string][][2]any{
			dayQuery:  {{outsideDay, int64(0)}},
			baseQuery: {{"m1", int64(3)}},
		},
	}
	rep, err := Verify(context.Background(), conn, c, testBefore, retention)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(rep.OutsideRetentionDays) != 1 || !rep.OutsideRetentionDays[0].Equal(outsideDay) {
		t.Errorf("OutsideRetentionDays = %v; want [%v]", rep.OutsideRetentionDays, outsideDay)
	}
	if rep.Retention != retention {
		t.Errorf("Retention = %v; want %v", rep.Retention, retention)
	}
}

func TestScanCounts_PropagatesScanError(t *testing.T) {
	conn := &singleRowsConn{rows: &erroringRows{hasRow: true, scanErr: errors.New("scan failed")}}
	_, err := scanCounts(context.Background(), conn, "SELECT 1", nil)
	if err == nil {
		t.Fatal("expected an error from a failing Scan")
	}
}

func TestScanCounts_PropagatesRowsErr(t *testing.T) {
	conn := &singleRowsConn{rows: &erroringRows{hasRow: false, finalErr: errors.New("rows.Err failed")}}
	_, err := scanCounts(context.Background(), conn, "SELECT 1", nil)
	if err == nil {
		t.Fatal("expected an error from a failing rows.Err()")
	}
}

// erroringRows is a driver.Rows that fails on either Scan or Err (never
// both) instead of yielding real data — mirrors internal/deltaprefix's own
// erroringRows.
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

// singleRowsConn is a Conn whose Query always returns one fixed
// driver.Rows, for pairing with erroringRows.
type singleRowsConn struct{ rows driver.Rows }

func (c *singleRowsConn) Exec(context.Context, string, ...any) error { return nil }

func (c *singleRowsConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return c.rows, nil
}

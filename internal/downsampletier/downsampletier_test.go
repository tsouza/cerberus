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

// TestBackfillSQL pins the rendered INSERT ... SELECT shape: the target
// column list, the exact-instant-before WHERE bound (no temporality
// filter, unlike internal/deltaprefix — see this package's doc), and the
// ceiling-bucket GROUP BY matching internal/schema/ddl's MV exactly.
func TestBackfillSQL(t *testing.T) {
	sql, args := BackfillSQL(testColumns(), testBefore)
	want := "INSERT INTO otel.otel_metrics_sum_downsample_tier " +
		"(`MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, `BucketEnd`, `LastTwoSamples`, `Temporality`) " +
		"SELECT `MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, " +
		"toStartOfInterval(`TimeUnix` - toIntervalNanosecond(1), toIntervalSecond(300)) + toIntervalSecond(300) AS `BucketEnd`, " +
		"timeSeriesLastTwoSamplesState(`TimeUnix`, `Value`) AS `LastTwoSamples`, " +
		"any(`AggregationTemporality`) AS `Temporality` " +
		"FROM `otel`.`otel_metrics_sum` WHERE `TimeUnix` < ? " +
		"GROUP BY `MetricName`, `Attributes`, `ResourceAttributes`, `ServiceName`, " +
		"toStartOfInterval(`TimeUnix` - toIntervalNanosecond(1), toIntervalSecond(300)) + toIntervalSecond(300)"
	if sql != want {
		t.Errorf("BackfillSQL sql =\n%s\nwant\n%s", sql, want)
	}
	if len(args) != 1 || args[0] != testBefore {
		t.Errorf("BackfillSQL args = %v; want [%v]", args, testBefore)
	}
}

// TestRebuildSQL pins the SAME shape as BackfillSQL but with NO `before`
// bound at all — the full-history rebuild this package's doc explains is
// the recovery path for a suspected stranded persisted state.
func TestRebuildSQL(t *testing.T) {
	sql, args := RebuildSQL(testColumns())
	if len(args) != 0 {
		t.Errorf("RebuildSQL args = %v; want none (no --before bound)", args)
	}
	backfillSQL, _ := BackfillSQL(testColumns(), testBefore)
	if sql == backfillSQL {
		t.Fatal("RebuildSQL rendered identically to a bounded BackfillSQL — the WHERE clause is missing")
	}
	wantPrefix := "INSERT INTO otel.otel_metrics_sum_downsample_tier "
	if len(sql) < len(wantPrefix) || sql[:len(wantPrefix)] != wantPrefix {
		t.Errorf("RebuildSQL sql = %q; want prefix %q", sql, wantPrefix)
	}
	if containsSubstr(sql, "WHERE") {
		t.Errorf("RebuildSQL sql = %q; want no WHERE clause at all", sql)
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
	boundary := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	sql, args := outsideRetentionDaysSQL(testColumns(), testBefore, boundary)
	bucketExpr := "toStartOfInterval(`TimeUnix` - toIntervalNanosecond(1), toIntervalSecond(300)) + toIntervalSecond(300)"
	want := "SELECT " + bucketExpr + " FROM `otel`.`otel_metrics_sum` " +
		"WHERE `TimeUnix` < ? AND " + bucketExpr + " < ? " +
		"GROUP BY " + bucketExpr + " ORDER BY " + bucketExpr
	if sql != want {
		t.Errorf("outsideRetentionDaysSQL sql =\n%s\nwant\n%s", sql, want)
	}
	if len(args) != 2 || args[0] != testBefore || args[1] != boundary {
		t.Errorf("outsideRetentionDaysSQL args = %v; want [%v %v]", args, testBefore, boundary)
	}
}

// TestBaseAndTierBucketsSQL pins Verify's two completeness reads: DISTINCT
// bucket counts per metric, base table vs. tier table, both bounded by the
// exact-instant `before`.
func TestBaseAndTierBucketsSQL(t *testing.T) {
	c := testColumns()

	baseSQL, baseArgs := baseBucketsSQL(c, testBefore, time.Time{}, false)
	bucketExpr := "toStartOfInterval(`TimeUnix` - toIntervalNanosecond(1), toIntervalSecond(300)) + toIntervalSecond(300)"
	wantBase := "SELECT `MetricName`, uniqExact(" + bucketExpr + ") AS `n` " +
		"FROM `otel`.`otel_metrics_sum` WHERE `TimeUnix` < ? GROUP BY `MetricName`"
	if baseSQL != wantBase {
		t.Errorf("baseBucketsSQL sql =\n%s\nwant\n%s", baseSQL, wantBase)
	}
	if len(baseArgs) != 1 || baseArgs[0] != testBefore {
		t.Errorf("baseBucketsSQL args = %v; want [%v]", baseArgs, testBefore)
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

	baseSQL, baseArgs := baseBucketsSQL(c, testBefore, boundary, true)
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

// twoCallConn succeeds on its first Query and fails on its second — the
// shape Verify's own two reads (base table, then tier table) need to reach
// its second error-wrap branch.
type twoCallConn struct {
	calls int
	err   error
}

func (c *twoCallConn) Exec(context.Context, string, ...any) error { return nil }

func (c *twoCallConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	c.calls++
	if c.calls == 2 {
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
// statement + args BackfillSQL renders, and wraps a real Exec failure with
// enough context to identify the target table.
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
	if got := err.Error(); !containsSubstr(got, schema.DownsampleTierTable) {
		t.Errorf("Backfill error %q does not name the target table %q", got, schema.DownsampleTierTable)
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
	// The outside-retention check must run BEFORE the INSERT.
	if len(conn.queries) != 1 {
		t.Fatalf("expected exactly 1 Query (the retention check); got %d", len(conn.queries))
	}
	if len(conn.execSQL) != 1 {
		t.Fatalf("expected exactly 1 Exec (the INSERT); got %d", len(conn.execSQL))
	}
	if len(result.OutsideRetentionDays) != 1 || !result.OutsideRetentionDays[0].Equal(outsideDay) {
		t.Errorf("OutsideRetentionDays = %v; want [%v]", result.OutsideRetentionDays, outsideDay)
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

// TestRebuild_TruncatesThenInserts confirms Rebuild issues TRUNCATE before
// the unbounded INSERT ... SELECT, in that order, and surfaces
// outside-retention days computed against `now` rather than any --before
// bound.
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
	if len(conn.execSQL) != 2 {
		t.Fatalf("Exec calls = %d; want 2 (TRUNCATE then INSERT)", len(conn.execSQL))
	}
	if conn.execSQL[0] != TruncateSQL(c) {
		t.Errorf("first Exec = %q; want TRUNCATE %q", conn.execSQL[0], TruncateSQL(c))
	}
	wantInsert, _ := RebuildSQL(c)
	if conn.execSQL[1] != wantInsert {
		t.Errorf("second Exec = %q; want %q", conn.execSQL[1], wantInsert)
	}

	failingTruncate := &fakeConn{execErr: errors.New("truncate failed")}
	_, err = Rebuild(context.Background(), failingTruncate, c, 0)
	if err == nil {
		t.Fatal("expected error from a failing TRUNCATE")
	}
	if len(failingTruncate.execSQL) != 1 {
		t.Errorf("expected Rebuild to stop after a failing TRUNCATE, not attempt the INSERT; got %d Exec calls", len(failingTruncate.execSQL))
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

func TestVerify_ReadsBothBucketCountsAndDiffs(t *testing.T) {
	c := testColumns()
	// Match keys must be MUTUALLY EXCLUSIVE substrings — a bare
	// c.SumTable ("otel_metrics_sum") is itself a substring of
	// schema.DownsampleTierTable ("otel_metrics_sum_downsample_tier"), so
	// containsSubstr would match BOTH queries against whichever key map
	// iteration (random order) happens to check first. The closing
	// backtick right after the table name disambiguates: it appears in
	// the base table's own quoted reference but not inside the tier
	// table's (longer) one.
	conn := &fakeConn{
		respond: map[string][][2]any{
			"`" + schema.DownsampleTierTable + "`": {{"m1", int64(4)}},
			"`" + c.SumTable + "`":                 {{"m1", int64(5)}},
		},
	}
	rep, err := Verify(context.Background(), conn, c, testBefore, 0)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.Pass() {
		t.Fatal("expected a mismatch (base=5, tier=4)")
	}
	if len(rep.Mismatches) != 1 || rep.Mismatches[0].MetricName != "m1" {
		t.Errorf("Mismatches = %v; want one entry for m1", rep.Mismatches)
	}
}

func TestVerify_PropagatesBaseTableQueryError(t *testing.T) {
	c := testColumns()
	conn := &twoCallConn{err: errors.New("boom")}
	// twoCallConn fails on its SECOND call. Verify's own read order is
	// base table first, then tier table — see Verify's own doc — so the
	// second call belongs to the TIER table read.
	_, err := Verify(context.Background(), conn, c, testBefore, 0)
	if err == nil {
		t.Fatal("expected an error from the second (tier table) Query")
	}
	if !containsSubstr(err.Error(), schema.DownsampleTierTable) {
		t.Errorf("Verify error %q does not name the tier table", err.Error())
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
	// Three distinct queries land on this one conn (the retention-day
	// check, baseBucketsSQL, tierBucketsSQL) — key each response by its
	// OWN exact rendered SQL (derived from the real renderers, not a
	// hand-guessed substring) so none can accidentally match another.
	dayQuery, _ := outsideRetentionDaysSQL(c, testBefore, boundary)
	baseQuery, _ := baseBucketsSQL(c, testBefore, boundary, true)
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

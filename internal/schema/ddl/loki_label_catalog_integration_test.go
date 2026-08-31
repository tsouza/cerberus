//go:build integration

package ddl_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// viewRefreshRow is the subset of system.view_refreshes this file reads —
// the SAME columns internal/chclient.QueryViewRefreshState reads in
// production, queried directly here (test-only administrative SQL, not the
// application read path) so this test does not depend on that package.
// Verified live against a ClickHouse 25.9 server
// (`SELECT name FROM system.columns WHERE table='view_refreshes'`): there
// is NO "last refresh result" enum column and NO refresh-count column —
// success/failure is read off exception (empty on success) plus comparing
// last_success_time against last_refresh_time.
type viewRefreshRow struct {
	status          string
	exception       string
	lastSuccessTime string
	lastRefreshTime string
}

// succeeded reports whether the most recently COMPLETED attempt this row
// describes was a success: no exception text, and a non-empty
// last_success_time. Deliberately does NOT also require last_success_time
// == last_refresh_time: both are set as part of the same successful
// attempt, but system.view_refreshes' DateTime (second-granularity)
// columns can observably land one apart even within a single successful
// completion — verified live (a real "Finished" attempt read
// last_success_time one second behind last_refresh_time) — so an exact
// equality check is a false-negative risk exception alone does not carry.
func (r viewRefreshRow) succeeded() bool {
	return r.exception == "" && r.lastSuccessTime != ""
}

// queryViewRefresh reads system.view_refreshes for one (database, view).
func queryViewRefresh(ctx context.Context, t *testing.T, conn driver.Conn, database, view string) (viewRefreshRow, bool) {
	t.Helper()
	rows, err := conn.Query(ctx,
		"SELECT status, exception, ifNull(toString(last_success_time), ''), ifNull(toString(last_refresh_time), '') "+
			"FROM system.view_refreshes WHERE database = ? AND view = ?",
		database, view)
	if err != nil {
		t.Fatalf("query view_refreshes: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return viewRefreshRow{}, false
	}
	var r viewRefreshRow
	if err := rows.Scan(&r.status, &r.exception, &r.lastSuccessTime, &r.lastRefreshTime); err != nil {
		t.Fatalf("scan view_refreshes: %v", err)
	}
	return r, true
}

// waitForRefreshPast blocks until system.view_refreshes reports a
// last_refresh_time strictly newer than after (a refresh attempt —
// success or failure — completed since the caller's baseline reading) or
// the timeout elapses, returning the row it observed. Polling (rather than
// trusting `SYSTEM REFRESH VIEW` to block) is deliberate: ClickHouse does
// not document that statement as synchronous across versions, so this test
// proves the OUTCOME (an attempt completed) rather than assuming a
// particular trigger-call blocking behavior.
func waitForRefreshPast(ctx context.Context, t *testing.T, conn driver.Conn, database, view, after string, timeout time.Duration) viewRefreshRow {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		row, found := queryViewRefresh(ctx, t, conn, database, view)
		// status=="Running" means an attempt is IN FLIGHT — last_refresh_time
		// can already show a new value at that point (verified live: it is
		// not exclusively a post-completion field), so the wait is not over
		// until the scheduler settles back to an idle state.
		if found && row.status != "Running" && row.lastRefreshTime != "" && row.lastRefreshTime != after {
			return row
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s.%s's last_refresh_time to advance past %q and settle out of Running (last observed: found=%v %+v)",
				timeout, database, view, after, found, row)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// secondBoundaryMargin bounds sleepPastSecondBoundary's wait — comfortably
// over one second so a second-granularity DateTime column reliably reads a
// new value across the sleep, with margin for scheduling jitter.
const secondBoundaryMargin = 1100 * time.Millisecond

// sleepPastSecondBoundary waits long enough to guarantee the NEXT
// system.view_refreshes row this test reads carries a last_refresh_time
// distinguishable from the one just observed. last_refresh_time is a plain
// (second-granularity) DateTime — verified live via `DESCRIBE
// system.view_refreshes` — so two attempts completing within the same
// wall-clock second are indistinguishable by timestamp alone; this test
// drives CREATE, RENAME and SYSTEM REFRESH VIEW back-to-back fast enough
// locally that it can otherwise happen.
func sleepPastSecondBoundary() {
	time.Sleep(secondBoundaryMargin)
}

// queryLabelCardinalities reads the catalog table the same shape
// internal/api/loki's read path does — SELECT LabelKey,
// uniqMerge(CardinalityState) GROUP BY LabelKey — returned as a map for
// easy comparison.
func queryLabelCardinalities(ctx context.Context, t *testing.T, conn driver.Conn, database, table string) map[string]uint64 {
	t.Helper()
	rows, err := conn.Query(ctx, fmt.Sprintf(
		"SELECT LabelKey, uniqMerge(CardinalityState) FROM %s.%s GROUP BY LabelKey", database, table,
	))
	if err != nil {
		t.Fatalf("query catalog table: %v", err)
	}
	defer rows.Close()
	out := map[string]uint64{}
	for rows.Next() {
		var (
			key   string
			count uint64
		)
		if err := rows.Scan(&key, &count); err != nil {
			t.Fatalf("scan catalog row: %v", err)
		}
		out[key] = count
	}
	return out
}

// insertLogRow inserts one otel_logs row carrying only Timestamp +
// ResourceAttributes — every other column takes its schema default (empty
// string / empty map / zero), which is all buildLabelCatalogSQL's ARRAY
// JOIN over ResourceAttributes needs.
func insertLogRow(ctx context.Context, t *testing.T, conn driver.Conn, database string, ts time.Time, attrs map[string]string) {
	t.Helper()
	stmt := fmt.Sprintf("INSERT INTO %s.otel_logs (Timestamp, ResourceAttributes) VALUES (?, ?)", database)
	if err := conn.Exec(ctx, stmt, ts, attrs); err != nil {
		t.Fatalf("insert otel_logs row: %v", err)
	}
}

// mapEq is a small equality helper — map[string]uint64 has no built-in
// comparable equality assertion in the stdlib testing package.
func mapEq(a, b map[string]uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestLokiLabelCatalog_RefreshAndFailureMode is the load-bearing proof for
// cerberus issue #2770's central claim: a refreshable materialized view's
// atomic target swap means a FAILED scheduled refresh keeps serving the
// PREVIOUS successful snapshot, not a partial or empty result — verified
// against a real ClickHouse server rather than assumed from the upstream
// design doc. It also pins system.view_refreshes' REAL shape: an earlier
// draft of this feature assumed a "last refresh result" enum column and a
// refresh counter column that turn out NOT to exist on a live 25.9 server
// (see viewRefreshRow's doc comment) — this test's assertions read the
// columns that actually exist (status, exception, last_success_time,
// last_refresh_time).
//
// Sequence:
//  1. Create otel_logs (catalog DISABLED), seed it, THEN enable the catalog
//     — so the view's first refresh has real data to aggregate from the
//     moment it exists (idempotent re-Apply is the same path a deployment
//     enabling this feature on an already-ingesting cluster takes).
//  2. Wait for the first refresh to complete; assert the catalog matches
//     the hand-computed cardinalities of the seeded rows.
//  3. Break the view's source (RENAME otel_logs away) and force another
//     refresh; wait for the attempt to complete, assert it recorded as a
//     FAILURE (exception populated, last_success_time did NOT advance to
//     match the new last_refresh_time), and assert the catalog STILL reads
//     the step-2 snapshot, byte-for-byte — nothing was swapped in from the
//     failed attempt.
//  4. Restore the source with a NEW row under a brand-new label key, force
//     a third refresh, wait for it to complete as a SUCCESS (exception
//     clears, last_success_time catches back up), and assert the catalog
//     now carries the new key — proving the swap mechanism resumes
//     normally once the source is healthy again, not stuck wedged from
//     step 3.
func TestLokiLabelCatalog_RefreshAndFailureMode(t *testing.T) {
	conn, db := startClickHouse(t)
	ctx := context.Background()

	const (
		catalogTable = "loki_label_catalog"
		catalogView  = "loki_label_catalog_mv"
	)

	// Step 1: base logs table only, then seed, then enable the catalog.
	baseCfg := ddl.Config{Database: db}
	if err := ddl.ApplyWithConfig(ctx, conn, baseCfg, []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("Apply (base logs table): %v", err)
	}

	now := time.Now().UTC()
	insertLogRow(ctx, t, conn, db, now, map[string]string{"job": "api", "env": "prod"})
	insertLogRow(ctx, t, conn, db, now, map[string]string{"job": "worker", "env": "prod"})
	insertLogRow(ctx, t, conn, db, now, map[string]string{"job": "api", "env": "staging"})

	catalogCfg := ddl.Config{Database: db, LokiLabelCatalogEnabled: true}
	if err := ddl.ApplyWithConfig(ctx, conn, catalogCfg, []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("Apply (enable catalog): %v", err)
	}

	// Step 2: wait for the first refresh (CREATE MATERIALIZED VIEW ...
	// REFRESH runs an initial refresh as part of the CREATE — verified
	// live — so baseline "" always advances past it) and check the
	// catalog.
	first := waitForRefreshPast(ctx, t, conn, db, catalogView, "", 60*time.Second)
	if !first.succeeded() {
		t.Fatalf("first refresh did not succeed: %+v", first)
	}
	wantAfterFirst := map[string]uint64{"job": 2, "env": 2}
	gotAfterFirst := queryLabelCardinalities(ctx, t, conn, db, catalogTable)
	if !mapEq(gotAfterFirst, wantAfterFirst) {
		t.Fatalf("catalog after first refresh = %+v, want %+v", gotAfterFirst, wantAfterFirst)
	}

	// Step 3: break the source and force a failing refresh. last_refresh_time
	// is a plain (second-granularity) DateTime, and this whole sequence runs
	// fast enough locally that the next attempt can otherwise complete
	// within the SAME wall-clock second as the CREATE's initial refresh —
	// sleepPastSecondBoundary crosses a full second first so the comparison
	// against first.lastRefreshTime below is meaningful.
	if err := conn.Exec(ctx, fmt.Sprintf("RENAME TABLE %s.otel_logs TO %s.otel_logs_broken", db, db)); err != nil {
		t.Fatalf("rename otel_logs away: %v", err)
	}
	sleepPastSecondBoundary()
	if err := conn.Exec(ctx, fmt.Sprintf("SYSTEM REFRESH VIEW %s.%s", db, catalogView)); err != nil {
		// A synchronous driver-side error here is ALSO evidence of a failed
		// refresh attempt on some ClickHouse versions; either way the
		// assertions below (exception populated, catalog unchanged) are
		// what actually matter.
		t.Logf("SYSTEM REFRESH VIEW returned an error (tolerated): %v", err)
	}
	second := waitForRefreshPast(ctx, t, conn, db, catalogView, first.lastRefreshTime, 60*time.Second)
	if second.succeeded() {
		t.Fatalf("expected the second refresh to FAIL (source table renamed away), but it reads as succeeded: %+v", second)
	}
	if second.exception == "" {
		t.Errorf("expected a non-empty exception on the failed refresh, got: %+v", second)
	}
	if second.lastSuccessTime != first.lastSuccessTime {
		t.Errorf("last_success_time moved on a FAILED refresh (%q -> %q) — it must stay pinned to the last actual success",
			first.lastSuccessTime, second.lastSuccessTime)
	}
	gotAfterFailure := queryLabelCardinalities(ctx, t, conn, db, catalogTable)
	if !mapEq(gotAfterFailure, wantAfterFirst) {
		t.Fatalf("catalog after a FAILED refresh changed — the atomic-swap contract is violated: got %+v, want the untouched step-2 snapshot %+v",
			gotAfterFailure, wantAfterFirst)
	}

	// Step 4: restore the source (still carrying its original 3 rows — a
	// RENAME never touched the data) and insert one row under a BRAND NEW
	// label key, then force a successful refresh. The 24h window
	// re-aggregates the WHOLE window on every refresh (not just what
	// changed since the last one), so job/env's cardinalities are
	// unchanged from step 2 — the new row repeats existing values for
	// both — while the brand new "region" key appearing at all is the
	// proof this refresh picked up live data rather than staying wedged
	// on the step-3 failure.
	if err := conn.Exec(ctx, fmt.Sprintf("RENAME TABLE %s.otel_logs_broken TO %s.otel_logs", db, db)); err != nil {
		t.Fatalf("restore otel_logs: %v", err)
	}
	insertLogRow(ctx, t, conn, db, time.Now().UTC(), map[string]string{"region": "us-east"})
	sleepPastSecondBoundary()
	if err := conn.Exec(ctx, fmt.Sprintf("SYSTEM REFRESH VIEW %s.%s", db, catalogView)); err != nil {
		t.Fatalf("SYSTEM REFRESH VIEW after restoring the source: %v", err)
	}
	third := waitForRefreshPast(ctx, t, conn, db, catalogView, second.lastRefreshTime, 60*time.Second)
	if !third.succeeded() {
		t.Fatalf("expected the third refresh (source restored) to succeed, got: %+v", third)
	}
	wantAfterThird := map[string]uint64{"job": 2, "env": 2, "region": 1}
	gotAfterThird := queryLabelCardinalities(ctx, t, conn, db, catalogTable)
	if !mapEq(gotAfterThird, wantAfterThird) {
		t.Fatalf("catalog after the recovered refresh = %+v, want %+v — the view must have picked back up, not stayed wedged on the failure",
			gotAfterThird, wantAfterThird)
	}
}

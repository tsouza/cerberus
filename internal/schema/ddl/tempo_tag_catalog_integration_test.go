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

// queryTagCatalogValues reads the catalog table the same shape
// internal/api/tempo's read path does — SELECT TagKey, topKMerge(50)(...)
// GROUP BY Scope, TagKey, for one Scope — returned as a map keyed by
// TagKey for easy comparison.
func queryTagCatalogValues(ctx context.Context, t *testing.T, conn driver.Conn, database, table, scope string) map[string][]string {
	t.Helper()
	rows, err := conn.Query(ctx, fmt.Sprintf(
		"SELECT TagKey, topKMerge(50)(TopValuesState) FROM %s.%s WHERE Scope = ? GROUP BY TagKey", database, table,
	), scope)
	if err != nil {
		t.Fatalf("query tag catalog table: %v", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var (
			key    string
			values []string
		)
		if err := rows.Scan(&key, &values); err != nil {
			t.Fatalf("scan tag catalog row: %v", err)
		}
		out[key] = values
	}
	return out
}

// insertTraceRow inserts one otel_traces row carrying only Timestamp +
// ResourceAttributes + SpanAttributes — every other column takes its
// schema default, mirroring insertLogRow's own minimal-column approach.
func insertTraceRow(ctx context.Context, t *testing.T, conn driver.Conn, database string, ts time.Time, resAttrs, spanAttrs map[string]string) {
	t.Helper()
	stmt := fmt.Sprintf("INSERT INTO %s.otel_traces (Timestamp, ResourceAttributes, SpanAttributes) VALUES (?, ?, ?)", database)
	if err := conn.Exec(ctx, stmt, ts, resAttrs, spanAttrs); err != nil {
		t.Fatalf("insert otel_traces row: %v", err)
	}
}

// keySetEq compares two map[string][]string by KEY SET only (the values
// are an approximate topK sample, not asserted byte-for-byte).
func keySetEq(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// TestTempoTagCatalog_RefreshAndFailureMode is the load-bearing proof for
// cerberus issue #2771's central claim, the Tempo sibling of
// TestLokiLabelCatalog_RefreshAndFailureMode: a refreshable materialized
// view's atomic target swap means a FAILED scheduled refresh keeps serving
// the PREVIOUS successful snapshot, not a partial or empty result —
// verified against a real ClickHouse server. See that test's doc comment
// for the full sequence description; this one repeats it against
// otel_traces / tempo_tag_catalog instead of otel_logs / loki_label_catalog.
func TestTempoTagCatalog_RefreshAndFailureMode(t *testing.T) {
	conn, db := startClickHouse(t)
	ctx := context.Background()

	const (
		catalogTable = "tempo_tag_catalog"
		catalogView  = "tempo_tag_catalog_mv"
	)

	// Step 1: base traces table only, then seed, then enable the catalog.
	baseCfg := ddl.Config{Database: db}
	if err := ddl.ApplyWithConfig(ctx, conn, baseCfg, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("Apply (base traces table): %v", err)
	}

	now := time.Now().UTC()
	insertTraceRow(ctx, t, conn, db, now, map[string]string{"service.name": "checkout"}, map[string]string{"http.method": "GET"})
	insertTraceRow(ctx, t, conn, db, now, map[string]string{"service.name": "cart"}, map[string]string{"http.method": "POST"})
	insertTraceRow(ctx, t, conn, db, now, map[string]string{"service.name": "checkout"}, map[string]string{"http.status_code": "200"})

	catalogCfg := ddl.Config{Database: db, TempoTagCatalogEnabled: true}
	if err := ddl.ApplyWithConfig(ctx, conn, catalogCfg, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("Apply (enable catalog): %v", err)
	}

	// Step 2: wait for the first refresh and check the catalog.
	first := waitForRefreshPast(ctx, t, conn, db, catalogView, "", 60*time.Second)
	if !first.succeeded() {
		t.Fatalf("first refresh did not succeed: %+v", first)
	}
	wantResourceAfterFirst := map[string][]string{"service.name": nil}
	wantSpanAfterFirst := map[string][]string{"http.method": nil, "http.status_code": nil}
	gotResourceAfterFirst := queryTagCatalogValues(ctx, t, conn, db, catalogTable, "resource")
	gotSpanAfterFirst := queryTagCatalogValues(ctx, t, conn, db, catalogTable, "span")
	if !keySetEq(gotResourceAfterFirst, wantResourceAfterFirst) {
		t.Fatalf("resource catalog after first refresh = %+v, want keys %+v", gotResourceAfterFirst, wantResourceAfterFirst)
	}
	if !keySetEq(gotSpanAfterFirst, wantSpanAfterFirst) {
		t.Fatalf("span catalog after first refresh = %+v, want keys %+v", gotSpanAfterFirst, wantSpanAfterFirst)
	}
	if got := gotResourceAfterFirst["service.name"]; len(got) != 2 {
		t.Errorf("service.name top values = %v, want 2 distinct values (checkout, cart)", got)
	}

	// Step 3: break the source and force a failing refresh.
	if err := conn.Exec(ctx, fmt.Sprintf("RENAME TABLE %s.otel_traces TO %s.otel_traces_broken", db, db)); err != nil {
		t.Fatalf("rename otel_traces away: %v", err)
	}
	sleepPastSecondBoundary()
	if err := conn.Exec(ctx, fmt.Sprintf("SYSTEM REFRESH VIEW %s.%s", db, catalogView)); err != nil {
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
	gotResourceAfterFailure := queryTagCatalogValues(ctx, t, conn, db, catalogTable, "resource")
	if !keySetEq(gotResourceAfterFailure, wantResourceAfterFirst) {
		t.Fatalf("resource catalog after a FAILED refresh changed — the atomic-swap contract is violated: got %+v, want the untouched step-2 snapshot %+v",
			gotResourceAfterFailure, wantResourceAfterFirst)
	}

	// Step 4: restore the source with a brand-new resource-scope key, force
	// a successful refresh, confirm it picked back up.
	if err := conn.Exec(ctx, fmt.Sprintf("RENAME TABLE %s.otel_traces_broken TO %s.otel_traces", db, db)); err != nil {
		t.Fatalf("restore otel_traces: %v", err)
	}
	insertTraceRow(ctx, t, conn, db, time.Now().UTC(), map[string]string{"k8s.namespace.name": "prod"}, nil)
	sleepPastSecondBoundary()
	if err := conn.Exec(ctx, fmt.Sprintf("SYSTEM REFRESH VIEW %s.%s", db, catalogView)); err != nil {
		t.Fatalf("SYSTEM REFRESH VIEW after restoring the source: %v", err)
	}
	third := waitForRefreshPast(ctx, t, conn, db, catalogView, second.lastRefreshTime, 60*time.Second)
	if !third.succeeded() {
		t.Fatalf("expected the third refresh (source restored) to succeed, got: %+v", third)
	}
	gotResourceAfterThird := queryTagCatalogValues(ctx, t, conn, db, catalogTable, "resource")
	if _, ok := gotResourceAfterThird["k8s.namespace.name"]; !ok {
		t.Fatalf("catalog after the recovered refresh = %+v, want it to carry the new k8s.namespace.name key — the view must have picked back up, not stayed wedged on the failure",
			gotResourceAfterThird)
	}
}

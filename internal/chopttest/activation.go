//go:build integration

package chopttest

import (
	"context"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// AssertNativeFunctionFired fails t unless the ClickHouse SQL text
// system.query_log recorded for queryID's most recent QueryFinish row
// contains wantFn — the native ts_grid_* function name (e.g.
// "timeSeriesRateToGrid") a Native*Lowerer emits. Reading the SQL text back
// out of the server's own query_log is the only way to prove a native
// lowerer's branch genuinely fired for a given request rather than falling
// back to the fan-out path: an HTTP 200 alone cannot distinguish the two
// (see the package doc and issue #2487's "hollow green" precedent).
//
// The caller is responsible for stamping queryID onto the request context
// (chclient.WithQueryID) before dispatching it, and for that context
// reaching the ClickHouse driver call in-process (an httptest.NewRecorder +
// ServeMux.ServeHTTP call, not a real network round trip through
// httptest.Server, which loses the stamped context at the transport
// boundary).
func AssertNativeFunctionFired(ctx context.Context, t testing.TB, conn driver.Conn, queryID, wantFn string) {
	t.Helper()
	text := queryTextForID(ctx, t, conn, queryID)
	if !strings.Contains(text, wantFn) {
		t.Errorf("query_id %s: emitted SQL does not contain %q — the native lowerer did not fire "+
			"(fell back to fan-out); emitted SQL:\n%s", queryID, wantFn, text)
	}
}

// queryTextForID flushes system.query_log and returns the query text of
// queryID's most recent QueryFinish row. Fatal (rather than a soft failure)
// when no such row exists — a caller's activation assertion would otherwise
// silently compare against an empty string and always fail with a confusing
// message, or worse, against a stale row from an unrelated earlier query
// sharing the same id.
func queryTextForID(ctx context.Context, t testing.TB, conn driver.Conn, queryID string) string {
	t.Helper()
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("chopttest: flush logs: %v", err)
	}
	var text string
	err := conn.QueryRow(
		ctx,
		"SELECT query FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ? "+
			"ORDER BY event_time_microseconds DESC LIMIT 1",
		queryID,
	).Scan(&text)
	if err != nil {
		t.Fatalf("chopttest: no QueryFinish row in system.query_log for query_id %s: %v", queryID, err)
	}
	return text
}

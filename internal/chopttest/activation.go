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

// AssertQuerySettingStamped fails t unless the ClickHouse settings map
// system.query_log recorded for queryID's most recent QueryFinish row carries
// setting with the value want.
//
// This is the SettingsRules-axis sibling of AssertNativeFunctionFired, and it
// exists for the same "hollow green" reason: a per-query setting is invisible
// in the emitted SQL and invisible in the HTTP status, so nothing about a
// passing request distinguishes "the rule fired" from "the rule was never
// wired at all". system.query_log's Settings column records only the settings
// the client actually CHANGED for that query, so a present key with the
// expected value is direct evidence the stamp reached the server, and a
// missing one (the empty string a Map(String, String) yields for an absent
// key) is direct evidence it did not.
//
// The caller is responsible for stamping queryID onto the request context
// (chclient.WithQueryID) before dispatching it — see AssertNativeFunctionFired
// for the full contract, which is identical.
func AssertQuerySettingStamped(ctx context.Context, t testing.TB, conn driver.Conn, queryID, setting, want string) {
	t.Helper()
	got, present := querySettingForID(ctx, t, conn, queryID, setting)
	if !present {
		t.Errorf("query_id %s: ClickHouse setting %q was NOT stamped on the dispatched query "+
			"(system.query_log.Settings carries no such key) — the rule that stamps it did not fire; want %q",
			queryID, setting, want)
		return
	}
	if got != want {
		t.Errorf("query_id %s: ClickHouse setting %q was stamped as %q, want %q",
			queryID, setting, got, want)
	}
}

// querySettingForID flushes system.query_log and returns queryID's most recent
// QueryFinish row's value for setting, plus whether the key was present at
// all. Fatal when no such row exists, for the same reason queryTextForID is:
// an absent row would otherwise read as an absent SETTING, conflating "the
// query never ran" with "the rule never fired".
func querySettingForID(ctx context.Context, t testing.TB, conn driver.Conn, queryID, setting string) (string, bool) {
	t.Helper()
	if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
		t.Fatalf("chopttest: flush logs: %v", err)
	}
	var (
		value   string
		present bool
	)
	err := conn.QueryRow(
		ctx,
		"SELECT Settings[?] AS value, mapContains(Settings, ?) AS present FROM system.query_log "+
			"WHERE type = 'QueryFinish' AND query_id = ? ORDER BY event_time_microseconds DESC LIMIT 1",
		setting, setting, queryID,
	).Scan(&value, &present)
	if err != nil {
		t.Fatalf("chopttest: no QueryFinish row in system.query_log for query_id %s: %v", queryID, err)
	}
	return value, present
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

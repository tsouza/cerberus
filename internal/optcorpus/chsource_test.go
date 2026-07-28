package optcorpus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

func TestQueryLogWindow_DerivesFromInterval(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"short interval clamps to 1h floor", time.Minute, time.Hour},
		{"30m interval -> 1h (2x == floor)", 30 * time.Minute, time.Hour},
		{"45m interval -> 1h30m (2x over floor)", 45 * time.Minute, 90 * time.Minute},
		{"2h interval -> 4h", 2 * time.Hour, 4 * time.Hour},
		{"non-positive interval -> floor", 0, time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := QueryLogWindow(tc.interval); got != tc.want {
				t.Errorf("QueryLogWindow(%s) = %s; want %s", tc.interval, got, tc.want)
			}
		})
	}
}

// capturingConn is a fake CHConn that records the args of the single corpus
// SELECT and then fails the query (so the test exercises only arg threading,
// not a full driver.Rows decode).
type capturingConn struct {
	gotArgs []any
}

func (c *capturingConn) Query(_ context.Context, _ string, args ...any) (driver.Rows, error) {
	c.gotArgs = args
	return nil, errors.New("stub: no rows")
}

func TestCHQueryLogSource_PassesWindowSecondsThenIDs(t *testing.T) {
	conn := &capturingConn{}
	// 90m window -> 5400 seconds bound first, then the id slice.
	src := NewCHQueryLogSource(conn, time.Second, 90*time.Minute)
	_, err := src.FinishedByQueryID(context.Background(), []string{"q1", "q2"})
	if err == nil {
		t.Fatal("FinishedByQueryID: want stub error, got nil")
	}
	if len(conn.gotArgs) != 2 {
		t.Fatalf("query args = %d; want 2 (window seconds, ids)", len(conn.gotArgs))
	}
	if got, ok := conn.gotArgs[0].(int64); !ok || got != 5400 {
		t.Errorf("first arg = %v; want int64(5400) window seconds", conn.gotArgs[0])
	}
	ids, ok := conn.gotArgs[1].([]string)
	if !ok || len(ids) != 2 || ids[0] != "q1" {
		t.Errorf("second arg = %v; want []string{q1,q2}", conn.gotArgs[1])
	}
}

// TestQueryLogQuery_Shape pins the reconciler's system.query_log SELECT.
//
// Two properties in it are load-bearing and invisible to any behavioural test
// that runs without a server:
//
//   - Every enum member is named through CAST(<name>, toTypeName(type)) rather
//     than as a bare string. A bare string in an IN-list makes ClickHouse
//     coerce the comparison to String, so a member name that does not exist
//     matches nothing SILENTLY; the CAST form raises UNKNOWN_ELEMENT_OF_ENUM
//     instead. Naming a member is therefore fail-loud by construction.
//   - The terminal reduction is max(type != QueryFinish), not a reduction over
//     the member NAMES. A distributed query emits one row per node for the same
//     query_id, and 'QueryFinish' sorts after 'ExceptionWhileProcessing', so a
//     name-ordered reduction would reclassify a failed query as a clean one.
func TestQueryLogQuery_Shape(t *testing.T) {
	t.Parallel()

	sql, args := queryLogQuery(5400, []string{"a", "b"})
	want := "SELECT query_id, " +
		"any(normalized_query_hash) AS `normalized_query_hash`, " +
		"max(read_rows) AS `read_rows`, " +
		"max(read_bytes) AS `read_bytes`, " +
		"max(query_duration_ms) AS `query_duration_ms`, " +
		"max(memory_usage) AS `memory_usage`, " +
		"sum(ProfileEvents['QueryConditionCacheHits']) AS `condition_cache_hits`, " +
		"sum(ProfileEvents['RowsReadByPrewhereReaders']) AS `prewhere_rows`, " +
		"toUInt8(max(type != CAST('QueryFinish', toTypeName(type)))) AS `terminal_failed`, " +
		"max(exception_code) AS `exception_code` " +
		"FROM `system`.`query_log` " +
		"WHERE type IN (CAST('QueryFinish', toTypeName(type)), " +
		"CAST('ExceptionWhileProcessing', toTypeName(type))) " +
		"AND event_time > now() - toIntervalSecond(?) " +
		"AND query_id IN (?) " +
		"GROUP BY query_id"
	if sql != want {
		t.Errorf("queryLogQuery SQL =\n%s\nwant\n%s", sql, want)
	}
	if len(args) != 2 {
		t.Fatalf("queryLogQuery args = %d; want 2 (window seconds, ids)", len(args))
	}
}

func TestNewCHQueryLogSource_WindowFallback(t *testing.T) {
	src := NewCHQueryLogSource(&capturingConn{}, time.Second, 0)
	if src.window != defaultQueryLogWindow {
		t.Errorf("non-positive window = %s; want default %s", src.window, defaultQueryLogWindow)
	}
}

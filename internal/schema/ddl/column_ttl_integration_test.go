//go:build integration

package ddl_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// materializeTTL forces immediate application of every TTL rule on table —
// table-level AND column-level — via `ALTER TABLE ... MATERIALIZE TTL`,
// deliberately NOT `OPTIMIZE TABLE ... FINAL`. Verified directly against a
// real ClickHouse 25.9 server: `OPTIMIZE ... FINAL` against a table where a
// data-skipping index depends on a column that also carries a column TTL
// (idx_lower_body + a Body TTL is exactly this shape) reliably raises a
// client-visible `Code: 10. NOT_FOUND_COLUMN_IN_BLOCK` error — reproduced
// on a minimal two-column table carrying only that combination, so it is
// not specific to this test's schema, and independent of whether the
// affected partition genuinely has merge work pending (it recurs on a
// retry). The underlying data is unaffected either way (a plain SELECT
// after the OPTIMIZE error still shows correct post-TTL values), but
// MATERIALIZE TTL is both the semantically precise statement (it targets
// exactly the TTL rules) and the one that completes without this error —
// see docs/operations.md's "Materializing a column TTL on existing parts".
func materializeTTL(ctx context.Context, t *testing.T, conn driver.Conn, database, table string) {
	t.Helper()
	if err := conn.Exec(ctx, fmt.Sprintf("ALTER TABLE %s.%s MATERIALIZE TTL", database, table)); err != nil {
		t.Fatalf("ALTER TABLE %s.%s MATERIALIZE TTL: %v", database, table, err)
	}
}

// bodyLengthsByTraceID returns length(Body) keyed by TraceId, for the logs
// table's column-TTL assertions below.
func bodyLengthsByTraceID(ctx context.Context, t *testing.T, conn driver.Conn, database string) map[string]int64 {
	t.Helper()
	rows, err := conn.Query(ctx, fmt.Sprintf(
		"SELECT TraceId, toInt64(length(Body)) FROM %s.otel_logs", database,
	))
	if err != nil {
		t.Fatalf("query Body lengths: %v", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var traceID string
		var n int64
		if err := rows.Scan(&traceID, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[traceID] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

// TestApply_LogsBodyColumnTTL is the end-to-end proof for the FIRST central
// design question cerberus issue #2769 left unresolved: whether
// `MODIFY COLUMN ... TTL` is accepted on Body — which carries the
// idx_lower_body tokenbf_v1 skip index — without dropping or recreating the
// index, and whether the table's `ttl_only_drop_parts=1` setting (baked
// into every auto-created table) blocks it. Neither question is answerable
// against chDB or a stubbed connection; only a real ClickHouse settles it.
func TestApply_LogsBodyColumnTTL(t *testing.T) {
	conn, database := startClickHouse(t)
	ctx := context.Background()

	const bodyTTL = 5 * time.Second
	cfg := ddl.Config{
		Database:  database,
		ColumnTTL: ddl.ColumnTTL{LogsBody: bodyTTL},
	}
	if err := ddl.ApplyWithConfig(ctx, conn, cfg, []ddl.Signal{ddl.Logs}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	createSQL := createTableQuery(ctx, t, conn, database, "otel_logs")
	if !strings.Contains(createSQL, "idx_lower_body") {
		t.Fatalf("idx_lower_body missing from otel_logs after the Body column TTL ALTER — an indexed column's index must survive untouched:\n%s", createSQL)
	}
	if !strings.Contains(createSQL, "TTL toDateTime(Timestamp) + toIntervalSecond(5)") {
		t.Fatalf("Body column TTL clause missing from otel_logs:\n%s", createSQL)
	}
	if !strings.Contains(createSQL, "ttl_only_drop_parts = 1") {
		t.Fatalf("precondition failed: otel_logs lost its ttl_only_drop_parts=1 setting — this test could not tell the two apart:\n%s", createSQL)
	}

	// One row old enough for Body to have expired (Timestamp far in the
	// past) and one row that can never cross the TTL boundary during this
	// test (Timestamp far in the FUTURE) — using a future timestamp for
	// the "fresh" row avoids any wall-clock race between the INSERT and
	// the MATERIALIZE TTL below.
	insert := fmt.Sprintf(`
		INSERT INTO %s.otel_logs (Timestamp, TraceId, SpanId, SeverityNumber, ServiceName, Body)
		VALUES
		  (now() - INTERVAL 1 HOUR, 'trace-old', 'span-old', 9, 'svc', 'expired body text with keyword banana'),
		  (now() + INTERVAL 1 HOUR, 'trace-new', 'span-new', 9, 'svc', 'fresh body text with keyword banana')
	`, database)
	if err := conn.Exec(ctx, insert); err != nil {
		t.Fatalf("insert: %v", err)
	}

	materializeTTL(ctx, t, conn, database, "otel_logs")

	lengths := bodyLengthsByTraceID(ctx, t, conn, database)
	if got := lengths["trace-old"]; got != 0 {
		t.Errorf("expired row's Body length = %d, want 0 (the column TTL should have cleared it)", got)
	}
	if got := lengths["trace-new"]; got == 0 {
		t.Errorf("fresh row's Body length = 0, want > 0 (the column TTL should not have touched it)")
	}

	// Re-materializing the index after the TTL has cleared the expired
	// row's Body must still succeed — the index stays consistent, not
	// stale or corrupted, after a column TTL fires on the column it
	// indexes.
	if err := conn.Exec(ctx, fmt.Sprintf("ALTER TABLE %s.otel_logs MATERIALIZE INDEX idx_lower_body", database)); err != nil {
		t.Fatalf("MATERIALIZE INDEX after TTL expiry: %v", err)
	}

	// A query filtering through idx_lower_body after expiry must return
	// exactly the fresh row: the expired row's Body is now empty, so it
	// correctly drops out of the match — proof that the index itself
	// stays correct, not that cerberus's own read path is parity-safe
	// (that is internal/api/loki's separate HeaderBodyTTLWindow
	// mitigation, covered by its own unit tests).
	var traceID string
	if err := conn.QueryRow(ctx, fmt.Sprintf(
		"SELECT TraceId FROM %s.otel_logs WHERE lower(Body) LIKE '%%banana%%'", database,
	)).Scan(&traceID); err != nil {
		t.Fatalf("query through idx_lower_body after TTL expiry: %v", err)
	}
	if traceID != "trace-new" {
		t.Errorf("query through idx_lower_body after TTL expiry: got TraceId=%q, want trace-new", traceID)
	}
}

// TestApply_TracesEventsLinksColumnTTL is the end-to-end proof for the
// SECOND central design question cerberus issue #2769 left unresolved:
// whether a Nested column's subcolumns (materialized by ClickHouse as
// ordinary Array(...) columns) accept a TTL, and whether
// `ALTER TABLE ... MATERIALIZE TTL` actually clears an expired row's Nested
// subcolumn to its default (empty array) — independently of a fresh row
// sharing the same part — the way it does for an ordinary scalar column.
// ClickHouse's own docs carry only a scalar-column TTL example, so this
// claim is unverified until proven against a real server.
func TestApply_TracesEventsLinksColumnTTL(t *testing.T) {
	conn, database := startClickHouse(t)
	ctx := context.Background()

	const eventsLinksTTL = 5 * time.Second
	cfg := ddl.Config{
		Database:  database,
		ColumnTTL: ddl.ColumnTTL{TracesEventsLinks: eventsLinksTTL},
	}
	if err := ddl.ApplyWithConfig(ctx, conn, cfg, []ddl.Signal{ddl.Traces}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	createSQL := createTableQuery(ctx, t, conn, database, "otel_traces")
	for _, col := range []string{"Events.Attributes", "Links.Attributes"} {
		wantSubstr := fmt.Sprintf("`%s` Array(Map(LowCardinality(String), String))", col)
		if !strings.Contains(createSQL, wantSubstr) {
			t.Fatalf("otel_traces CREATE missing materialized Nested subcolumn %q:\n%s", wantSubstr, createSQL)
		}
	}
	if n := strings.Count(createSQL, "TTL toDateTime(Timestamp) + toIntervalSecond(5)"); n != 2 {
		t.Fatalf("otel_traces CREATE has %d column-TTL clauses reading toIntervalSecond(5), want 2 (Events.Attributes and Links.Attributes):\n%s", n, createSQL)
	}

	// One span old enough for its Events/Links Attributes to have
	// expired, one span that can never cross the TTL boundary during this
	// test (Timestamp far in the FUTURE) — same future-timestamp trick
	// TestApply_LogsBodyColumnTTL uses to avoid a wall-clock race against
	// MATERIALIZE TTL below.
	insert := fmt.Sprintf(`
		INSERT INTO %s.otel_traces
		  (Timestamp, TraceId, SpanId, ServiceName, SpanName, Duration,
		   `+"`Events.Timestamp`, `Events.Name`, `Events.Attributes`,"+`
		   `+"`Links.TraceId`, `Links.SpanId`, `Links.TraceState`, `Links.Attributes`"+`)
		VALUES
		  (now() - INTERVAL 1 HOUR, 'trace-old', 'span-old', 'svc', 'op', 100,
		   [now() - INTERVAL 1 HOUR], ['exception'], [map('msg', 'boom')],
		   ['linked-trace-1'], ['linked-span-1'], ['state1'], [map('k', 'v')]),
		  (now() + INTERVAL 1 HOUR, 'trace-new', 'span-new', 'svc', 'op', 200,
		   [now() + INTERVAL 1 HOUR], ['exception'], [map('msg', 'boom2')],
		   ['linked-trace-2'], ['linked-span-2'], ['state2'], [map('k2', 'v2')])
	`, database)
	if err := conn.Exec(ctx, insert); err != nil {
		t.Fatalf("insert: %v", err)
	}

	materializeTTL(ctx, t, conn, database, "otel_traces")

	rows, err := conn.Query(ctx, fmt.Sprintf(
		"SELECT TraceId, toInt64(length(`Events.Attributes`)), toInt64(length(`Links.Attributes`)), toInt64(length(`Events.Name`)) FROM %s.otel_traces ORDER BY TraceId",
		database,
	))
	if err != nil {
		t.Fatalf("query Nested subcolumn lengths: %v", err)
	}
	defer rows.Close()
	type row struct {
		traceID        string
		eventsAttrsLen int64
		linksAttrsLen  int64
		eventsNameLen  int64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.traceID, &r.eventsAttrsLen, &r.linksAttrsLen, &r.eventsNameLen); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}

	for _, r := range got {
		switch r.traceID {
		case "trace-old":
			if r.eventsAttrsLen != 0 {
				t.Errorf("trace-old Events.Attributes length = %d, want 0 (the column TTL should have cleared it to an empty array)", r.eventsAttrsLen)
			}
			if r.linksAttrsLen != 0 {
				t.Errorf("trace-old Links.Attributes length = %d, want 0 (the column TTL should have cleared it to an empty array)", r.linksAttrsLen)
			}
			// Events.Name carries no TTL (only Events.Attributes /
			// Links.Attributes are TTL'd — see ColumnTTL's doc comment
			// for why the other Nested subcolumns are left at full row
			// retention) — it must survive untouched even on the
			// expired row, proving the TTL is scoped to the exact
			// subcolumn cerberus targets, not the whole Nested block.
			if r.eventsNameLen != 1 {
				t.Errorf("trace-old Events.Name length = %d, want 1 (untouched — no TTL on this subcolumn)", r.eventsNameLen)
			}
		case "trace-new":
			if r.eventsAttrsLen == 0 {
				t.Errorf("trace-new Events.Attributes length = 0, want > 0 (the column TTL should not have touched it)")
			}
			if r.linksAttrsLen == 0 {
				t.Errorf("trace-new Links.Attributes length = 0, want > 0 (the column TTL should not have touched it)")
			}
		default:
			t.Errorf("unexpected TraceId %q", r.traceID)
		}
	}
}

//go:build integration

package chclient_test

import (
	"context"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
)

// chTimeoutExceededCode is ClickHouse's TIMEOUT_EXCEEDED server error code
// (159): the clean, designed-for-purpose abort a query's own
// `max_execution_time` setting raises. This is what a client-configured
// query timeout is SUPPOSED to end in.
const chTimeoutExceededCode = 159

// TestQueryTimeout_ServerSideCleanAbort pins the fix for a production
// incident: ClickHouse logging `NetException: Unknown packet from client` /
// "Client has dropped the connection" for queries that had already run past
// cerberus's own configured query timeout, instead of the clean
// TIMEOUT_EXCEEDED (code 159) abort the `max_execution_time` setting exists
// to produce — meaning ClickHouse kept doing (sometimes multi-GB) work for a
// client that had already walked away, with no protocol-level signal telling
// it to stop.
//
// Root cause: reqctx.ApplyQueryTimeout stamps the SAME budget onto both
// chclient.WithQueryTimeout (ClickHouse's own `max_execution_time`) and a Go
// context.WithTimeout deadline (a backstop watchdog — see its doc comment).
// clickhouse-go/v2's own queryOptions() (context.go) derives ITS OWN
// `max_execution_time` from ctx.Deadline() — `time.Until(deadline) + 5s` —
// and stamps it into the outgoing settings map UNCONDITIONALLY, clobbering
// whatever chclient.querySettings already computed. Because that
// driver-derived cap is, by construction, always LATER than ctx's own Done()
// firing time, ClickHouse's clean server-side abort can never win the race
// against the caller's own Go ctx — every query that ran its full budget
// aborted through the client-ctx cancellation path instead, which is itself
// racy inside the driver (the outer ctx.Done() select races the raw
// socket's own ctx.Deadline()-derived read/write deadline) and, when the
// raw-socket side won, ClickHouse never received a protocol Cancel packet at
// all — it only noticed the dropped TCP connection.
//
// The fix (chclient.hiddenDeadlineContext, wired into queryContext) hides
// ctx's Deadline() from the driver so its auto-override never fires:
// chclient's own `max_execution_time` reaches ClickHouse unmodified and gets
// the first chance to abort cleanly.
//
// This test cannot assert PURELY on client-observed timing (both the clean
// and the broken behaviour make client.Query return in ~budget seconds), so
// it asserts on ClickHouse's OWN account of what happened —
// system.query_log — which is the one place the two behaviours are
// distinguishable: TIMEOUT_EXCEEDED (159) for the fix, versus a NetException
// (ClickHouse error codes in the 236 / "unknown packet" family) for the
// regression.
//
// Gated behind the `integration` build tag (Docker required); the
// strict-scan lane runs it via `just chclient-integration`.
func TestQueryTimeout_ServerSideCleanAbort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), timeoutContainerBudget)
	defer cancel()

	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:25.9-alpine",
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	client, err := chclient.New(chclient.Config{
		Addr:         host + ":" + port.Port(),
		Database:     "otel",
		Username:     "cerberus",
		Password:     "cerberus",
		QueryTimeout: timeoutQueryBudget,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// A separate observer connection reads system.query_log so the assertion
	// is ClickHouse's own account, not driver bookkeeping.
	observer, err := chclient.New(chclient.Config{
		Addr:     host + ":" + port.Port(),
		Database: "otel",
		Username: "cerberus",
		Password: "cerberus",
	})
	if err != nil {
		t.Fatalf("connect observer: %v", err)
	}
	t.Cleanup(func() { _ = observer.Close() })

	queryID := "chclient-timeout-clean-abort-probe"
	// A per-row sleep with a small block size, so the query runs far longer
	// than timeoutQueryBudget without ever hitting ClickHouse's OWN
	// per-block sleep guard (3s) or exhausting inside a single block.
	const sql = "SELECT count() FROM numbers(100000) WHERE sleepEachRow(0.001) = 0"

	// Mirrors reqctx.ApplyQueryTimeout's actual production wiring exactly:
	// the SAME budget on both the CH-side setting and the Go ctx deadline.
	qctx, qcancel := context.WithTimeout(ctx, timeoutQueryBudget)
	defer qcancel()
	qctx = chclient.WithQueryTimeout(qctx, timeoutQueryBudget)
	qctx = chclient.WithQueryID(qctx, queryID)
	qctx = chclient.WithMaxBlockSize(qctx, timeoutQueryBlockRows)

	if _, err := client.Query(qctx, sql); err == nil {
		t.Fatalf("client.Query: expected a timeout error, got nil")
	}

	typ, code, exc := waitForQueryLogOutcome(ctx, t, observer, queryID)
	t.Logf("system.query_log: type=%s exception_code=%d exception=%q", typ, code, exc)

	if code != chTimeoutExceededCode {
		t.Fatalf(
			"system.query_log exception_code = %d (%q); want %d (TIMEOUT_EXCEEDED) — "+
				"ClickHouse did not abort the query itself, meaning it kept working "+
				"past cerberus's own client timeout instead of receiving a clean "+
				"server-side cap",
			code, exc, chTimeoutExceededCode,
		)
	}
}

// timeoutContainerBudget covers a cold image pull plus server startup plus
// the probe's own run time.
const timeoutContainerBudget = 5 * time.Minute

// timeoutQueryBudget is the query timeout this test configures on both the
// ClickHouse-side max_execution_time setting and the Go ctx watchdog,
// mirroring reqctx.ApplyQueryTimeout's real production wiring. Short enough
// to keep the test fast, long enough that ClickHouse's own abort has to win
// a genuine race rather than a trivially-fast one.
const timeoutQueryBudget = 2 * time.Second

// timeoutQueryBlockRows caps the probe's block size so each block's
// sleepEachRow total (rows * 1ms) stays comfortably under ClickHouse's own
// per-block sleep ceiling (3s) while still taking a fraction of a second —
// long enough that the query's overall runtime is dominated by many blocks,
// not swallowed by a single one finishing before the timeout ever matters.
const timeoutQueryBlockRows = 1000

// queryLogPollBudget bounds how long this test waits for system.query_log to
// carry the probe's outcome — the table is flushed asynchronously (SYSTEM
// FLUSH LOGS nudges it, but does not guarantee immediacy under load).
const queryLogPollBudget = 15 * time.Second

// queryLogPollInterval is the gap between poll attempts while waiting for
// system.query_log to settle.
const queryLogPollInterval = 200 * time.Millisecond

// waitForQueryLogOutcome polls system.query_log for the terminal row
// (type != 'QueryStart') matching queryID, issuing SYSTEM FLUSH LOGS on each
// attempt so the async flush is nudged rather than relied on to race the
// poll. Fails the test if no terminal row appears within queryLogPollBudget.
func waitForQueryLogOutcome(
	ctx context.Context, t *testing.T, observer *chclient.Client, queryID string,
) (typ string, code int32, exception string) {
	t.Helper()

	deadline := time.Now().Add(queryLogPollBudget)
	for {
		if err := observer.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
			t.Logf("SYSTEM FLUSH LOGS: %v", err)
		}
		row := observer.Conn().QueryRow(ctx, `
			SELECT type, exception_code, exception
			FROM system.query_log
			WHERE query_id = ? AND type != 'QueryStart'
			ORDER BY event_time DESC
			LIMIT 1
		`, queryID)
		if err := row.Scan(&typ, &code, &exception); err == nil {
			return typ, code, exception
		}
		if time.Now().After(deadline) {
			t.Fatalf("system.query_log: no terminal row for query_id %q within %s", queryID, queryLogPollBudget)
		}
		time.Sleep(queryLogPollInterval)
	}
}

//go:build integration

package chclient_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestCursorTeardown_ReturnsConnectionToPool is the only place cerberus can
// observe the fact the whole teardown contract rests on: whether a pooled
// ClickHouse connection SURVIVES a cursor teardown.
//
// Nothing below a real server can answer that. chDB has no connection pool at
// all, and a fake cursor cannot model clickhouse-go's two terminal branches —
// drain-to-EndOfStream on a live context releases the connection, while a
// cancel leaves undrained bytes on the wire and the driver destroys the socket
// outright. The difference is a pool statistic and a live TCP session, both of
// which exist only when a real driver is talking to a real server.
//
// The two arms are identical apart from ordering, which is the point:
//
//   - close-then-cancel (chclient.CloseCursor) — the connection returns to the
//     idle pool, the server keeps its session, and the next query reuses it.
//   - cancel-then-close — the socket is destroyed, the server loses the
//     session, and the next query pays for a fresh dial.
//
// Each arm is asserted from BOTH ends: cerberus's own pool statistic and the
// server's TCP session census, read over an independent observer client. The
// client-side view alone is driver bookkeeping; the server-side view makes
// "the socket was destroyed" a fact rather than an inference.
//
// Gated behind the `integration` build tag (Docker required); the strict-scan
// lane runs it via `just chclient-integration`.
func TestCursorTeardown_ReturnsConnectionToPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), teardownContainerBudget)
	defer cancel()

	// The probe must stream several blocks so the driver's process goroutine is
	// provably still in flight when the teardown runs. Asserted rather than
	// left to a comment, because the shape is what makes the cancel arm
	// deterministic and a future edit to either constant could silently undo it.
	if blocks := teardownProbeRows / teardownProbeBlockRows; blocks <= driverBlockBufferDepth+1 {
		t.Fatalf("probe streams %d blocks; need more than %d so the driver parks mid-stream",
			blocks, driverBlockBufferDepth+1)
	}

	fx := startTeardownFixture(ctx, t)
	conn := fx.subject.Conn()

	// Warm the pool: one fully-drained query leaves exactly one idle
	// connection, which is the baseline both arms are measured against.
	if _, err := fx.subject.Query(ctx, teardownOneRowSQL); err != nil {
		t.Fatalf("warmup query: %v", err)
	}
	waitForIdle(t, conn, 1, "warmup")
	pooled := serverSessions(ctx, t, fx.observer)

	t.Run("close before cancel keeps the connection pooled", func(t *testing.T) {
		qctx, qcancel := context.WithCancel(ctx)
		cur, err := fx.subject.QueryCursor(qctx, teardownProbeSQL)
		if err != nil {
			qcancel()
			t.Fatalf("QueryCursor: %v", err)
		}
		// Read one row and walk away: the remainder is still on the wire,
		// which is exactly the state that makes the ordering decidable.
		if !cur.Next() {
			qcancel()
			t.Fatalf("cursor yielded no rows: %v", cur.Err())
		}
		if err := chclient.CloseCursor(qctx, cur, qcancel); err != nil {
			t.Fatalf("CloseCursor: %v", err)
		}

		// A connection the driver destroyed can never be idle, so an idle
		// count of 1 is the release.
		waitForIdle(t, conn, 1, "after a close-then-cancel teardown")
		// And the server still holds the session, so the release kept a real
		// socket rather than merely a counted one.
		waitForSessions(ctx, t, fx.observer, pooled, "after a close-then-cancel teardown")

		// Reuse is the payoff: the follow-up query must run on that same
		// socket, which shows up as the session census not moving.
		if _, err := fx.subject.Query(ctx, teardownOneRowSQL); err != nil {
			t.Fatalf("query on the released connection: %v", err)
		}
		waitForIdle(t, conn, 1, "after reusing the released connection")
		waitForSessions(ctx, t, fx.observer, pooled, "after reusing the released connection")
	})

	t.Run("cancel before close destroys the connection", func(t *testing.T) {
		qctx, qcancel := context.WithCancel(ctx)
		cur, err := fx.subject.QueryCursor(qctx, teardownProbeSQL)
		if err != nil {
			qcancel()
			t.Fatalf("QueryCursor: %v", err)
		}
		if !cur.Next() {
			qcancel()
			t.Fatalf("cursor yielded no rows: %v", cur.Err())
		}
		// The inverted ordering CloseCursor exists to prevent.
		qcancel()
		_ = cur.Close()

		// The server dropping a session is the destruction itself, observed at
		// the end that cannot be fooled by driver bookkeeping.
		waitForSessions(ctx, t, fx.observer, pooled-1, "after a cancel-then-close teardown")
		// And the pool STAYS empty. The positive form (wait for idle == 0)
		// would be vacuous — the pool is legitimately empty the instant a
		// connection is checked out, so a poll that returns on the first match
		// cannot tell a destroyed connection from an in-flight one, and would
		// pass just as happily if the release arm's ordering were used here.
		requireIdleStaysEmpty(t, conn, "after a cancel-then-close teardown")

		// The next query therefore pays for a fresh dial — which is the cost
		// the ordering contract exists to avoid, and the reason a cancelling
		// gateway churns connections under steady load.
		if _, err := fx.subject.Query(ctx, teardownOneRowSQL); err != nil {
			t.Fatalf("query after the destroyed connection: %v", err)
		}
		waitForIdle(t, conn, 1, "after redialling")
		waitForSessions(ctx, t, fx.observer, pooled, "after redialling")
	})
}

// teardownProbeColumns is the Sample column shape the cursor decodes
// positionally.
const teardownProbeColumns = `SELECT MetricName, Attributes, TimeUnix, Value FROM otel_metrics_gauge`

// teardownOneRowSQL is the fully-drained query used to warm the pool and to
// prove a connection came back usable.
const teardownOneRowSQL = teardownProbeColumns + ` LIMIT 1`

// serverSessionsSQL reads the server's live native-protocol session count. The
// observer's own connection is part of it, which is why every assertion is
// against a baseline rather than an absolute.
const serverSessionsSQL = `SELECT value FROM system.metrics WHERE metric = 'TCPConnection'`

// teardownProbeSQL is the probe both arms tear down mid-stream. max_block_size
// is pinned so the result arrives in teardownProbeRows/teardownProbeBlockRows
// blocks regardless of the server's own default — it is the block COUNT, not
// the row count, that parks the driver mid-stream and makes the two orderings
// diverge deterministically.
var teardownProbeSQL = fmt.Sprintf(
	`%s SETTINGS max_block_size = %d`,
	teardownProbeColumns, teardownProbeBlockRows,
)

const (
	// teardownContainerBudget covers a cold image pull plus server startup.
	teardownContainerBudget = 5 * time.Minute

	// driverBlockBufferDepth is clickhouse-go's default BlockBufferSize: the
	// depth of the channel its process goroutine feeds decoded blocks into.
	// Once more than depth+1 blocks are in flight the producer parks on a send,
	// so the goroutine has demonstrably not returned and the teardown's outcome
	// is decided by the ordering under test rather than by a race with the
	// stream ending. A single-block result makes that select a coin flip.
	driverBlockBufferDepth = 2

	// teardownProbeRows and teardownProbeBlockRows shape the probe into eight
	// blocks: comfortably past driverBlockBufferDepth, yet small enough that
	// the whole remainder drains in tens of milliseconds on a race-instrumented
	// build — an order of magnitude inside chclient.CursorDrainBudget, so the
	// release arm is never decided by machine speed.
	teardownProbeRows      = 2_000
	teardownProbeBlockRows = 250

	// poolSettleBudget is how long a pool statistic is given to reach its
	// terminal value: the driver's release / destroy runs on its own process
	// goroutine, so Stats() is eventually consistent with a teardown. It is
	// also the dwell time over which the destroy arm proves the pool STAYS
	// empty.
	poolSettleBudget = 5 * time.Second

	// poolPollInterval is the gap between reads while settling.
	poolPollInterval = 10 * time.Millisecond
)

// teardownFixture pairs the client under test with an independent observer.
// They must not share a pool: the subject is pinned to a single connection so
// one destroyed connection is the whole pool and cannot hide behind a sibling,
// while the observer needs a connection of its own to ask the server what it
// still holds.
type teardownFixture struct {
	subject  *chclient.Client
	observer *chclient.Client
}

// startTeardownFixture brings up a ClickHouse container, seeds the probe table,
// and connects the subject + observer clients to it.
func startTeardownFixture(ctx context.Context, t *testing.T) teardownFixture {
	t.Helper()

	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:25.8-alpine",
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
	addr := host + ":" + port.Port()

	fx := teardownFixture{
		subject:  newTeardownClient(t, addr, 1),
		observer: newTeardownClient(t, addr, teardownObserverConns),
	}

	if err := fx.subject.Exec(ctx, `
		CREATE TABLE otel_metrics_gauge (
			MetricName String,
			Attributes Map(String, String),
			TimeUnix DateTime64(9),
			Value Float64
		) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix)
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := fx.subject.Exec(ctx, `
		INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value)
		SELECT 'up', map('job', 'api'), toDateTime64(1717995600 + number, 9), toFloat64(number)
		FROM numbers(?)
	`, teardownProbeRows); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	// Warm the observer so its own connection is already part of every census
	// it reports, making the counts comparable across observations.
	if _, err := fx.observer.Query(ctx, teardownOneRowSQL); err != nil {
		t.Fatalf("warm observer: %v", err)
	}
	return fx
}

// teardownObserverConns pins the observer to one connection too, so its own
// contribution to the session census is a constant the baseline absorbs.
const teardownObserverConns = 1

// newTeardownClient connects a client with its pool pinned to conns.
func newTeardownClient(t *testing.T, addr string, conns int) *chclient.Client {
	t.Helper()

	client, err := chclient.New(chclient.Config{
		Addr:         addr,
		Database:     "otel",
		Username:     "cerberus",
		Password:     "cerberus",
		MaxOpenConns: conns,
		MaxIdleConns: conns,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// serverSessions reads the server's live native-protocol session count over the
// observer client.
func serverSessions(ctx context.Context, t *testing.T, observer *chclient.Client) int64 {
	t.Helper()

	var n int64
	if err := observer.Conn().QueryRow(ctx, serverSessionsSQL).Scan(&n); err != nil {
		t.Fatalf("read server sessions: %v", err)
	}
	return n
}

// waitForSessions blocks until the server reports want live sessions, failing
// with the last observed value if it never settles there.
func waitForSessions(
	ctx context.Context, t *testing.T, observer *chclient.Client, want int64, stage string,
) {
	t.Helper()

	deadline := time.Now().Add(poolSettleBudget)
	var got int64
	for {
		got = serverSessions(ctx, t, observer)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(poolPollInterval)
	}
	t.Fatalf("server holds %d native sessions %s; want %d", got, stage, want)
}

// waitForIdle blocks until the driver reports want idle connections, failing
// the test with the last observed value if the pool never settles there.
func waitForIdle(t *testing.T, conn driver.Conn, want int, stage string) {
	t.Helper()

	deadline := time.Now().Add(poolSettleBudget)
	var got int
	for {
		got = conn.Stats().Idle
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(poolPollInterval)
	}
	t.Fatalf("Stats().Idle = %d %s; want %d", got, stage, want)
}

// requireIdleStaysEmpty holds the destroy arm's assertion open for the whole
// settle budget. Idle is 0 the moment a connection is checked out, so only the
// DWELL distinguishes a destroyed socket from an in-flight one.
func requireIdleStaysEmpty(t *testing.T, conn driver.Conn, stage string) {
	t.Helper()

	deadline := time.Now().Add(poolSettleBudget)
	for time.Now().Before(deadline) {
		if got := conn.Stats().Idle; got != 0 {
			t.Fatalf("Stats().Idle = %d %s; want the pool to stay empty because the socket was destroyed",
				got, stage)
		}
		time.Sleep(poolPollInterval)
	}
}

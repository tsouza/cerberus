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
// The arms do NOT share a probe, and that is load-bearing rather than
// incidental — see teardownDrainableProbeSQL / teardownStalledProbeSQL. Each
// ordering is only decidable when the driver has exactly one terminal branch
// available to it, and the two orderings need opposite stream shapes to get
// there: one needs a remainder that drains inside the budget, the other needs a
// remainder the server has not produced yet.
//
// Gated behind the `integration` build tag (Docker required); the strict-scan
// lane runs it via `just chclient-integration`.
func TestCursorTeardown_ReturnsConnectionToPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), teardownContainerBudget)
	defer cancel()

	// The probe must stream several blocks so the driver's process goroutine is
	// provably still in flight when the teardown runs. Asserted rather than
	// left to a comment, because the shape is what makes the release arm
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
		cur, err := fx.subject.QueryCursor(qctx, teardownDrainableProbeSQL)
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
		// The STALLED probe. With the drainable one this arm asserts the
		// outcome of a race inside clickhouse-go rather than the ordering:
		// Close() drains, the drain unparks the driver's producer, and if the
		// producer reaches EndOfStream before connect.cancel() closes the
		// socket, `process` returns nil, `release(conn, nil)` pools the
		// connection, and the session census never moves.
		opened := time.Now()
		cur, err := fx.subject.QueryCursor(qctx, teardownStalledProbeSQL)
		if err != nil {
			qcancel()
			t.Fatalf("QueryCursor: %v", err)
		}
		if !cur.Next() {
			qcancel()
			t.Fatalf("cursor yielded no rows: %v", cur.Err())
		}
		// The stall is this arm's PREMISE, so it is asserted rather than
		// assumed. Reaching the first row costs the server one block's worth of
		// per-row sleeps; if a future ClickHouse folds that away, the remainder
		// is buffered client-side again and the assertion below silently decays
		// into a coin flip. Failing here says so out loud instead.
		if waited := time.Since(opened); waited < teardownStallFloor {
			qcancel()
			_ = cur.Close()
			t.Fatalf("first row arrived after %s; want at least %s — the probe's per-row server-side stall is not in effect, so the remainder can be buffered client-side and the cancel would race the drain",
				waited, teardownStallFloor)
		}

		// The inverted ordering CloseCursor exists to prevent. The remainder is
		// still inside the server, so the drain in Close() has nothing to reach
		// EndOfStream with: the driver's only terminal branch is the cancel,
		// which destroys the socket.
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

// teardownDrainableProbeSQL is the RELEASE arm's probe. max_block_size is
// pinned so the result arrives in teardownProbeRows/teardownProbeBlockRows
// blocks regardless of the server's own default: more than the driver's block
// buffer holds, so the producer is parked mid-stream when the teardown starts
// and the drain is a real one — yet small enough that the whole remainder
// arrives well inside chclient.CursorDrainBudget, so the release is never
// decided by machine speed.
var teardownDrainableProbeSQL = fmt.Sprintf(
	`%s SETTINGS max_block_size = %d`,
	teardownProbeColumns, teardownProbeBlockRows,
)

// teardownStalledProbeSQL is the DESTROY arm's probe: the same shape, held back
// by a per-row server-side sleep.
//
// The stall is what makes that arm an assertion instead of a coin flip.
// clickhouse-go's `connect.process` selects over three ready-able cases —
// ctx.Done, the reader's error, the reader's completion — and `rows.Close()`
// DRAINS, which is precisely what lets the reader complete. Hand the destroy
// arm a probe whose remainder is already in the client's socket buffer and the
// drain can reach EndOfStream before the cancellation is observed; `process`
// then returns nil, `release(conn, nil)` returns the connection to the idle
// pool, and the server keeps the session the arm is trying to watch die. Two
// orderings, one outcome, decided by whichever goroutine the scheduler picked.
//
// A remainder the SERVER has not produced yet removes that branch outright:
// there is nothing to drain, the reader is blocked on the wire rather than on
// the block channel, and the cancel is the only way the query can end. It also
// keeps the reader off the block channel while the driver closes it, which is
// the one ordering in which clickhouse-go would close a channel out from under
// a parked sender.
var teardownStalledProbeSQL = fmt.Sprintf(
	`%s WHERE sleepEachRow(%v) = 0 SETTINGS max_block_size = %d`,
	teardownProbeColumns, teardownStallSecondsPerRow, teardownProbeBlockRows,
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
	// release arm is never decided by machine speed. teardownProbeBlockRows
	// pins the probe table's index granularity as well as max_block_size, so
	// the block count is the server's behaviour rather than arithmetic.
	teardownProbeRows      = 2_000
	teardownProbeBlockRows = 250

	// teardownStallSecondsPerRow is the destroy arm's server-side brake: every
	// row the stalled probe emits costs the server this much sleep, so the
	// remainder stays inside ClickHouse instead of being buffered on the
	// client where a drain could reach the end of it.
	teardownStallSecondsPerRow = 0.002

	// teardownStallPerBlock is what one block of the stalled probe therefore
	// costs — the gap the cancellation has to win, against a scheduler wakeup
	// and a socket close.
	teardownStallPerBlock = time.Duration(teardownStallSecondsPerRow*float64(time.Second)) * teardownProbeBlockRows

	// teardownStallFloor is how much of that gap must be OBSERVED for the stall
	// to count as in effect. Half a block absorbs ClickHouse's own sleep
	// granularity while staying two orders of magnitude above the unstalled
	// case, which arrives in single-digit milliseconds.
	teardownStallFloor = teardownStallPerBlock / 2

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

	// index_granularity is pinned to the probe's block size, not left at the
	// default. A MergeTree reader cannot subdivide a granule for the filter
	// stage, so with the default 8192 the whole 2000-row table is ONE block
	// there however max_block_size is set — which would collapse the stalled
	// probe into a single block with no remainder to stall.
	if err := fx.subject.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE otel_metrics_gauge (
			MetricName String,
			Attributes Map(String, String),
			TimeUnix DateTime64(9),
			Value Float64
		) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix)
		SETTINGS index_granularity = %d
	`, teardownProbeBlockRows)); err != nil {
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

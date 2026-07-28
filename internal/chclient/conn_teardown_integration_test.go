//go:build integration

package chclient_test

import (
	"context"
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
// outright. The pool statistic is the difference, and it exists only when a
// real driver is talking to a real server.
//
// The two arms are identical apart from ordering, which is the point:
//
//   - close-then-cancel (chclient.CloseCursor) — the connection returns to the
//     idle pool and the next query reuses it.
//   - cancel-then-close — the connection is destroyed and the next query pays
//     for a fresh dial.
//
// Gated behind the `integration` build tag (Docker required); the strict-scan
// lane runs it via `just chclient-integration`.
func TestCursorTeardown_ReturnsConnectionToPool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), teardownContainerBudget)
	defer cancel()

	client := startTeardownFixture(ctx, t)
	conn := client.Conn()

	// Warm the pool: one fully-drained query leaves exactly one idle
	// connection, which is the baseline both arms are measured against.
	if _, err := client.Query(ctx, teardownProbeSQL+" LIMIT 1"); err != nil {
		t.Fatalf("warmup query: %v", err)
	}
	waitForIdle(t, conn, 1, "warmup")

	t.Run("close before cancel keeps the connection pooled", func(t *testing.T) {
		qctx, qcancel := context.WithCancel(ctx)
		cur, err := client.QueryCursor(qctx, teardownProbeSQL)
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
		// count of 1 is the release. The follow-up query then proves it is a
		// USABLE connection rather than merely a counted one.
		waitForIdle(t, conn, 1, "after a close-then-cancel teardown")
		if _, err := client.Query(ctx, teardownProbeSQL+" LIMIT 1"); err != nil {
			t.Fatalf("query on the released connection: %v", err)
		}
		waitForIdle(t, conn, 1, "after reusing the released connection")
	})

	t.Run("cancel before close destroys the connection", func(t *testing.T) {
		qctx, qcancel := context.WithCancel(ctx)
		cur, err := client.QueryCursor(qctx, teardownProbeSQL)
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

		// The pool is empty because the socket was torn down, not returned.
		waitForIdle(t, conn, 0, "after a cancel-then-close teardown")
		// The next query therefore pays for a fresh dial — which is the cost
		// the ordering contract exists to avoid, and the reason a cancelling
		// gateway churns connections under steady load.
		if _, err := client.Query(ctx, teardownProbeSQL+" LIMIT 1"); err != nil {
			t.Fatalf("query after the destroyed connection: %v", err)
		}
		waitForIdle(t, conn, 1, "after redialling")
	})
}

// teardownProbeSQL selects the Sample column shape the cursor decodes
// positionally, over enough rows that a one-row read leaves the rest of the
// stream unread on the wire.
const teardownProbeSQL = `SELECT MetricName, Attributes, TimeUnix, Value FROM otel_metrics_gauge`

const (
	// teardownContainerBudget covers a cold image pull plus server startup.
	teardownContainerBudget = 5 * time.Minute

	// teardownProbeRows is sized so a one-row read leaves the rest of the
	// result stream unread — the state in which the two teardown orderings
	// diverge — while the remainder still drains well inside
	// chclient.CursorDrainBudget on a race-instrumented build.
	teardownProbeRows = 40_000

	// poolSettleBudget is how long a pool statistic is given to reach its
	// terminal value: the driver's release / destroy runs on its own process
	// goroutine, so Stats() is eventually consistent with a teardown.
	poolSettleBudget = 5 * time.Second

	// poolPollInterval is the gap between Stats() reads while settling.
	poolPollInterval = 10 * time.Millisecond
)

// startTeardownFixture brings up a ClickHouse container, connects a client
// pinned to a single pooled connection (so one destroyed connection is the
// whole pool and cannot hide behind a sibling), and seeds the probe table.
func startTeardownFixture(ctx context.Context, t *testing.T) *chclient.Client {
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

	client, err := chclient.New(chclient.Config{
		Addr:         host + ":" + port.Port(),
		Database:     "otel",
		Username:     "cerberus",
		Password:     "cerberus",
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Exec(ctx, `
		CREATE TABLE otel_metrics_gauge (
			MetricName String,
			Attributes Map(String, String),
			TimeUnix DateTime64(9),
			Value Float64
		) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix)
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := client.Exec(ctx, `
		INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value)
		SELECT 'up', map('job', 'api'), toDateTime64(1717995600 + number, 9), toFloat64(number)
		FROM numbers(?)
	`, teardownProbeRows); err != nil {
		t.Fatalf("seed rows: %v", err)
	}
	return client
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

//go:build integration

package chclient_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestAcquireDataShardFanout_CancelledDispatch_ServerActuallyKillsQuery is the
// real-ClickHouse counterpart to fanout_gate_test.go's
// TestAcquireDataShardFanout_CancelledDispatch_IssuesKillQueryBeforeRelease
// (and its three siblings). Those tests pin the fix's call-shape and ordering
// against execRecordingConn, an in-process fake that only records that
// c.conn.Exec(ctx, killDataShardQuerySQL, queryID) was called — they cannot
// prove ClickHouse itself receives, parses, and honors that KILL QUERY for
// the still-running statement it targets (cerberus issue #3489).
//
// This test drives the production dispatch seam directly — Client.QueryCursor,
// which opens through queryOpen, acquires the SAME c.dataShardFanoutGate via
// acquireDataShardFanout, and releases through gatedRows.Close — against a
// real testcontainers ClickHouse. It cancels the dispatch's ctx while the
// server-side statement is still genuinely running, then asserts over an
// INDEPENDENT observer connection's system.processes that ClickHouse actually
// stopped the query, not merely that cerberus issued the Exec call.
//
// The assertion that actually distinguishes the fix from a broken release()
// is timing, not mere eventual absence: clickhouse-go's own connect.cancel()
// (fanout_gate.go's own "CANCELLATION FIX" doc) sends a ClientCancel packet
// and tears down the local socket on ANY ctx cancellation, SYNC KILL QUERY or
// not, and that alone eventually makes ClickHouse stop the statement too, on
// its own schedule — so a release() that dropped the KILL QUERY call entirely
// would still make the query disappear from system.processes if this test
// merely polled for "eventually gone" for long enough. What KILL QUERY ...
// SYNC specifically buys is a SYNCHRONOUS guarantee: by the time
// killDataShardQuery's Exec call — and so release(), and so cur.Close() —
// returns, ClickHouse has ALREADY confirmed the statement is dead. This test
// therefore checks system.processes with NO poll at all, immediately after
// cur.Close() returns: that is the exact instant SYNC's guarantee applies to,
// and a release() that skips KILL QUERY (or frees the gate weight before it
// completes) leaves the statement listed as running at that instant, which is
// what this assertion catches — verified directly by temporarily disabling
// the KILL QUERY call in acquireDataShardFanout's release() and confirming
// this test fails against a real server (it does; system.processes still
// listed the query immediately after Close(), and only self-resolved several
// seconds later via the plain ClientCancel teardown).
//
// Gated behind the `integration` build tag (Docker required); the
// strict-scan lane runs it via `just chclient-integration`.
func TestAcquireDataShardFanout_CancelledDispatch_ServerActuallyKillsQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), killQueryContainerBudget)
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
	addr := host + ":" + port.Port()

	// DataShardCount > 1 is what allocates c.dataShardFanoutGate at all
	// (NewDataShardFanoutGate's own doc) — the one precondition under which
	// acquireDataShardFanout's release ever issues KILL QUERY. MaxOpenConns
	// sized to the gate's own weight (dataShardCount, the default
	// multiplier) so the single dispatch below admits immediately.
	const dataShardCount = 2
	subject, err := chclient.New(chclient.Config{
		Addr:           addr,
		Database:       "otel",
		Username:       "cerberus",
		Password:       "cerberus",
		DataShardCount: dataShardCount,
		MaxOpenConns:   dataShardCount,
		MaxIdleConns:   dataShardCount,
	})
	if err != nil {
		t.Fatalf("connect subject: %v", err)
	}
	t.Cleanup(func() { _ = subject.Close() })

	// A separate observer connection reads system.processes so the
	// assertion is ClickHouse's own live account of what is running, not
	// driver-side bookkeeping — mirrors conn_teardown_integration_test.go's
	// fixture shape (invariant: subject and observer never share a pool).
	observer, err := chclient.New(chclient.Config{
		Addr:     addr,
		Database: "otel",
		Username: "cerberus",
		Password: "cerberus",
	})
	if err != nil {
		t.Fatalf("connect observer: %v", err)
	}
	t.Cleanup(func() { _ = observer.Close() })

	const queryID = "chclient-cancel-kill-query-probe"
	// A per-row sleep, block-capped like teardownStalledProbeSQL's own probe
	// (conn_teardown_integration_test.go): ClickHouse refuses to run a
	// single block whose total sleepEachRow time exceeds its own
	// max_execution_time-independent per-block sleep ceiling (observed: a
	// query erroring TOO_SLOW / code 160 immediately, rather than ever
	// appearing in system.processes, when max_block_size is left at its
	// default and the per-block sleep total is computed against the whole
	// default block width). killQueryProbeBlockRows caps the block width so
	// each block's own sleep total stays comfortably under that ceiling,
	// while killQueryProbeRows * killQueryProbeSleepSeconds — the STATEMENT's
	// total runtime — sits far above every polling budget below, so a
	// broken release() (no KILL QUERY, or the gate weight freed before it
	// completes) leaves the process listed long past this test's deadlines.
	sql := fmt.Sprintf(
		"SELECT count() FROM numbers(%d) WHERE sleepEachRow(%v) = 0 SETTINGS max_block_size = %d",
		killQueryProbeRows, killQueryProbeSleepSeconds, killQueryProbeBlockRows,
	)

	qctx, qcancel := context.WithCancel(ctx)
	qctx = chclient.WithQueryID(qctx, queryID)

	cur, err := subject.QueryCursor(qctx, sql)
	if err != nil {
		qcancel()
		t.Fatalf("QueryCursor: %v", err)
	}

	// Confirm the premise: the statement is genuinely running server-side
	// before it is ever cancelled. Without this, a QueryCursor call that
	// merely opened without ClickHouse having started the statement yet
	// would make the later "no longer running" assertion vacuous.
	waitForProcessPresent(ctx, t, observer, queryID, killQueryPollBudget)

	// Cancel the dispatch's own ctx BEFORE the cursor is closed — the same
	// ordering fanout_gate_test.go's cancellation tests and
	// conn_teardown_integration_test.go's "cancel before close" arm use:
	// this is what makes acquireDataShardFanout's release() see a non-nil
	// ctx.Err() and take the KILL QUERY branch instead of the plain
	// release.
	qcancel()

	closeDone := make(chan struct{})
	go func() {
		_ = cur.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(killQueryCloseBudget):
		t.Fatal("cur.Close() did not return within the budget — release() appears to have hung")
	}

	// The server-side proof, checked with NO poll — see the test's own doc
	// above for why immediacy (not mere eventual absence) is what actually
	// distinguishes KILL QUERY ... SYNC from the plain local cancellation
	// every dispatch performs regardless.
	var nImmediate uint64
	if err := observer.Conn().QueryRow(ctx, killQueryProcessCountSQL, queryID).Scan(&nImmediate); err != nil {
		t.Fatalf("read system.processes immediately after Close: %v", err)
	}
	if nImmediate != 0 {
		t.Fatalf(
			"system.processes lists %d row(s) for query_id %q immediately after cur.Close() returned — "+
				"KILL QUERY ... SYNC should have already confirmed the statement dead by this point, "+
				"before the gate weight was ever released",
			nImmediate, queryID,
		)
	}

	// Belt-and-braces: it must also STAY absent (not merely observed gone
	// in the one instant checked above, and not a query that reappears via
	// some retry the driver issues).
	waitForProcessAbsent(ctx, t, observer, queryID, killQueryPollBudget)
}

// killQueryContainerBudget covers a cold image pull plus server startup plus
// the probe's own polling budgets.
const killQueryContainerBudget = 5 * time.Minute

// killQueryProbeBlockRows caps the probe's block width so each block's own
// sleepEachRow total stays under ClickHouse's per-block sleep ceiling (see
// the test's own doc: an uncapped block errors TOO_SLOW before the query
// ever appears in system.processes at all).
const killQueryProbeBlockRows = 100

// killQueryProbeSleepSeconds is the probe's per-row server-side sleep.
// killQueryProbeBlockRows * killQueryProbeSleepSeconds (2s) sits under
// ClickHouse's 3s-per-block sleep ceiling.
const killQueryProbeSleepSeconds = 0.02

// killQueryProbeRows is the probe's total row count. killQueryProbeRows *
// killQueryProbeSleepSeconds is the statement's total server-side runtime
// (200s) — far above every polling budget below, so a broken release()
// leaves the process observably running for the whole test rather than
// finishing naturally inside the window this test uses to decide "still
// running" vs. "gone".
const killQueryProbeRows = 10_000

// killQueryPollBudget bounds how long this test waits for system.processes to
// reach the expected state (present, then absent). ClickHouse's own KILL
// QUERY ... SYNC checkpoints against the query's cancellation flag on a
// per-block cadence, so a healthy kill settles in well under a second; this
// budget leaves generous headroom without masking a genuinely hung release.
const killQueryPollBudget = 15 * time.Second

// killQueryPollInterval is the gap between system.processes poll attempts.
const killQueryPollInterval = 100 * time.Millisecond

// killQueryCloseBudget bounds how long cur.Close() (and therefore
// acquireDataShardFanout's release(), including its KILL QUERY ... SYNC
// round-trip bounded by killDataShardQueryTimeout) is given to return.
const killQueryCloseBudget = 10 * time.Second

// killQueryProcessCountSQL reads how many currently-running queries carry the
// given query_id.
const killQueryProcessCountSQL = `SELECT count() FROM system.processes WHERE query_id = ?`

// waitForProcessPresent polls system.processes over observer until queryID
// shows up as a running query, failing the test if it never does within
// budget.
func waitForProcessPresent(
	ctx context.Context, t *testing.T, observer *chclient.Client, queryID string, budget time.Duration,
) {
	t.Helper()

	deadline := time.Now().Add(budget)
	var n uint64
	for {
		if err := observer.Conn().QueryRow(ctx, killQueryProcessCountSQL, queryID).Scan(&n); err != nil {
			t.Fatalf("read system.processes: %v", err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("system.processes never listed query_id %q as running within %s", queryID, budget)
		}
		time.Sleep(killQueryPollInterval)
	}
}

// waitForProcessAbsent polls system.processes over observer until queryID no
// longer shows up as a running query, failing the test with the last
// observed count if it is still listed once budget expires.
func waitForProcessAbsent(
	ctx context.Context, t *testing.T, observer *chclient.Client, queryID string, budget time.Duration,
) {
	t.Helper()

	deadline := time.Now().Add(budget)
	var n uint64
	for {
		if err := observer.Conn().QueryRow(ctx, killQueryProcessCountSQL, queryID).Scan(&n); err != nil {
			t.Fatalf("read system.processes: %v", err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf(
				"system.processes still lists %d row(s) for query_id %q after %s — "+
					"ClickHouse did not actually terminate the cancelled dispatch's statement",
				n, queryID, budget,
			)
		}
		time.Sleep(killQueryPollInterval)
	}
}

package chclient

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// Layer 11 — active background breaker-recovery tests.
//
// These exercise the recovery loop (breaker_recovery.go) in isolation: the
// loop must drive an OPEN breaker back to CLOSED with NO external
// allow()/record() traffic (the traffic-starved-replica fix), must exit
// cleanly on Close (goleak), and must do ZERO CH I/O while every breaker is
// CLOSED (the happy-path no-op).

// recoveryFakeConn is a minimal recoveryPinger whose Ping fails the first
// failFirst calls and then succeeds — modelling a ClickHouse that comes back
// after the breaker has tripped. pingCount records every Ping so a test can
// assert "the loop pinged N times" or "the loop never pinged at all".
type recoveryFakeConn struct {
	failFirst int64
	pingCount atomic.Int64
}

func (c *recoveryFakeConn) Ping(context.Context) error {
	n := c.pingCount.Add(1)
	if n <= c.failFirst {
		return errors.New("recoveryFakeConn: simulated CH outage")
	}
	return nil
}

// newRecoveryTestClient builds a Client over conn whose background recovery
// loop runs at the supplied (short) interval and per-probe budget, so a test
// can observe recovery in bounded wall-clock without waiting the production 5s
// cadence. The per-head breakers are built with the SAME short interval as
// their OPEN-state backoff so a tripped breaker becomes probe-eligible
// immediately — otherwise allow() would gate the loop on the production 5s
// default. It mirrors what New does for the loop while skipping the live CH
// driver. The caller MUST Close the returned Client — that is what stops +
// joins the goroutine the goleak test asserts on.
func newRecoveryTestClient(conn recoveryPinger, interval, pingTimeout time.Duration) *Client {
	return newRecoveryTestClientWithSetup(conn, interval, pingTimeout, nil)
}

// newRecoveryTestClientWithSetup is newRecoveryTestClient plus a hook that runs
// against the fresh default breaker BEFORE the loop goroutine is started. A
// test that needs a breaker PRECONDITION (e.g. already OPEN) must establish it
// here rather than after construction: post-construction tripping races the
// loop's first tick, which can transition the breaker to HALF-OPEN before the
// precondition assertion reads it.
func newRecoveryTestClientWithSetup(
	conn recoveryPinger,
	interval, pingTimeout time.Duration,
	setup func(def *breaker),
) *Client {
	// window=0 keeps the default rolling failure window; openInterval=interval
	// makes the OPEN backoff elapse on the same tiny cadence the loop ticks at.
	def, registry := buildBreakers(false, 0, 0, interval, nil)
	if setup != nil {
		setup(def)
	}
	c := &Client{br: def, breakers: registry, cursorDecoder: rowDecoder{}}
	c.recovery = startRecoveryLoop(conn, breakerList(def, registry), interval, pingTimeout)
	return c
}

// tripBreaker drives br to OPEN by recording threshold consecutive failures —
// the same path a burst of failing requests would take, but invoked directly
// so the test sets up the precondition without any request traffic.
func tripBreaker(br *breaker) {
	failErr := errors.New("trip")
	for i := 0; i < br.resolveThreshold(); i++ {
		br.record(context.Background(), failErr)
	}
}

// TestRecoveryLoop_DrivesOpenToClosed — the background loop alone, with NO
// external allow()/record() calls, must drive a tripped breaker back to
// CLOSED once the fake conn starts answering Ping. This is the
// traffic-starved-replica fix: recovery happens on the loop's schedule, not on
// inbound request traffic.
func TestRecoveryLoop_DrivesOpenToClosed(t *testing.T) {
	t.Parallel()

	const (
		interval     = 5 * time.Millisecond
		pingTimeout  = 100 * time.Millisecond
		failFirstTwo = 2
	)
	conn := &recoveryFakeConn{failFirst: failFirstTwo}
	client := newRecoveryTestClient(conn, interval, pingTimeout)
	t.Cleanup(func() { _ = client.Close() })

	// Trip the default breaker OPEN with no request traffic.
	tripBreaker(client.br)
	require.Equal(t, "open", client.br.currentState(),
		"precondition: breaker must be OPEN before the loop runs")

	// The loop's first probes fail (conn answers an error failFirst times),
	// re-opening the breaker each time; once the conn starts succeeding the
	// next probe closes the circuit. Assert it reaches CLOSED within a
	// bounded wall-clock — the interval is tiny so this resolves fast.
	require.Eventually(t, func() bool {
		return client.br.currentState() == "closed"
	}, 2*time.Second, interval,
		"recovery loop did not drive the breaker OPEN→CLOSED")

	// And the conn was actually pinged more than once (failed probes + the
	// closing one) — proof recovery came from the loop, not from any caller.
	require.Greater(t, conn.pingCount.Load(), int64(1),
		"loop should have fired multiple recovery pings")
}

// TestRecoveryLoop_CloseStopsGoroutine — Close must stop + join the recovery
// goroutine so nothing leaks. goleak.VerifyNone after Close fails if the
// goroutine is still parked on its ticker.
func TestRecoveryLoop_CloseStopsGoroutine(t *testing.T) {
	// Not parallel: goleak inspects the whole-process goroutine set, so it
	// must not race sibling tests' goroutines. IgnoreCurrent snapshots the
	// pre-existing (test-framework / runtime) goroutines so only ours count.
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	const (
		interval    = time.Millisecond
		pingTimeout = 50 * time.Millisecond
	)
	conn := &recoveryFakeConn{}
	client := newRecoveryTestClient(conn, interval, pingTimeout)

	// Let the loop spin at least once so the goroutine is provably live, then
	// Close and assert it is gone.
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, client.Close())

	// Close is idempotent + view-safe: a second Close (e.g. via a ForHead
	// view sharing the handle) must not panic on a re-closed stop channel.
	require.NoError(t, client.Close())
}

// TestRecoveryLoop_ClosedBreakerNoPing — while every breaker is CLOSED the
// loop must do ZERO CH I/O: the peek() gate short-circuits before any Ping, so
// a healthy replica's recovery loop is a pure no-op.
func TestRecoveryLoop_ClosedBreakerNoPing(t *testing.T) {
	t.Parallel()

	const (
		interval    = time.Millisecond
		pingTimeout = 50 * time.Millisecond
	)
	conn := &recoveryFakeConn{}
	client := newRecoveryTestClient(conn, interval, pingTimeout)
	t.Cleanup(func() { _ = client.Close() })

	// All breakers start CLOSED. Give the loop many tick windows to run.
	time.Sleep(50 * time.Millisecond)

	require.Equal(t, int64(0), conn.pingCount.Load(),
		"loop pinged CH while all breakers were CLOSED — peek() gate failed")
}

// TestRecoveryLoop_PingerIsConnSubset — compile-proof that the production
// driver.Conn satisfies the narrow recoveryPinger the loop holds, so New can
// hand its real conn to startRecoveryLoop unchanged.
func TestRecoveryLoop_PingerIsConnSubset(t *testing.T) {
	t.Parallel()
	var _ recoveryPinger = driver.Conn(nil)
}

// blockingPinger is a recoveryPinger whose Ping PARKS until its ctx is done —
// modelling the exact production shape the cancellation fix targets: a
// synthetic recovery ping mid-dial against a dead ClickHouse, holding the loop
// goroutine for the whole per-ping budget. It publishes when the ping has
// entered (entered) and which error the ping ctx finally carried (pingErr).
type blockingPinger struct {
	entered   chan struct{} // closed once the first Ping is parked
	enterOnce sync.Once

	// pingErr is the ctx error the parked Ping observed. It is written by the
	// recovery goroutine BEFORE that goroutine closes its done channel, and
	// read by the test only AFTER Close's join has received from that channel —
	// the join is a happens-before edge, so the unsynchronised read is
	// race-free (and -race clean).
	pingErr error
}

func newBlockingPinger() *blockingPinger {
	return &blockingPinger{entered: make(chan struct{})}
}

func (p *blockingPinger) Ping(ctx context.Context) error {
	p.enterOnce.Do(func() { close(p.entered) })
	<-ctx.Done()
	p.pingErr = ctx.Err()
	return p.pingErr
}

var _ recoveryPinger = (*blockingPinger)(nil)

// TestRecoveryLoop_CloseCancelsInFlightPing — Close must ABORT a recovery ping
// that is already in flight, not wait it out.
//
// The bug: the per-ping ctx used to be rooted at context.Background(), so it
// was reachable only by its own timer. Close signalled the loop and then joined
// the goroutine — but the goroutine was parked inside Ping, so the join (and
// therefore process shutdown) blocked for the full remaining
// recoveryPingTimeout, which is sized to the ClickHouse dial timeout (5s by
// default). Every shutdown that landed mid-probe paid it.
//
// The fix: the loop owns a cancellable root ctx and every per-ping ctx descends
// from it, so stop() cancels BEFORE joining and the parked ping returns
// immediately. This test parks a ping, times Close, and pins all four
// observable consequences.
func TestRecoveryLoop_CloseCancelsInFlightPing(t *testing.T) {
	// Not parallel: it measures wall-clock Close latency, so it must not
	// contend with sibling tests. goleak (registered FIRST, hence run LAST)
	// proves the loop goroutine truly exited rather than being abandoned.
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	const (
		// Tick fast so the blocked ping is in flight within the setup window.
		interval = time.Millisecond
		// The per-ping budget, same order as production's recoveryPingTimeout
		// (== the CH dial timeout, defaultDialTimeout = 5s). Deliberately far
		// larger than closeBudget: a Close that waits this out IS the bug.
		blockedPingTimeout = 5 * time.Second
		// Wall-clock ceiling for Close while a ping is parked. Cancellation is
		// signal-speed (a ctx-done close plus a goroutine wake), so this leaves
		// orders of magnitude of headroom for a loaded CI runner while staying
		// 10x under blockedPingTimeout — it cannot pass if Close waits for the
		// ping's own deadline.
		closeBudget = 500 * time.Millisecond
		// How long to wait for the loop's first tick to reach Ping. Generous
		// against a 1ms interval so a scheduling hiccup can't fail the setup.
		enterBudget = 2 * time.Second
	)

	conn := newBlockingPinger()
	// Trip the default breaker BEFORE the loop goroutine exists, so the
	// precondition is deterministic — the loop's first tick then already has a
	// non-CLOSED breaker to probe, and nothing races the assertion.
	client := newRecoveryTestClientWithSetup(conn, interval, blockedPingTimeout, func(def *breaker) {
		tripBreaker(def)
		require.Equal(t, "open", def.currentState(),
			"precondition: breaker must be OPEN so the loop fires a recovery ping")
	})
	// Safety net: on ANY exit path (including a t.Fatal above the measured
	// Close) the loop is torn down before goleak's deferred check runs.
	// Registered after goleak's defer, so it runs before it.
	defer func() { _ = client.Close() }()

	// Wait until the synthetic ping is provably parked inside Ping — that is
	// the state whose shutdown cost this test bounds.
	select {
	case <-conn.entered:
	case <-time.After(enterBudget):
		t.Fatalf("recovery loop never entered Ping within %s", enterBudget)
	}

	start := time.Now()
	require.NoError(t, client.Close())
	elapsed := time.Since(start)

	// 1. Close did not wait out the per-ping budget.
	require.Less(t, elapsed, closeBudget,
		"Close blocked %s joining an in-flight recovery ping (per-ping budget %s): the ping ctx is not cancelled by Close",
		elapsed, blockedPingTimeout)

	// 2. The ping observed CANCELLATION, not deadline expiry — proof the abort
	//    came from Close's cancel propagating down the ctx tree rather than
	//    from the per-ping timer firing. (Unsynchronised read is safe: Close
	//    joined the goroutine; see blockingPinger.pingErr.)
	require.ErrorIs(t, conn.pingErr, context.Canceled,
		"in-flight ping must observe context.Canceled from Close")
	require.NotErrorIs(t, conn.pingErr, context.DeadlineExceeded,
		"ping ended on its own timeout instead of on Close's cancellation")

	// 3. The goroutine is gone: its done channel is closed. Close's join is
	//    what guarantees this, so an open channel here means the join is not
	//    doing its job (and goleak would flag the survivor too).
	select {
	case <-client.recovery.doneCh:
	default:
		t.Fatal("recovery goroutine did not exit: done channel still open after Close")
	}

	// 4. A shutdown-cancelled probe is NO CH-health verdict. The breaker stays
	//    HALF-OPEN (a failure verdict would have driven it back to OPEN, a
	//    success to CLOSED) and its probe slot is released, so the next allow()
	//    admits a fresh probe instead of stalling forever.
	require.Equal(t, "half-open", client.br.currentState(),
		"a cancelled probe must record no verdict, leaving the breaker HALF-OPEN")
	require.True(t, client.br.allow(),
		"the cancelled probe must release the HALF-OPEN probe slot")

	// Close stays idempotent once the ctx is cancelled and the done channel is
	// closed — the double-Close / shared-ForHead-view path.
	require.NoError(t, client.Close())
}

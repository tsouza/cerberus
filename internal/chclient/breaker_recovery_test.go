package chclient

import (
	"context"
	"errors"
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
	// window=0 keeps the default rolling failure window; openInterval=interval
	// makes the OPEN backoff elapse on the same tiny cadence the loop ticks at.
	def, registry := buildBreakers(false, 0, 0, interval, nil)
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

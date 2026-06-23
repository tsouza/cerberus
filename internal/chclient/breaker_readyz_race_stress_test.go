package chclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/health"
)

// Layer 11 — concurrency stress over the three shared-mutable-state slots a
// static read-audit can only size by hand: the /readyz TTL cache, the
// circuit-breaker state machine, and the breaker recovery probe. The two
// "medium" findings the audit flagged are exactly the interleavings asserted
// here:
//
//   - the /readyz TTL slot (health.Handler.cachedAt/cachedResp/cachedCode):
//     concurrent probes refresh it under one mutex; a torn write would surface
//     as a -race report on the cache fields.
//   - the breaker recovery probe writing state (allow/record under b.mu) while
//     request goroutines read it (peek/currentState, also under b.mu): the
//     background loop and inbound traffic both mutate the same breaker.
//
// The test stands up a real breaker + recovery loop (over a fake conn whose
// health can be flipped to error on demand) and a real health.Handler with TTL
// caching enabled, then hammers all three slots from N goroutines while a
// controller flips the upstream between healthy and dead. It asserts only what
// a stress test legitimately can: no -race data race (the build flag does the
// real work), no panic, and that the breaker never leaves the closed/open/
// half-open vocabulary. It is bounded (a few hundred ms) so it runs inside the
// `check` lane's `-race` on every PR.

// stressFakeConn is a Pinger whose responses flip between healthy and a
// simulated CH outage under an atomic flag, so the controller goroutine can
// drive the breaker OPEN and back to CLOSED while everything else hammers it.
// It satisfies both chclient.recoveryPinger (the recovery loop) and
// health.Pinger (the readyz cache), so one fake drives both shared slots.
type stressFakeConn struct {
	down      atomic.Bool
	pingCount atomic.Int64
}

var errStressOutage = errors.New("stressFakeConn: simulated CH outage")

func (c *stressFakeConn) Ping(ctx context.Context) error {
	c.pingCount.Add(1)
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.down.Load() {
		return errStressOutage
	}
	return nil
}

// compile-proof the one fake feeds both shared slots.
var (
	_ recoveryPinger = (*stressFakeConn)(nil)
	_ health.Pinger  = (*stressFakeConn)(nil)
)

func TestConcurrencyStress_ReadyzCacheBreakerPool(t *testing.T) {
	t.Parallel()

	const (
		// Tiny so the OPEN backoff elapses and the recovery loop ticks fast;
		// the whole test stays well under a second.
		interval    = time.Millisecond
		pingTimeout = 50 * time.Millisecond
		// readyz TTL is short enough that concurrent probes both hit the
		// refresh branch (write path) and the served-from-cache branch (read
		// path) within the run, exercising both arms of the mutex.
		readyzTTL = 2 * time.Millisecond

		workersPerLane = 8
		runFor         = 300 * time.Millisecond
	)

	conn := &stressFakeConn{}

	// Real breaker registry + a live background recovery loop over the fake
	// conn. This is the production wiring (buildBreakers + startRecoveryLoop),
	// minus the live driver — reused verbatim from the recovery test harness.
	client := newRecoveryTestClient(conn, interval, pingTimeout)
	t.Cleanup(func() { _ = client.Close() })

	// The probe-head breaker is the one /readyz fronts in production; drive the
	// stress over it so the recovery loop, the request path, and the readyz
	// path all converge on a single shared breaker.
	br := client.breakers[HeadProbe]

	// Real /readyz handler with TTL caching ON, backed by the same fake conn.
	// SchemaReady=true so readiness gates only on the ping — the cache slot is
	// what we want under contention.
	h := health.New(health.Options{
		Pinger:      conn,
		SchemaReady: func() bool { return true },
		PingTimeout: pingTimeout,
		CacheTTL:    readyzTTL,
	})
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	deadline := time.Now().Add(runFor)
	cont := func() bool { return time.Now().Before(deadline) }

	var wg sync.WaitGroup
	hc := srv.Client()

	// Lane 1: hammer /readyz — reads + refreshes the TTL cache slot.
	for i := 0; i < workersPerLane; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cont() {
				resp, err := hc.Get(srv.URL + "/readyz")
				if err != nil {
					continue
				}
				_ = resp.Body.Close()
			}
		}()
	}

	// Lane 2: simulate the query_range path — acquire (here: read the fake
	// conn's health via a ping) gated on the breaker's allow()/record(). This
	// is the inbound-traffic writer that races the background recovery probe.
	for i := 0; i < workersPerLane; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cont() {
				if !br.allow() {
					// Breaker OPEN: fast-fail, exactly like the production
					// ErrCircuitOpen path. No record() — allow() returned false.
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
				err := conn.Ping(ctx)
				br.record(ctx, err)
				cancel()
			}
		}()
	}

	// Lane 3: concurrent state readers — the solver's pre-flight peek() and the
	// metrics/logging currentState(), both reading the slot the writers mutate.
	for i := 0; i < workersPerLane; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cont() {
				_ = br.peek()
				_ = br.currentState()
				_ = br.observeLevel()
			}
		}()
	}

	// Controller: flip the upstream dead↔healthy so the breaker is repeatedly
	// driven CLOSED→OPEN (by lane 2's failing pings) and OPEN→HALF-OPEN→CLOSED
	// (by the background recovery loop + lane 2 probes) for the whole run. This
	// is what forces the recovery probe to write breaker state concurrently
	// with the readers and request goroutines.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for cont() {
			conn.down.Store(true)
			time.Sleep(20 * time.Millisecond)
			conn.down.Store(false)
			time.Sleep(20 * time.Millisecond)
		}
	}()

	wg.Wait()

	// No panic / no -race report got us here. Assert the breaker landed in a
	// consistent terminal state — the state machine must never expose anything
	// outside its three-phase vocabulary even after the concurrent churn.
	switch got := br.currentState(); got {
	case "closed", "open", "half-open":
	default:
		t.Fatalf("breaker left a consistent state vocabulary: %q", got)
	}

	// The recovery loop + request lanes actually exercised the fake conn — a
	// zero ping count would mean the lanes never ran the shared paths.
	if conn.pingCount.Load() == 0 {
		t.Fatal("no pings issued — the shared breaker/pool path was never exercised")
	}

	// Final consistency probe on the readyz cache: after the controller's last
	// flip, one more probe must return a well-formed code (the cache slot is
	// not corrupt). With the upstream left healthy at run end this resolves to
	// 200 within one TTL+ping window; accept either readiness verdict since the
	// final flip timing is racy by design — we assert the slot is intact, not a
	// specific value.
	resp, err := hc.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("final /readyz probe errored: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("final /readyz status = %d; want 200 or 503", resp.StatusCode)
	}
}

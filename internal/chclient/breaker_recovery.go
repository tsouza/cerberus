package chclient

import (
	"context"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Active background breaker recovery.
//
// The breaker state machine (breaker.go) recovers PASSIVELY: only an inbound
// request that calls allow() drives the OPEN→HALF-OPEN transition, and the
// HALF-OPEN probe IS the caller's own CH operation under the caller's own
// (often short) context. That coupling has one pathological case, hit by the
// k8s `ch-pod-kill` chaos scenario with ≥2 cerberus replicas behind one
// ClickHouse:
//
//   - CH is killed; both replicas' breakers trip OPEN.
//   - The replica still in the k8s Service keeps getting query traffic
//     (10–12s client budgets), so its HALF-OPEN probe completes and it
//     re-CLOSEs in ~30s.
//   - The OTHER replica goes /readyz-red, k8s pulls it from the Service, and
//     its ONLY remaining traffic is k8s readiness pings — which the health
//     package caps at PingTimeout (default 1s). A recovering clickhouse-go
//     pool needs up to the dial timeout (~5s) to dial fresh or to fail-and-
//     evict a stale half-open socket, so every 1s-bounded probe deadline-
//     exceeds → recorded as a FAILURE → the breaker re-opens → the broken
//     conn is never drained → the breaker stays OPEN until the pod is
//     deleted (~5min). That makes the chaos lane flaky.
//
// The recovery loop closes that gap. It is a per-root-Client background
// goroutine that, on a fixed cadence, fires a SYNTHETIC CH ping through any
// non-CLOSED breaker's existing allow()/record() path under a DEDICATED
// internal context whose timeout is large enough to complete a fresh dial or
// evict a stale conn (≥ the CH dial timeout — see recoveryPingTimeout). That
// makes recovery deterministic and traffic-independent: a Service-pulled
// replica self-heals on the loop's schedule instead of starving on
// too-short readiness pings, and the HeadProbe breaker recovers so /readyz
// flips green and k8s rejoins the pod.
//
// It deliberately reuses the EXISTING state machine — allow() admits the
// probe slot (returning false, and the loop skipping, if a REAL request
// already holds it, so there is never a double-probe), and record() drives
// the HALF-OPEN→CLOSED (success) or HALF-OPEN→OPEN (failure) edge. The loop
// adds no second state machine; it is purely a traffic source for the one
// that already exists.

// recoveryPinger is the narrow slice of driver.Conn the recovery loop needs:
// a single Ping. Narrowing it (rather than holding the whole driver.Conn)
// keeps the loop's contract minimal and makes the unit tests' fake trivial.
type recoveryPinger interface {
	Ping(ctx context.Context) error
}

// recoveryLoop is the lifecycle handle for one Client's background
// breaker-recovery goroutine. It is created and started exactly once, by New,
// on the root Client; ForHead views share the pointer but never start a
// second loop. stop() is idempotent and joins the goroutine so Close is
// goleak-clean.
type recoveryLoop struct {
	stopOnce sync.Once
	stopCh   chan struct{} // closed by stop() to signal the goroutine to exit
	doneCh   chan struct{} // closed by the goroutine just before it returns
}

// recoveryPingTimeout is the per-tick synthetic-ping budget. It MUST be at
// least the CH dial timeout so a fresh dial (or the read that fails+evicts a
// stale half-open socket) can complete inside one probe instead of deadline-
// exceeding and being miscounted as a CH-health failure — the exact bug that
// stranded a traffic-starved replica's breaker OPEN. We size it to the dial
// timeout verbatim: that is the smallest budget that always clears a fresh
// dial, and a probe that needs longer than a full dial is itself evidence CH
// is still unhealthy (a correct FAILURE verdict, not a premature one).
func recoveryPingTimeout(cfg Config) time.Duration {
	return resolveDialTimeout(cfg)
}

// startRecoveryLoop builds the recovery handle and launches its goroutine,
// returning the handle so the root Client can store it (and join it in
// Close). interval is the tick cadence — the same OPEN-state backoff the
// breaker itself would admit a probe on, so the loop never probes faster than
// the breaker's own recovery rhythm. pingTimeout is the per-probe budget
// (recoveryPingTimeout). The breakers slice is every breaker the loop drives:
// the default plus every per-head registry entry.
func startRecoveryLoop(
	conn recoveryPinger,
	breakers []*breaker,
	interval, pingTimeout time.Duration,
) *recoveryLoop {
	r := &recoveryLoop{
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go r.run(conn, breakers, interval, pingTimeout)
	return r
}

// run is the goroutine body: a ticker loop that, on each tick, drives every
// non-CLOSED breaker toward recovery via a synthetic ping. It returns (and
// closes doneCh) as soon as stopCh is signalled.
func (r *recoveryLoop) run(
	conn recoveryPinger,
	breakers []*breaker,
	interval, pingTimeout time.Duration,
) {
	defer close(r.doneCh)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.probeOnce(conn, breakers, pingTimeout)
		}
	}
}

// probeOnce drives a single recovery pass over every breaker. For each
// breaker it first peeks (zero CH I/O) and skips any CLOSED breaker — the
// happy path, where a healthy replica's loop never touches ClickHouse. For a
// non-CLOSED breaker it tries to take the HALF-OPEN probe slot via allow();
// if allow() admits (backoff elapsed and no REAL request already holds the
// slot), it fires the synthetic ping under a fresh dedicated-timeout context
// and feeds the outcome to record(), which either closes the circuit
// (success) or re-opens it and restarts the backoff (failure). If allow()
// declines — a real request is mid-probe, or the backoff hasn't elapsed — the
// loop skips, so it never races or double-probes a real recovery in flight.
func (r *recoveryLoop) probeOnce(conn recoveryPinger, breakers []*breaker, pingTimeout time.Duration) {
	for _, br := range breakers {
		// Cheap read-only gate: a CLOSED breaker needs no recovery, so
		// skip it WITHOUT touching CH. This is what keeps a healthy
		// replica's loop a pure no-op — peek() takes only the breaker's
		// own mutex, never the network.
		if br.peek() == "closed" {
			continue
		}
		// allow() returns true for a CLOSED breaker too, but peek() above
		// already skipped those; here a true means the OPEN backoff elapsed
		// and we hold the single HALF-OPEN probe slot. A false means either
		// the backoff hasn't elapsed or a REAL request is mid-probe — skip,
		// so we never double-probe.
		if !br.allow() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
		err := conn.Ping(ctx)
		br.record(ctx, err)
		cancel()
	}
}

// stop signals the goroutine to exit and blocks until it has. It is
// idempotent — a sync.Once guards the channel close — so a double Close (or a
// Close on a ForHead view that shares this handle) is safe. The join is what
// makes Close goleak-clean: by the time stop returns, the recovery goroutine
// has run its deferred close(doneCh) and exited.
func (r *recoveryLoop) stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
	<-r.doneCh
}

// breakerList flattens the default breaker plus every per-head registry entry
// into the slice the recovery loop iterates. Order is irrelevant — each
// breaker is probed independently — so it is built once at construction and
// never mutated, matching the immutable-registry contract buildBreakers
// already relies on.
func breakerList(def *breaker, registry map[Head]*breaker) []*breaker {
	out := make([]*breaker, 0, len(registry)+1)
	out = append(out, def)
	for _, br := range registry {
		out = append(out, br)
	}
	return out
}

// assert at compile time that a real driver.Conn satisfies recoveryPinger —
// the production conn the root Client hands the loop. Keeps the narrow
// interface honest against the driver surface.
var _ recoveryPinger = driver.Conn(nil)

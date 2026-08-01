package chclient

import (
	"context"
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
//
// The loop's single cancellation primitive is a context OWNED BY THIS HANDLE
// (and therefore by the Client that holds it): cancel is the root of the ctx
// the goroutine selects on AND the parent of every per-ping ctx. That parenting
// is the whole point — an in-flight synthetic ping is aborted the instant
// Close cancels, instead of running out its full per-ping timeout while
// shutdown blocks on the join. A ping ctx rooted at context.Background()
// (what this used to be) made Close block up to recoveryPingTimeout — i.e. a
// full CH dial timeout of dead air on every shutdown that happened to land
// mid-probe.
type recoveryLoop struct {
	// cancel cancels the loop's root ctx: it stops the ticker select AND
	// aborts an in-flight ping, because the per-ping ctx descends from it.
	// context.CancelFunc is safe to call repeatedly and concurrently, which
	// is what makes stop() idempotent (double Close, or a Close on a
	// shared-pointer ForHead view).
	cancel context.CancelFunc
	doneCh chan struct{} // closed by the goroutine just before it returns
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
// (recoveryPingTimeout), applied as an UPPER BOUND on top of the handle's
// cancellable root ctx. The breakers slice is every breaker the loop drives:
// the default plus every per-head registry entry.
func startRecoveryLoop(
	conn recoveryPinger,
	breakers []*breaker,
	interval, pingTimeout time.Duration,
) *recoveryLoop {
	ctx, cancel := context.WithCancel(context.Background())
	r := &recoveryLoop{
		cancel: cancel,
		doneCh: make(chan struct{}),
	}
	go r.run(ctx, conn, breakers, interval, pingTimeout)
	return r
}

// run is the goroutine body: a ticker loop that, on each tick, drives every
// non-CLOSED breaker toward recovery via a synthetic ping. It returns (and
// closes doneCh) as soon as ctx is cancelled — either between ticks (the
// select below) or from inside a probe sweep (probeOnce's ctx checks).
func (r *recoveryLoop) run(
	ctx context.Context,
	conn recoveryPinger,
	breakers []*breaker,
	interval, pingTimeout time.Duration,
) {
	defer close(r.doneCh)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.probeOnce(ctx, conn, breakers, pingTimeout)
		}
	}
}

// probeOnce drives a single recovery pass over every breaker. For each
// breaker it first peeks (zero CH I/O) and skips any CLOSED breaker — the
// happy path, where a healthy replica's loop never touches ClickHouse. For a
// non-CLOSED breaker it tries to take the HALF-OPEN probe slot via allow();
// if allow() admits (backoff elapsed and no REAL request already holds the
// slot), it fires the synthetic ping under a fresh per-ping ctx and feeds the
// outcome to record(), which either closes the circuit (success) or re-opens
// it and restarts the backoff (failure). If allow() declines — a real request
// is mid-probe, or the backoff hasn't elapsed — the loop skips, so it never
// races or double-probes a real recovery in flight.
//
// The per-ping ctx DESCENDS from the loop's ctx, so pingTimeout is only an
// UPPER bound: Close's cancel aborts the ping immediately instead of leaving
// shutdown to wait it out. A ping cut short that way surfaces as
// context.Canceled, which breaker.record treats as "no verdict" — it releases
// the HALF-OPEN probe slot without counting a CH-health failure, exactly the
// right reading of "we shut down mid-probe". The ctx check at the top of each
// iteration keeps a multi-breaker sweep from starting a fresh ping after
// cancellation.
func (r *recoveryLoop) probeOnce(
	ctx context.Context,
	conn recoveryPinger,
	breakers []*breaker,
	pingTimeout time.Duration,
) {
	for _, br := range breakers {
		// Cancelled mid-sweep (Close): stop before opening another ping.
		if ctx.Err() != nil {
			return
		}
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
		pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
		err := conn.Ping(pingCtx)
		br.record(pingCtx, err)
		cancel()
	}
}

// stop cancels the loop's ctx and then blocks until the goroutine has exited.
// The ORDER is the point: cancelling FIRST aborts an in-flight synthetic ping
// (its ctx descends from the loop ctx), so the join that follows returns
// promptly rather than waiting out the remainder of recoveryPingTimeout. It is
// idempotent — context.CancelFunc tolerates repeated and concurrent calls, and
// a receive on an already-closed doneCh returns immediately — so a double
// Close (or a Close on a ForHead view that shares this handle) is safe. The
// join is what makes Close goleak-clean: by the time stop returns, the
// recovery goroutine has run its deferred close(doneCh) and exited.
func (r *recoveryLoop) stop() {
	r.cancel()
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

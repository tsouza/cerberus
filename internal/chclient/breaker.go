package chclient

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// ErrCircuitOpen is the sentinel returned by Client methods when the
// ClickHouse-disconnect circuit breaker has tripped to OPEN. Callers
// translate it into HTTP 503 + `Retry-After: 5` via the per-handler
// error path so saturated upstream failures fail-fast at the wire
// (no dial timeout, no inner-stage retries).
//
// Callers MUST compare via errors.Is — the chclient surface wraps the
// sentinel under stage-prefix wrappers like `chclient: query: ...`,
// matching the existing error-wrapping style in this package.
var ErrCircuitOpen = errors.New("chclient: circuit breaker open")

// Circuit-breaker tuning defaults. These are the GA defaults, applied
// whenever a breaker field is left at its zero value (the zero-value
// breaker, and any Config that doesn't override them) — they're tight
// enough that an actual CH outage trips the breaker within one Grafana
// panel refresh, yet loose enough that transient hiccups (a single slow
// query, a CH node mid-restart) don't open the door to spurious 503s.
//
// As of #95 the three knobs are tunable (and the breaker is disablable)
// via the CERBERUS_CH_BREAKER_* env vars wired through chclient.Config;
// these consts remain the out-of-the-box defaults so behaviour is
// byte-unchanged when nothing is overridden. They are applied through
// the breaker's resolverThreshold / resolverWindow / resolverOpenInterval
// helpers whenever the corresponding per-breaker field is left zero.
//
// breakerThreshold is the number of consecutive failures required
// within breakerWindow for the breaker to trip from CLOSED to OPEN.
// Chosen with a 5-failure budget over a 10-second window: roughly the
// shape of a single CH node going dark — Grafana's default refresh
// (every 5s) fires two panels per window, so a real outage trips the
// breaker before the user can even click again.
//
// breakerOpenInterval is the back-off after OPEN trips. After this
// elapses the breaker switches to HALF-OPEN and admits exactly one
// probe request; success closes the circuit, failure restarts the
// 5-second timer. Five seconds is short enough that recovery is
// near-instant once CH comes back, and long enough that an oscillating
// CH (flapping between healthy and dead) doesn't beat the breaker
// into the ground.
const (
	breakerThreshold    = 5
	breakerWindow       = 10 * time.Second
	breakerOpenInterval = 5 * time.Second
)

// breakerState enumerates the three lifecycle phases of the circuit
// breaker. CLOSED is the normal operating state; OPEN is the
// fast-fail state after consecutive failures; HALF-OPEN is the
// probe-window after the backoff interval has elapsed since the
// OPEN trip — exactly one request passes through to test recovery.
type breakerState int

const (
	stateClosed   breakerState = iota // requests flow through
	stateOpen                         // fast-fail every request
	stateHalfOpen                     // admit exactly one probe
)

// breaker is the per-Client circuit-breaker state machine. The zero
// value is a usable CLOSED breaker; embedding it in Client without
// explicit initialisation is intentional — clients that never see a
// failure stay CLOSED forever and pay zero coordination cost beyond
// the uncontended mutex lock on each call.
//
// All mutable state is guarded by mu. record() and allow() are the
// two write/read paths exercised by the wrapped Client methods; both
// hold mu for the duration. The mutex contention is bounded: every
// CH-touching call already round-trips ClickHouse, so the breaker's
// critical section (a few field accesses + a time.Now call) is
// dwarfed by the network round-trip even on the happy path.
type breaker struct {
	mu sync.Mutex

	// state is the current lifecycle phase. Transitions:
	//   CLOSED → OPEN: failures within the failure window exceed
	//     the configured threshold (resolveThreshold()).
	//   OPEN → HALF-OPEN: allow() called after openedAt +
	//     resolveOpenInterval() has elapsed; the call also reserves
	//     the in-flight probe slot.
	//   HALF-OPEN → CLOSED: probe completes with nil error.
	//   HALF-OPEN → OPEN: probe completes with non-nil error;
	//     openedAt is reset to now so the 5-second timer restarts.
	state breakerState

	// failures counts consecutive errors observed since the
	// failureWindowStart timestamp. record() resets this counter on
	// success or when the window rolls over.
	failures int

	// failureWindowStart anchors the rolling failure window. The
	// breaker counts failures since this timestamp; if more than
	// resolveWindow() elapses without crossing the threshold, the
	// counter resets on the next failure (the window slides).
	failureWindowStart time.Time

	// openedAt records when the breaker tripped OPEN. allow() reads
	// it to decide whether the backoff has elapsed and the next
	// request should be admitted as the HALF-OPEN probe.
	openedAt time.Time

	// probeInFlight is set when a HALF-OPEN probe has been admitted
	// and not yet completed. Concurrent allow() calls during the
	// HALF-OPEN window see it set and short-circuit to
	// ErrCircuitOpen so exactly one probe runs at a time.
	probeInFlight bool

	// now is the clock source. Defaults to time.Now via the helper
	// nowOrTime; tests inject a deterministic clock to drive the
	// state machine without sleeping.
	now func() time.Time

	// disabled, when true, turns the breaker into a no-op: allow()
	// always admits and record() never advances the state machine, so
	// the circuit can never trip. Set from chclient.Config.BreakerDisabled
	// (CERBERUS_CH_BREAKER_ENABLED=false). Default false — the breaker
	// is enabled out of the box.
	disabled bool

	// threshold / window / openInterval are the per-breaker tuning knobs
	// (#95). Each is read through its resolver helper (resolveThreshold /
	// resolveWindow / resolveOpenInterval) so a zero value falls back to
	// the package default — that keeps the zero-value breaker, and any
	// Config that doesn't override a knob, byte-identical to the GA
	// constants. cmd/cerberus sets them from CERBERUS_CH_BREAKER_THRESHOLD
	// / _WINDOW / _OPEN_INTERVAL via chclient.Config.
	threshold    int
	window       time.Duration
	openInterval time.Duration
}

// resolveThreshold returns the configured consecutive-failure threshold,
// falling back to the breakerThreshold default when unset (zero value).
func (b *breaker) resolveThreshold() int {
	if b.threshold > 0 {
		return b.threshold
	}
	return breakerThreshold
}

// resolveWindow returns the configured rolling failure window, falling
// back to the breakerWindow default when unset (zero value).
func (b *breaker) resolveWindow() time.Duration {
	if b.window > 0 {
		return b.window
	}
	return breakerWindow
}

// resolveOpenInterval returns the configured OPEN-state backoff, falling
// back to the breakerOpenInterval default when unset (zero value).
func (b *breaker) resolveOpenInterval() time.Duration {
	if b.openInterval > 0 {
		return b.openInterval
	}
	return breakerOpenInterval
}

// nowOrTime returns b.now() if set, otherwise time.Now(). Kept as a
// helper so the breaker's zero value works without an init step.
func (b *breaker) nowOrTime() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// allow reports whether the next request should proceed through to
// ClickHouse. The caller MUST call record(err) after the request
// completes; the breaker uses record's err to decide whether to
// close (probe success), stay open (probe failure), or merely
// increment the failure counter (regular CLOSED-state failure).
//
// On HALF-OPEN, allow returns true exactly once — for the probe.
// Concurrent callers see probeInFlight set and receive false until
// the probe's record() call completes. This guarantees the GA design:
// at most one in-flight probe through the breaker during HALF-OPEN.
func (b *breaker) allow() bool {
	// A disabled breaker is always-allow: it never short-circuits, so the
	// circuit can never be OPEN to fast-fail against. No lock needed —
	// disabled is set once at construction and never mutated.
	if b.disabled {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case stateClosed:
		return true
	case stateOpen:
		// Has the backoff elapsed? If so, transition to HALF-OPEN
		// and admit the probe in this same call so we don't race
		// other goroutines for the slot.
		if b.nowOrTime().Sub(b.openedAt) < b.resolveOpenInterval() {
			return false
		}
		b.state = stateHalfOpen
		b.probeInFlight = true
		return true
	case stateHalfOpen:
		// Another goroutine already grabbed the probe slot. Wait
		// for record() on that goroutine to resolve the half-open
		// outcome.
		if b.probeInFlight {
			return false
		}
		// Probe completed but state machine hasn't been driven to
		// CLOSED (record() always sets state); if we land here we
		// admit a fresh probe. In practice this branch is
		// unreachable because record() either closes the circuit
		// or re-opens it before releasing probeInFlight; the guard
		// makes the state machine defensive if record is ever
		// split into two phases.
		b.probeInFlight = true
		return true
	default:
		// Unknown state — fail safe (admit). The state field is
		// exclusively written by methods on *breaker, so this
		// branch is unreachable; the default is a defensive
		// fallback.
		return true
	}
}

// peek reports the breaker's current lifecycle phase as a stable string
// WITHOUT mutating state — in particular without admitting or reserving a
// HALF-OPEN probe (unlike allow, which transitions OPEN→HALF-OPEN and
// reserves the probe). It exists for the solver's pre-flight: a routed
// K-shard fan-out must fail fast when the breaker is not CLOSED rather than
// burn the single recovery probe on a doomed request, so the peek is
// strictly read-only.
//
// It does evaluate the OPEN backoff window so a caller sees "would admit a
// probe" (half-open) once the interval has elapsed — but it does NOT take
// the slot; allow still owns the transition. The returned strings are the
// stable vocabulary "closed" / "open" / "half-open".
func (b *breaker) peek() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		return "closed"
	case stateOpen:
		// The backoff has elapsed but no probe is reserved yet — report
		// half-open so the solver still defers to route-A probing.
		if b.nowOrTime().Sub(b.openedAt) >= breakerOpenInterval {
			return "half-open"
		}
		return "open"
	case stateHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// record observes the outcome of a CH-touching call and advances
// the breaker state machine accordingly. ctx is the request context
// the call ran under; err is the post-CH error (nil on success,
// non-nil otherwise). The caller MUST invoke this exactly once after
// allow() returns true; not calling it leaves the breaker in a
// corrupted state (especially under HALF-OPEN where probeInFlight
// stays true and the breaker stalls forever).
//
// Special case: record skips bookkeeping when err == ErrCircuitOpen
// because that error means the breaker's allow() already short-
// circuited; the caller passed it through to record purely to make
// the call-site shape uniform. Counting it would double-fault the
// breaker.
func (b *breaker) record(ctx context.Context, err error) {
	// A disabled breaker keeps no state — record is a no-op so the
	// circuit can never trip. Mirrors allow()'s disabled early-return.
	if b.disabled {
		return
	}

	// Don't double-count: ErrCircuitOpen means allow() returned
	// false, so the call never touched CH. Counting it as a failure
	// would keep the breaker permanently OPEN.
	if errors.Is(err, ErrCircuitOpen) {
		return
	}

	// A ClickHouse MEMORY_LIMIT_EXCEEDED rejection (code 241) is a
	// per-query resource cap doing its job, not CH being down: the
	// server answered with a typed exception, which is positive proof
	// it is alive and healthy. Count it as a SUCCESS so a burst of
	// over-broad queries (e.g. several wide-window matrix panels fired
	// concurrently by a dashboard refresh) can never trip the breaker
	// and 503 unrelated traffic. This mirrors the sample-budget
	// contract (ErrTooManySamples), which stays out of the failure
	// count by construction because it surfaces post-open via
	// cursor.Err(); the memory cap can additionally reject at query
	// open, so it needs the explicit filter here.
	if err != nil && isMemoryLimitExceeded(err) {
		err = nil
	}

	// A ClickHouse TIMEOUT_EXCEEDED rejection (code 159) is the
	// wall-clock sibling of the memory cap above: the server enforcing
	// the per-query `max_execution_time` cerberus stamps (with
	// timeout_overflow_mode=throw) on a query that ran too long. The
	// server answering with a typed exception is positive proof it is
	// alive and healthy — a deliberately-slow / pathological query is
	// not a CH outage. Count it as a SUCCESS so a burst of over-long
	// queries can never trip the breaker and 503 unrelated traffic,
	// exactly as the code-241 memory rejection is handled.
	if err != nil && isQueryTimeoutExceeded(err) {
		err = nil
	}

	// A pool acquire-timeout (clickhouse.ErrAcquireConnTimeout) is NOT a
	// ClickHouse-health failure: it means every connection in the local
	// pool is busy and the acquire blocked past DialTimeout without one
	// freeing up. That is a local pool-sizing signal — the cerberus
	// replica is asking CH for more concurrency than MaxOpenConns
	// allows — and says nothing about whether ClickHouse is alive. The
	// sharded-pushdown solver's fan-out makes this reachable under
	// healthy CH, so counting it would let a too-small pool trip the
	// breaker and 503 traffic against a perfectly healthy backend. Treat
	// it as a SUCCESS so it can never advance the failure counter, the
	// same way the code-241 memory-limit rejection is handled above. The
	// fix for a recurring acquire-timeout is to raise MaxOpenConns, not
	// to fail CH health.
	if err != nil && errors.Is(err, clickhouse.ErrAcquireConnTimeout) {
		err = nil
	}

	// Client-initiated cancellation is not a ClickHouse health
	// signal: the caller walked away before the backend answered.
	// Grafana aborts every in-flight panel query on dashboard
	// navigation, so counting cancellations as failures lets a
	// fast-navigating client trip the breaker against a perfectly
	// healthy CH — the compose kiosk sweep produced exactly that
	// storm (rapid ?viewPanel navigations cancelled dozens of
	// in-flight queries within the 10s window, opened the breaker,
	// and 503'd every panel for the next 5s; see PR #701's
	// kiosk-console-error capture). The ctx.Err() check catches
	// driver errors that stringify the cancellation instead of
	// wrapping context.Canceled. Deadline expiry still counts as a
	// failure: a backend that can't answer inside the caller's
	// budget is operationally indistinguishable from a dead one.
	if err != nil &&
		(errors.Is(err, context.Canceled) ||
			(ctx != nil && errors.Is(ctx.Err(), context.Canceled))) {
		b.mu.Lock()
		defer b.mu.Unlock()
		// A cancelled HALF-OPEN probe is no verdict either way —
		// release the probe slot so the next allow() admits a fresh
		// probe instead of stalling the state machine forever.
		if b.state == stateHalfOpen {
			b.probeInFlight = false
		}
		return
	}

	// PER-REQUEST BREAKER DEDUP. A failure that survived the neutral arms
	// above is a real CH-health signal that WOULD advance the counter. If the
	// request installed a dedup latch (the solver's routed K-shard fan-out via
	// WithBreakerDedup), only the FIRST real failure to win the CAS counts;
	// every sibling failure is treated as breaker-NEUTRAL so K concurrent
	// shard opens against a degraded CH advance the shared counter by exactly
	// 1, not by K. Route-A requests install no latch (claim() returns true on
	// a nil latch), so their counting is byte-unchanged. The neutral sibling
	// still drives the half-open probe slot consistently — it releases an
	// in-flight probe rather than corrupting the state machine, exactly as a
	// cancellation would.
	if err != nil && !breakerDedupFromContext(ctx).claim() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.state == stateHalfOpen {
			b.probeInFlight = false
		}
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case stateHalfOpen:
		// The probe completed. Either close the circuit (success)
		// or restart the backoff timer (failure).
		b.probeInFlight = false
		if err == nil {
			b.state = stateClosed
			b.failures = 0
			b.failureWindowStart = time.Time{}
			return
		}
		// Probe failed: stay OPEN and reset the timer so we try
		// again after another full backoff interval.
		b.state = stateOpen
		b.openedAt = b.nowOrTime()
		// Reset the failure counter — the rolling-window logic is
		// CLOSED-state machinery; once we've tripped OPEN, only
		// the probe outcome matters.
		b.failures = 0
		b.failureWindowStart = time.Time{}
		return

	case stateClosed:
		if err == nil {
			// Success: reset the failure counter. This is the
			// "consecutive failures" semantics — any success
			// resets the counter so a flap of (fail, fail,
			// fail, succeed, fail, fail) doesn't open the
			// circuit.
			b.failures = 0
			b.failureWindowStart = time.Time{}
			return
		}
		// Failure under CLOSED: advance the counter, possibly
		// resetting the window if too long has passed since the
		// first failure of the current sequence.
		now := b.nowOrTime()
		if b.failureWindowStart.IsZero() || now.Sub(b.failureWindowStart) > b.resolveWindow() {
			// Window rolled over — start a fresh count.
			b.failureWindowStart = now
			b.failures = 1
		} else {
			b.failures++
		}
		if b.failures >= b.resolveThreshold() {
			b.state = stateOpen
			b.openedAt = now
			b.failures = 0
			b.failureWindowStart = time.Time{}
		}
		return

	case stateOpen:
		// Shouldn't happen: allow() short-circuits with
		// ErrCircuitOpen, which is filtered above. If it does
		// (e.g. a caller raced allow() with a state change) we
		// stay OPEN — record is idempotent under retries.
		return
	}
}

// currentState returns the current breaker phase as a stable string
// for logging / tests. Always one of "closed", "open", or "half-open".
func (b *breaker) currentState() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateClosed:
		return "closed"
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half-open"
	}
	return "unknown"
}

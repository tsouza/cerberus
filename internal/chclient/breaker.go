package chclient

import (
	"errors"
	"sync"
	"time"
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

// Circuit-breaker tuning. These are the GA defaults, not configurable
// via flags or env vars — they're tight enough that an actual CH
// outage trips the breaker within one Grafana panel refresh, yet
// loose enough that transient hiccups (a single slow query, a CH
// node mid-restart) don't open the door to spurious 503s.
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
	//   CLOSED → OPEN: failures within breakerWindow exceed
	//     breakerThreshold.
	//   OPEN → HALF-OPEN: allow() called after openedAt +
	//     breakerOpenInterval has elapsed; the call also reserves
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
	// breakerWindow elapses without crossing the threshold, the
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
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case stateClosed:
		return true
	case stateOpen:
		// Has the backoff elapsed? If so, transition to HALF-OPEN
		// and admit the probe in this same call so we don't race
		// other goroutines for the slot.
		if b.nowOrTime().Sub(b.openedAt) < breakerOpenInterval {
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

// record observes the outcome of a CH-touching call and advances
// the breaker state machine accordingly. err is the post-CH error
// (nil on success, non-nil otherwise). The caller MUST invoke this
// exactly once after allow() returns true; not calling it leaves the
// breaker in a corrupted state (especially under HALF-OPEN where
// probeInFlight stays true and the breaker stalls forever).
//
// Special case: record skips bookkeeping when err == ErrCircuitOpen
// because that error means the breaker's allow() already short-
// circuited; the caller passed it through to record purely to make
// the call-site shape uniform. Counting it would double-fault the
// breaker.
func (b *breaker) record(err error) {
	// Don't double-count: ErrCircuitOpen means allow() returned
	// false, so the call never touched CH. Counting it as a failure
	// would keep the breaker permanently OPEN.
	if errors.Is(err, ErrCircuitOpen) {
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
		if b.failureWindowStart.IsZero() || now.Sub(b.failureWindowStart) > breakerWindow {
			// Window rolled over — start a fresh count.
			b.failureWindowStart = now
			b.failures = 1
		} else {
			b.failures++
		}
		if b.failures >= breakerThreshold {
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

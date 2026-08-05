package chclient

import (
	"errors"
	"math"
	"time"
)

// CircuitOpenError is the typed carrier every breaker fast-fail returns. It
// wraps [ErrCircuitOpen] — so every existing `errors.Is(err,
// chclient.ErrCircuitOpen)` check keeps matching, bare or nested — and adds the
// one piece of LIVE breaker state the wire layer needs to answer the client
// honestly: RetryAfter, the breaker's OWN configured OPEN-state backoff.
//
// The `Retry-After` header is a promise: it tells the caller the moment the
// service expects to be able to answer. A breaker that fast-fails for
// CERBERUS_CH_BREAKER_OPEN_INTERVAL cannot answer before that interval elapses,
// so any header shorter than it invites a synchronised retry storm against a
// backend the breaker exists to protect, and any header longer than it makes
// the outage look worse than it is. Carrying the interval ON the error is what
// keeps the two in step: the value travels from the breaker that owns it to the
// handler that writes it, through error chains that cross package boundaries
// (chclient -> engine -> api) with no plumbing and no second copy to drift.
type CircuitOpenError struct {
	// Stage is the operation prefix the chclient surface stamps on its errors
	// ("chclient: query", "chclient: ping", "chclient: exec", "solver:
	// pre-flight"). It is rendered verbatim ahead of the sentinel text so the
	// wire message is byte-identical to the fmt.Errorf wrap this type replaced.
	Stage string

	// RetryAfter is the breaker's resolved OPEN-state backoff — the interval
	// after which the breaker admits its next HALF-OPEN recovery probe. Zero
	// means the producer had no breaker to read (a defensive path), in which
	// case [RetryAfterSeconds] falls back to the package default.
	RetryAfter time.Duration
}

// NewCircuitOpenError builds a fast-fail error for stage carrying retryAfter as
// the breaker's recovery interval. Exported for producers OUTSIDE chclient that
// fast-fail on a breaker they only hold through an interface — the solver's
// route-B pre-flight, which peeks the breaker rather than calling through it.
func NewCircuitOpenError(stage string, retryAfter time.Duration) *CircuitOpenError {
	return &CircuitOpenError{Stage: stage, RetryAfter: retryAfter}
}

// Error renders `<stage>: chclient: circuit breaker open`.
func (e *CircuitOpenError) Error() string {
	return e.Stage + ": " + ErrCircuitOpen.Error()
}

// Unwrap exposes the [ErrCircuitOpen] sentinel so errors.Is keeps matching.
func (e *CircuitOpenError) Unwrap() error { return ErrCircuitOpen }

// minRetryAfterSeconds is the floor cerberus advertises on a breaker-open
// response. `Retry-After` is expressed in WHOLE seconds (RFC 9110 §10.2.3), and
// a sub-second recovery interval would otherwise round to 0 — which tells the
// caller to retry immediately, the exact opposite of what a breaker-open
// response means. One second is the smallest value that still reads as "back
// off".
const minRetryAfterSeconds = 1

// RetryAfterSeconds returns the `Retry-After` value (whole seconds) to advertise
// for err, derived from the breaker's own recovery interval rather than from a
// literal. The interval rounds UP so a client that honours the header never
// returns before the breaker would admit its next probe, and is floored at
// [minRetryAfterSeconds].
//
// An error chain that carries no [CircuitOpenError] — a bare ErrCircuitOpen
// produced by a test seam, say — falls back to the package-default
// breakerOpenInterval, the same value a breaker with no operator override
// resolves to.
func RetryAfterSeconds(err error) int {
	retryAfter := breakerOpenInterval
	var open *CircuitOpenError
	if errors.As(err, &open) && open.RetryAfter > 0 {
		retryAfter = open.RetryAfter
	}
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < minRetryAfterSeconds {
		return minRetryAfterSeconds
	}
	return secs
}

// openErr builds the fast-fail error for stage, stamping THIS breaker's
// resolved OPEN-state backoff onto it. Every `if !c.br.allow()` arm in the
// Client surface returns through here, so the interval the wire advertises can
// never drift from the interval the breaker actually enforces.
//
// A nil breaker (the zero-value Client seam) always admits, so this is
// unreachable for one; the guard keeps the fallback honest rather than
// nil-dereferencing if that ever changes.
func (b *breaker) openErr(stage string) error {
	if b == nil {
		return &CircuitOpenError{Stage: stage, RetryAfter: breakerOpenInterval}
	}
	return &CircuitOpenError{Stage: stage, RetryAfter: b.resolveOpenInterval()}
}

// HeadBreakerStates reports the CURRENT lifecycle phase of every per-head
// breaker in this Client's registry, keyed by [Head] — "closed", "open", or
// "half-open". Every ForHead view shares the one registry, so the answer is the
// same whichever view it is asked of.
//
// It reads each breaker's STORED state, exactly as the cerberus_ch_breaker_state
// gauge does (observeLevel), NOT the backoff-evaluating peek(): the readiness
// probe and the gauge must never disagree about which head is tripped, and peek
// would report an untouched OPEN breaker as "half-open" the instant its backoff
// elapsed even though no probe has run. Reading the stored field is also
// strictly read-only — it can never reserve the single HALF-OPEN probe slot out
// from under a real request.
//
// A Client built without the registry (a bare struct literal test seam) has no
// heads and returns nil.
func (c *Client) HeadBreakerStates() map[Head]string {
	if len(c.breakers) == 0 {
		return nil
	}
	out := make(map[Head]string, len(c.breakers))
	for h, br := range c.breakers {
		out[h] = br.currentState()
	}
	return out
}

// BreakerRetryAfter reports the OPEN-state backoff of the breaker THIS Client
// view gates on — the interval after which a tripped breaker admits its next
// recovery probe. It is the value stamped on every fast-fail this view
// produces, exposed for callers that fast-fail on their own (the solver's
// route-B pre-flight peeks the breaker instead of calling through it, so it
// must build the error itself).
func (c *Client) BreakerRetryAfter() time.Duration {
	if c.br == nil {
		return breakerOpenInterval
	}
	return c.br.resolveOpenInterval()
}

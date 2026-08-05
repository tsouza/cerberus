package chclient

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// Layer 1 — the `Retry-After` a breaker-open response advertises is derived
// from the breaker's OWN configured recovery interval. The header is a promise
// about when cerberus expects to be able to answer, and only the breaker that
// tripped knows that; a fixed value drifts the moment an operator tunes
// CERBERUS_CH_BREAKER_OPEN_INTERVAL.

// TestOpenErr_CarriesBreakerOwnInterval — the tripped breaker stamps ITS
// resolved interval on the error it returns, so a breaker tuned away from the
// package default advertises the tuned value.
func TestOpenErr_CarriesBreakerOwnInterval(t *testing.T) {
	t.Parallel()

	tuned := &breaker{openInterval: 45 * time.Second}
	err := tuned.openErr("chclient: query")

	var open *CircuitOpenError
	if !errors.As(err, &open) {
		t.Fatalf("openErr() = %v; want a *CircuitOpenError", err)
	}
	if open.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %v; want 45s", open.RetryAfter)
	}
	if got := RetryAfterSeconds(err); got != 45 {
		t.Errorf("RetryAfterSeconds() = %d; want 45", got)
	}
}

// TestOpenErr_DefaultBreakerUsesPackageDefault — an untuned breaker resolves to
// the package default, which is what the wire has always advertised.
func TestOpenErr_DefaultBreakerUsesPackageDefault(t *testing.T) {
	t.Parallel()

	err := (&breaker{}).openErr("chclient: ping")
	want := int(breakerOpenInterval.Seconds())
	if got := RetryAfterSeconds(err); got != want {
		t.Errorf("RetryAfterSeconds() = %d; want %d", got, want)
	}
}

// TestOpenErr_MatchesSentinelAndMessage — the typed error must stay
// interchangeable with the fmt.Errorf wrap it replaced: every errors.Is check
// in the engine and the three handlers keeps matching, and the message the
// client sees is unchanged.
func TestOpenErr_MatchesSentinelAndMessage(t *testing.T) {
	t.Parallel()

	err := (&breaker{}).openErr("chclient: query")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Error("errors.Is(err, ErrCircuitOpen) = false; want true")
	}
	want := fmt.Errorf("chclient: query: %w", ErrCircuitOpen).Error()
	if err.Error() != want {
		t.Errorf("Error() = %q; want %q", err.Error(), want)
	}
	// Nested one level deeper: handlers see the error through the engine's
	// stage wraps, so errors.As must reach it through a chain too.
	nested := fmt.Errorf("engine: execute: %w", err)
	if got := RetryAfterSeconds(nested); got != int(breakerOpenInterval.Seconds()) {
		t.Errorf("RetryAfterSeconds(nested) = %d; want %d", got, int(breakerOpenInterval.Seconds()))
	}
}

// TestRetryAfterSeconds_RoundsUpAndFloors — the header is whole seconds
// (RFC 9110 §10.2.3). Rounding DOWN would let a client return before the
// breaker admits its next probe, and a sub-second interval truncating to 0
// would tell the client to retry immediately — the opposite of what a
// breaker-open response means.
func TestRetryAfterSeconds_RoundsUpAndFloors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		interval time.Duration
		want     int
	}{
		{"whole", 30 * time.Second, 30},
		{"fractional rounds up", 1500 * time.Millisecond, 2},
		{"sub-second floors to one", 200 * time.Millisecond, minRetryAfterSeconds},
		{"exactly one", time.Second, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := NewCircuitOpenError("solver: pre-flight", tc.interval)
			if got := RetryAfterSeconds(err); got != tc.want {
				t.Errorf("RetryAfterSeconds(%v) = %d; want %d", tc.interval, got, tc.want)
			}
		})
	}
}

// TestRetryAfterSeconds_UnrelatedErrorFallsBack — an error chain that carries
// no breaker interval (a bare sentinel from a test seam, or an unrelated
// failure a handler routes through the same writer) falls back to the package
// default rather than to zero.
func TestRetryAfterSeconds_UnrelatedErrorFallsBack(t *testing.T) {
	t.Parallel()

	want := int(breakerOpenInterval.Seconds())
	for _, err := range []error{
		ErrCircuitOpen,
		fmt.Errorf("chclient: query: %w", ErrCircuitOpen),
		errors.New("something else entirely"),
		nil,
	} {
		if got := RetryAfterSeconds(err); got != want {
			t.Errorf("RetryAfterSeconds(%v) = %d; want %d", err, got, want)
		}
	}
}

// TestBreakerRetryAfter_ReadsTheViewsOwnBreaker — the solver's route-B
// pre-flight fast-fails on a breaker it only peeks, so it has to build the
// error itself; the interval it reads must be the one the very breaker it
// peeked would have enforced.
func TestBreakerRetryAfter_ReadsTheViewsOwnBreaker(t *testing.T) {
	t.Parallel()

	tuned := &Client{br: &breaker{openInterval: 90 * time.Second}}
	if got := tuned.BreakerRetryAfter(); got != 90*time.Second {
		t.Errorf("BreakerRetryAfter() = %v; want 90s", got)
	}
	if got := (&Client{}).BreakerRetryAfter(); got != breakerOpenInterval {
		t.Errorf("BreakerRetryAfter() with no breaker = %v; want %v", got, breakerOpenInterval)
	}
}

// TestHeadBreakerStates_ReportsEveryHeadsStoredPhase — the readiness probe
// reads per-head phases from here, and it must see the SAME phase the
// cerberus_ch_breaker_state gauge exports (the stored state), not a
// backoff-evaluating peek that would silently reserve the HALF-OPEN probe slot.
func TestHeadBreakerStates_ReportsEveryHeadsStoredPhase(t *testing.T) {
	t.Parallel()

	c := &Client{breakers: map[Head]*breaker{
		HeadProm:  {state: stateOpen},
		HeadLoki:  {},
		HeadTempo: {state: stateHalfOpen},
	}}

	states := c.HeadBreakerStates()
	want := map[Head]string{
		HeadProm:  "open",
		HeadLoki:  "closed",
		HeadTempo: "half-open",
	}
	if len(states) != len(want) {
		t.Fatalf("HeadBreakerStates() = %v; want %v", states, want)
	}
	for h, phase := range want {
		if states[h] != phase {
			t.Errorf("HeadBreakerStates()[%s] = %q; want %q", h, states[h], phase)
		}
	}

	// A registry-less view (the bare struct literal test seam) has no heads to
	// report; the probe must not read that as "every head is fine".
	if got := (&Client{}).HeadBreakerStates(); got != nil {
		t.Errorf("HeadBreakerStates() with no registry = %v; want nil", got)
	}
}

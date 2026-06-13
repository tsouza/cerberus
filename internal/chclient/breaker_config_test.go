package chclient

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Layer 11 — circuit-breaker configurability tests (#95). These pin the
// three contracts the tunable/disablable breaker must hold:
//
//   - A custom threshold trips OPEN at exactly the configured failure
//     count, not the GA default of 5.
//   - A disabled breaker is always-allow and never trips, no matter how
//     many failures it records.
//   - A zero-valued breaker (the bare-Config / zero-value path) reproduces
//     the pre-#95 hardcoded GA constants exactly, so default behaviour is
//     byte-unchanged.
//
// They drive the breaker state machine directly via allow()/record() with
// a manually-advanced clock — the breaker spawns no goroutines, so no
// Client / fake-conn plumbing is needed to exercise the transitions.

// fixedClock returns a clock function plus a setter, anchored at a stable
// instant so OPEN-interval timers can be raced without sleeping.
func fixedClock() (func() time.Time, func(time.Time)) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	get := func() time.Time { return now }
	set := func(t time.Time) { now = t }
	return get, set
}

// drive records n consecutive failures against b under a CLOSED-state
// admit, returning after the nth. err is the simulated CH-health failure.
func drive(b *breaker, n int, err error) {
	for i := 0; i < n; i++ {
		_ = b.allow()
		b.record(context.Background(), err)
	}
}

// TestBreaker_CustomThresholdTrips — a breaker configured with
// threshold=3 must trip OPEN on the third consecutive failure, and must
// still be CLOSED after the second. The default threshold (5) would keep
// it CLOSED after three, so this proves the knob is wired, not ignored.
func TestBreaker_CustomThresholdTrips(t *testing.T) {
	t.Parallel()
	clk, _ := fixedClock()
	b := &breaker{threshold: 3, now: clk}
	failErr := errors.New("simulated CH outage")

	drive(b, 2, failErr)
	if got := b.currentState(); got != "closed" {
		t.Fatalf("after 2 failures (threshold 3): state = %q, want closed", got)
	}

	drive(b, 1, failErr)
	if got := b.currentState(); got != "open" {
		t.Fatalf("after 3 failures (threshold 3): state = %q, want open", got)
	}

	// allow() must now fast-fail (OPEN, backoff not yet elapsed).
	if b.allow() {
		t.Fatal("allow() admitted while OPEN before backoff elapsed; want false")
	}
}

// TestBreaker_CustomThresholdHigherThanDefault — threshold=8 must NOT
// trip at the default 5. Guards against a wiring that clamps to the
// default or ignores values above it.
func TestBreaker_CustomThresholdHigherThanDefault(t *testing.T) {
	t.Parallel()
	clk, _ := fixedClock()
	b := &breaker{threshold: 8, now: clk}
	failErr := errors.New("simulated CH outage")

	drive(b, breakerThreshold, failErr) // 5 failures — the GA default
	if got := b.currentState(); got != "closed" {
		t.Fatalf("after %d failures (threshold 8): state = %q, want closed", breakerThreshold, got)
	}

	drive(b, 3, failErr) // now at 8
	if got := b.currentState(); got != "open" {
		t.Fatalf("after 8 failures (threshold 8): state = %q, want open", got)
	}
}

// TestBreaker_DisabledNeverTrips — a disabled breaker admits every call
// and never trips, even after far more failures than any threshold.
func TestBreaker_DisabledNeverTrips(t *testing.T) {
	t.Parallel()
	clk, _ := fixedClock()
	b := &breaker{disabled: true, now: clk}
	failErr := errors.New("simulated CH outage")

	drive(b, 100, failErr)

	if got := b.currentState(); got != "closed" {
		t.Fatalf("disabled breaker after 100 failures: state = %q, want closed", got)
	}
	if !b.allow() {
		t.Fatal("disabled breaker allow() = false; want always-allow")
	}
}

// TestBreaker_DisabledIgnoresLowThreshold — disabled must win even when a
// threshold is also configured (disabled is the master switch).
func TestBreaker_DisabledIgnoresLowThreshold(t *testing.T) {
	t.Parallel()
	clk, _ := fixedClock()
	b := &breaker{disabled: true, threshold: 1, now: clk}
	failErr := errors.New("simulated CH outage")

	drive(b, 50, failErr)
	if got := b.currentState(); got != "closed" {
		t.Fatalf("disabled breaker with threshold=1 after 50 failures: state = %q, want closed", got)
	}
	if !b.allow() {
		t.Fatal("disabled breaker allow() = false; want always-allow")
	}
}

// TestBreaker_DefaultsReproduceGABehaviour — a zero-valued breaker (the
// bare-Config path) trips at exactly the GA default threshold (5), stays
// CLOSED at 4, and uses the GA open-interval (5s) for the HALF-OPEN
// transition. This pins that #95 changed nothing at defaults.
func TestBreaker_DefaultsReproduceGABehaviour(t *testing.T) {
	t.Parallel()
	clk, setNow := fixedClock()
	b := &breaker{now: clk} // all knobs zero — resolves to GA defaults
	failErr := errors.New("simulated CH outage")

	drive(b, breakerThreshold-1, failErr)
	if got := b.currentState(); got != "closed" {
		t.Fatalf("after %d failures: state = %q, want closed (default threshold %d)", breakerThreshold-1, got, breakerThreshold)
	}

	drive(b, 1, failErr) // now at breakerThreshold
	if got := b.currentState(); got != "open" {
		t.Fatalf("after %d failures: state = %q, want open", breakerThreshold, got)
	}

	// Before the GA open-interval elapses, allow() fast-fails.
	setNow(time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC).Add(breakerOpenInterval - time.Second))
	if b.allow() {
		t.Fatal("allow() admitted before default open-interval elapsed; want false")
	}

	// Past the GA open-interval, allow() admits the HALF-OPEN probe.
	setNow(time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC).Add(breakerOpenInterval + time.Second))
	if !b.allow() {
		t.Fatal("allow() did not admit probe after default open-interval elapsed; want true")
	}
	if got := b.currentState(); got != "half-open" {
		t.Fatalf("after open-interval elapsed: state = %q, want half-open", got)
	}
}

// TestBreaker_CustomWindowRolls — with a 2s window, two failures spaced
// 3s apart never trip a threshold-2 breaker, because the window rolls
// over and the failure counter resets between them.
func TestBreaker_CustomWindowRolls(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	now := base
	b := &breaker{threshold: 2, window: 2 * time.Second, now: func() time.Time { return now }}
	failErr := errors.New("simulated CH outage")

	_ = b.allow()
	b.record(context.Background(), failErr) // failure 1 at t=0

	now = base.Add(3 * time.Second) // past the 2s window
	_ = b.allow()
	b.record(context.Background(), failErr) // failure 1 of a fresh window

	if got := b.currentState(); got != "closed" {
		t.Fatalf("two failures spaced past the window: state = %q, want closed", got)
	}

	// A second failure inside the fresh window now trips (threshold 2).
	now = base.Add(3*time.Second + 500*time.Millisecond)
	_ = b.allow()
	b.record(context.Background(), failErr)
	if got := b.currentState(); got != "open" {
		t.Fatalf("second failure inside window: state = %q, want open", got)
	}
}

// TestBreaker_CustomOpenInterval — a breaker with a 1s open-interval
// admits the HALF-OPEN probe after 1s, not the GA default 5s.
func TestBreaker_CustomOpenInterval(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	now := base
	b := &breaker{threshold: 1, openInterval: time.Second, now: func() time.Time { return now }}
	failErr := errors.New("simulated CH outage")

	_ = b.allow()
	b.record(context.Background(), failErr) // trips OPEN (threshold 1)
	if got := b.currentState(); got != "open" {
		t.Fatalf("after 1 failure (threshold 1): state = %q, want open", got)
	}

	// At +1.5s (past the 1s interval, well under the GA 5s) the probe is
	// admitted — proving the custom interval, not the default, governs.
	now = base.Add(1500 * time.Millisecond)
	if !b.allow() {
		t.Fatal("allow() did not admit probe after custom 1s open-interval; want true")
	}
	if got := b.currentState(); got != "half-open" {
		t.Fatalf("after custom open-interval elapsed: state = %q, want half-open", got)
	}
}

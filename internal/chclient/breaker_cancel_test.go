package chclient

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestBreakerRecord_ClientCancellationIsNeutral pins the rule that
// client-initiated cancellation never advances the breaker state
// machine: a cancelled query says nothing about ClickHouse health.
// Grafana aborts every in-flight panel query on dashboard navigation,
// so counting cancellations as failures let a fast-navigating client
// trip the breaker against a perfectly healthy CH — the compose kiosk
// sweep's rapid ?viewPanel navigations cancelled dozens of in-flight
// queries inside one breakerWindow, opened the breaker, and 503'd
// every subsequent panel (PR #701's kiosk-console-error capture).
func TestBreakerRecord_ClientCancellationIsNeutral(t *testing.T) {
	t.Parallel()

	b := &breaker{}

	// Wrapped context.Canceled errors never trip the breaker, no
	// matter how many arrive inside the window.
	for i := 0; i < breakerThreshold*3; i++ {
		b.record(context.Background(), fmt.Errorf("chclient: query: %w", context.Canceled))
	}
	if got := b.currentState(); got != "closed" {
		t.Fatalf("breaker state after wrapped-Canceled storm = %q, want closed", got)
	}

	// Driver errors that stringify the cancellation instead of
	// wrapping context.Canceled are caught via the request context.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < breakerThreshold*3; i++ {
		b.record(canceledCtx, errors.New("driver: context canceled (not wrapped)"))
	}
	if got := b.currentState(); got != "closed" {
		t.Fatalf("breaker state after unwrapped-cancel storm = %q, want closed", got)
	}

	// Genuine backend failures still trip it.
	for i := 0; i < breakerThreshold; i++ {
		b.record(context.Background(), errors.New("dial tcp 127.0.0.1:9000: connection refused"))
	}
	if got := b.currentState(); got != "open" {
		t.Fatalf("breaker state after genuine failures = %q, want open", got)
	}
}

// TestBreakerRecord_CancelledProbeReleasesSlot pins the HALF-OPEN
// interaction: a cancelled probe is no verdict either way, so the
// probe slot must be released for the next allow() to admit a fresh
// probe — swallowing the slot would stall the breaker OPEN forever.
func TestBreakerRecord_CancelledProbeReleasesSlot(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	b := &breaker{now: func() time.Time { return now }}

	// Trip the breaker with genuine failures.
	for i := 0; i < breakerThreshold; i++ {
		b.record(context.Background(), errors.New("connection refused"))
	}
	if got := b.currentState(); got != "open" {
		t.Fatalf("setup: breaker = %q, want open", got)
	}

	// Backoff elapses; allow() admits the HALF-OPEN probe.
	now = now.Add(breakerOpenInterval + time.Second)
	if !b.allow() {
		t.Fatal("expected allow() to admit the half-open probe after backoff")
	}

	// The probe gets cancelled mid-flight. The slot must come free…
	b.record(context.Background(), fmt.Errorf("chclient: query: %w", context.Canceled))
	if got := b.currentState(); got != "half-open" {
		t.Fatalf("breaker after cancelled probe = %q, want half-open", got)
	}
	// …so the next caller is admitted as a fresh probe, and its
	// success closes the circuit.
	if !b.allow() {
		t.Fatal("expected allow() to admit a fresh probe after the cancelled one")
	}
	b.record(context.Background(), nil)
	if got := b.currentState(); got != "closed" {
		t.Fatalf("breaker after successful probe = %q, want closed", got)
	}
}

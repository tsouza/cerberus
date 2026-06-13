package chclient

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSampleBudget_SharedAcrossConsumers — a single budget shared by K
// "shards" enforces ONE per-request limit: the cumulative draw across all
// shards trips at the configured max, not K times it.
func TestSampleBudget_SharedAcrossConsumers(t *testing.T) {
	b := NewSampleBudget(10)
	// Four shards each drawing 3 = 12 total; the budget must reject once the
	// shared remaining hits 0 (after 10 draws).
	ok := 0
	for shard := 0; shard < 4; shard++ {
		for i := 0; i < 3; i++ {
			if b.consume(1) {
				ok++
			}
		}
	}
	if ok != 10 {
		t.Fatalf("shared budget served %d samples, want exactly 10", ok)
	}
	if b.Limit() != 10 {
		t.Fatalf("Limit() = %d, want 10", b.Limit())
	}
}

// TestSampleBudget_Unlimited — a non-positive max never trips.
func TestSampleBudget_Unlimited(t *testing.T) {
	b := NewSampleBudget(0)
	for i := 0; i < 1000; i++ {
		if !b.consume(1) {
			t.Fatalf("unlimited budget tripped at %d", i)
		}
	}
}

// TestWithSampleBudget_RoundTrips — the ctx carries the budget for the
// cursor (and the solver) to consult.
func TestWithSampleBudget_RoundTrips(t *testing.T) {
	b := NewSampleBudget(5)
	ctx := WithSampleBudget(context.Background(), b)
	if got := budgetFromContext(ctx); got != b {
		t.Fatalf("budget did not round-trip through ctx")
	}
	if got := SampleBudgetFromContext(ctx); got != b {
		t.Fatalf("exported accessor did not round-trip the budget")
	}
	// A nil budget is a no-op (route-A path).
	if WithSampleBudget(context.Background(), nil) == nil {
		t.Fatalf("WithSampleBudget(nil) returned a nil ctx")
	}
	if budgetFromContext(context.Background()) != nil {
		t.Fatalf("bare ctx must carry no budget")
	}
}

// TestPeekBreakerState_ReadOnly — peek reports the state WITHOUT reserving
// the half-open probe, so a subsequent allow() can still take it.
func TestPeekBreakerState_ReadOnly(t *testing.T) {
	now := time.Now()
	b := &breaker{now: func() time.Time { return now }}

	if got := b.peek(); got != "closed" {
		t.Fatalf("fresh breaker peek = %q, want closed", got)
	}

	// Trip it OPEN by recording threshold failures.
	for i := 0; i < breakerThreshold+1; i++ {
		b.record(context.Background(), errors.New("ch down"))
	}
	if got := b.peek(); got != "open" {
		t.Fatalf("tripped breaker peek = %q, want open", got)
	}

	// Advance past the backoff: peek should report half-open WITHOUT
	// reserving the probe.
	now = now.Add(breakerOpenInterval + time.Millisecond)
	if got := b.peek(); got != "half-open" {
		t.Fatalf("post-backoff peek = %q, want half-open", got)
	}
	// Peek must not have consumed the probe: allow() can still take it.
	if !b.allow() {
		t.Fatalf("peek consumed the half-open probe — allow() denied")
	}
	// And now the probe IS reserved, so a second allow is denied.
	if b.allow() {
		t.Fatalf("second allow admitted a second concurrent probe")
	}
}

// TestPeekBreakerState_ClientSurface — the *Client wrapper exposes the
// stable string vocabulary.
func TestPeekBreakerState_ClientSurface(t *testing.T) {
	c := &Client{}
	if got := c.PeekBreakerState(); got != "closed" {
		t.Fatalf("zero-value Client breaker = %q, want closed", got)
	}
}

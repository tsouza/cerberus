package admit

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

func newTestLimiter(cap int) *Limiter {
	return newWithProvider("prom", cap, sdkmetric.NewMeterProvider())
}

// TestTryAcquireTopUp_FullGrant — when the semaphore has headroom the top-up
// grants the full requested amount.
func TestTryAcquireTopUp_FullGrant(t *testing.T) {
	l := newTestLimiter(10)
	// Simulate the handler's weight-1 charge.
	rel0, ok := l.Acquire(context.Background())
	if !ok {
		t.Fatal("initial acquire failed")
	}
	defer rel0()

	granted, release := l.TryAcquireTopUp(context.Background(), 3)
	if granted != 3 {
		t.Fatalf("want 3 granted, got %d", granted)
	}
	release()
	// After release the units are back: a second full top-up succeeds.
	granted2, release2 := l.TryAcquireTopUp(context.Background(), 3)
	if granted2 != 3 {
		t.Fatalf("units not released: second top-up granted %d", granted2)
	}
	release2()
}

// TestTryAcquireTopUp_PartialDegrade — when only some units are free the
// top-up grants what it can (degrade, never reject).
func TestTryAcquireTopUp_PartialDegrade(t *testing.T) {
	l := newTestLimiter(3)
	// Consume 1 (handler) + 1 extra so only 1 unit remains free.
	r1, _ := l.Acquire(context.Background())
	defer r1()
	g1, rel1 := l.TryAcquireTopUp(context.Background(), 1)
	if g1 != 1 {
		t.Fatalf("setup grant want 1, got %d", g1)
	}
	defer rel1()

	// Now 1 unit free; ask for 3 — should get exactly 1.
	granted, release := l.TryAcquireTopUp(context.Background(), 3)
	if granted != 1 {
		t.Fatalf("partial degrade: want 1, got %d", granted)
	}
	release()
}

// TestTryAcquireTopUp_ZeroWhenSaturated — a saturated semaphore grants 0,
// and the solver degrades to sequential (it never 503s here).
func TestTryAcquireTopUp_ZeroWhenSaturated(t *testing.T) {
	l := newTestLimiter(1)
	r, _ := l.Acquire(context.Background()) // saturate
	defer r()
	granted, release := l.TryAcquireTopUp(context.Background(), 4)
	if granted != 0 {
		t.Fatalf("saturated top-up granted %d, want 0", granted)
	}
	release() // must be a safe no-op
}

// TestTryAcquireTopUp_NilReceiver — disabled admission grants 0 with a no-op
// release (the solver runs at full P).
func TestTryAcquireTopUp_NilReceiver(t *testing.T) {
	var l *Limiter
	granted, release := l.TryAcquireTopUp(context.Background(), 5)
	if granted != 0 {
		t.Fatalf("nil limiter granted %d, want 0", granted)
	}
	release()
}

// TestTryAcquireTopUp_ReleaseIdempotent — the release closure double-frees
// nothing.
func TestTryAcquireTopUp_ReleaseIdempotent(t *testing.T) {
	l := newTestLimiter(5)
	granted, release := l.TryAcquireTopUp(context.Background(), 2)
	if granted != 2 {
		t.Fatalf("want 2, got %d", granted)
	}
	release()
	release() // second call must not over-release
	// All 5 should be free now: a 5-unit top-up succeeds.
	g, rel := l.TryAcquireTopUp(context.Background(), 5)
	if g != 5 {
		t.Fatalf("double-release corrupted the semaphore: got %d free of 5", g)
	}
	rel()
}

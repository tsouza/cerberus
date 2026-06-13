package chclient

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// errRealCH models a genuine ClickHouse-health failure: not ErrCircuitOpen,
// not a code-241 memory-limit exception, not a cancellation. record() WOULD
// count it under CLOSED state.
var errRealCH = errors.New("dial tcp 127.0.0.1:9000: connection refused")

// TestBreakerDedup_ConcurrentFailuresCountOnce pins the docs §"Parallel
// execution" #6 contract: with a per-request dedup latch installed, K
// CONCURRENT real failures advance the breaker's CLOSED-state failure counter
// by EXACTLY 1 — not by K. Without the latch a routed fan-out of K shard opens
// against a degraded CH would advance the shared counter by up to K in one
// logical request, tripping the threshold-5 breaker far faster than a single
// route-A query.
func TestBreakerDedup_ConcurrentFailuresCountOnce(t *testing.T) {
	t.Parallel()

	for _, gomax := range []int{1, 4} {
		gomax := gomax
		t.Run("gomax"+itoa(gomax), func(t *testing.T) {
			prev := runtime.GOMAXPROCS(gomax)
			defer runtime.GOMAXPROCS(prev)

			b := &breaker{}
			ctx := WithBreakerDedup(context.Background())

			const k = 4 // mirror a 4-shard fan-out
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < k; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start // release all opens at once to maximise the race
					b.record(ctx, errRealCH)
				}()
			}
			close(start)
			wg.Wait()

			b.mu.Lock()
			failures := b.failures
			state := b.state
			b.mu.Unlock()

			if failures != 1 {
				t.Fatalf("dedup latch: %d concurrent failures advanced the counter to %d, want exactly 1", k, failures)
			}
			if state != stateClosed {
				t.Fatalf("breaker must stay CLOSED after one deduped failure, got %v", state)
			}
		})
	}
}

// TestBreakerDedup_RouteAUnaffected pins that a request WITHOUT a dedup latch
// (the single-statement route-A path) records each failure normally — the
// latch only affects opted-in requests, so route-A behaviour is byte-unchanged.
func TestBreakerDedup_RouteAUnaffected(t *testing.T) {
	t.Parallel()

	b := &breaker{}
	// No WithBreakerDedup on this ctx: every failure must count.
	for i := 0; i < breakerThreshold; i++ {
		b.record(context.Background(), errRealCH)
	}
	if got := b.currentState(); got != "open" {
		t.Fatalf("route-A (no latch): %d failures must trip the breaker, got %q", breakerThreshold, got)
	}
}

// TestBreakerDedup_NeutralArmsStillApply pins that the neutral arms
// (ErrCircuitOpen, code-241, cancellation) run BEFORE the dedup consult: a
// latched request whose failures are all neutral never claims the latch, so a
// later REAL failure under the same latch still counts.
func TestBreakerDedup_NeutralArmsStillApply(t *testing.T) {
	t.Parallel()

	b := &breaker{}
	ctx := WithBreakerDedup(context.Background())

	// Neutral failures must NOT claim the latch. Each exercises a distinct
	// neutral arm: ErrCircuitOpen, a wrapped context.Canceled, and a driver
	// error stringifying the cancel but caught via the request ctx.
	b.record(ctx, ErrCircuitOpen)
	b.record(ctx, fmt.Errorf("chclient: query: %w", context.Canceled))
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	b.record(canceledCtx, errors.New("driver: context canceled (not wrapped)"))

	b.mu.Lock()
	failures := b.failures
	b.mu.Unlock()
	if failures != 0 {
		t.Fatalf("neutral failures claimed the latch / advanced the counter: failures=%d", failures)
	}

	// The first REAL failure under the still-unclaimed latch counts.
	b.record(ctx, errRealCH)
	b.mu.Lock()
	failures = b.failures
	b.mu.Unlock()
	if failures != 1 {
		t.Fatalf("first real failure under an unclaimed latch must count: failures=%d, want 1", failures)
	}
}

// TestBreakerDedup_PerRequestLatch pins that the latch is per-REQUEST: a fresh
// WithBreakerDedup ctx claims its own slot, so two separate logical requests
// each count one failure (no cross-request state, the no-caching invariant).
func TestBreakerDedup_PerRequestLatch(t *testing.T) {
	t.Parallel()

	b := &breaker{}

	// Request 1: two concurrent failures count once.
	ctx1 := WithBreakerDedup(context.Background())
	b.record(ctx1, errRealCH)
	b.record(ctx1, errRealCH)

	// Request 2: a fresh latch — its first failure counts again.
	ctx2 := WithBreakerDedup(context.Background())
	b.record(ctx2, errRealCH)

	b.mu.Lock()
	failures := b.failures
	b.mu.Unlock()
	if failures != 2 {
		t.Fatalf("two separate requests must each count one failure: failures=%d, want 2", failures)
	}
}

// itoa is a tiny dependency-free int→string for subtest names.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

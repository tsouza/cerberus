package admit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/tsouza/cerberus/internal/api/admit"
)

// Layer 11 — admission-cap chaos under saturation.

// TestAdmit_Saturated_503WithRetryAfter — once the cap is full, every
// subsequent request must come back 503 with `Retry-After: 1`.
func TestAdmit_Saturated_503WithRetryAfter(t *testing.T) {
	t.Parallel()
	l := admit.New("prom", 1)

	// Hold the only slot.
	rel, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatalf("setup acquire: want ok")
	}
	defer rel()

	h := l.Middleware(1, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatalf("handler must not run while saturated")
	}))

	for range 20 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status: got %d, want 503", rec.Code)
		}
		if rec.Header().Get("Retry-After") != "1" {
			t.Errorf("Retry-After: got %q, want \"1\"", rec.Header().Get("Retry-After"))
		}
		if !strings.Contains(rec.Body.String(), "admission") {
			t.Errorf("body missing admission marker: %s", rec.Body.String())
		}
	}
}

// TestAdmit_ConcurrentReleases_FIFO — release a slot while goroutines
// queue for it; verify subsequent acquires succeed.
//
// (Note: the semaphore here is non-blocking on Acquire, so "queueing"
// is the caller's job — what we verify is that releasing a slot
// permits the next Acquire to succeed.)
func TestAdmit_ConcurrentReleases_FIFO(t *testing.T) {
	t.Parallel()
	const cap = 4
	l := admit.New("prom", cap)

	// Take all 4 slots.
	releases := make([]func(), cap)
	for i := range releases {
		rel, ok := l.Acquire(t.Context())
		if !ok {
			t.Fatalf("acquire %d: want ok", i)
		}
		releases[i] = rel
	}

	// Saturated — next acquire fails.
	if _, ok := l.Acquire(t.Context()); ok {
		t.Fatal("acquire while saturated: want fail")
	}

	// Release one — a fresh acquire must succeed.
	releases[0]()
	rel, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatal("acquire after release: want ok")
	}
	defer rel()
	for i := 1; i < cap; i++ {
		releases[i]()
	}
}

// TestAdmit_PerHandlerCaps_Independent — saturating one limiter must
// not affect another (each head gets its own).
func TestAdmit_PerHandlerCaps_Independent(t *testing.T) {
	t.Parallel()
	prom := admit.New("prom", 1)
	loki := admit.New("loki", 1)

	rel, ok := prom.Acquire(t.Context())
	if !ok {
		t.Fatalf("prom acquire: want ok")
	}
	defer rel()

	// Loki cap is untouched.
	relL, ok := loki.Acquire(t.Context())
	if !ok {
		t.Fatal("loki acquire: should be unaffected by prom saturation")
	}
	defer relL()
}

// TestAdmit_ReleaseAfterReject_NoCorruption — a rejected Acquire still
// returns a non-nil release closure that is a no-op. Calling it must
// not corrupt the semaphore.
func TestAdmit_ReleaseAfterReject_NoCorruption(t *testing.T) {
	t.Parallel()
	l := admit.New("prom", 1)

	rel, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatalf("first acquire: want ok")
	}

	// Second acquire is rejected — the returned release closure is a
	// no-op.
	relReject, ok := l.Acquire(t.Context())
	if ok {
		t.Fatalf("second acquire: want reject")
	}
	if relReject == nil {
		t.Fatalf("rejected acquire: release closure must not be nil")
	}
	relReject() // must not panic / must not free a slot it didn't take
	relReject() // idempotent

	// Original release path still works.
	rel()
	rel2, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatal("re-acquire after legit release: want ok")
	}
	defer rel2()
}

// TestAdmit_StressUnderPressure_NoDeadlock — drive many concurrent
// Acquire/release cycles and verify no goroutine deadlocks.
func TestAdmit_StressUnderPressure_NoDeadlock(t *testing.T) {
	t.Parallel()
	const cap = 8
	l := admit.New("prom", cap)

	var (
		wg       sync.WaitGroup
		admitted atomic.Int64
		rejected atomic.Int64
	)
	deadline := time.After(2 * time.Second)
	stop := make(chan struct{})
	go func() {
		<-deadline
		close(stop)
	}()

	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rel, ok := l.Acquire(t.Context())
				if !ok {
					rejected.Add(1)
					continue
				}
				admitted.Add(1)
				rel()
			}
		}()
	}
	wg.Wait()

	if admitted.Load() == 0 {
		t.Fatal("admitted: zero — limiter never released")
	}
}

// TestAdmit_MiddlewareUnderHandlerPanic_ReleasesSlot — when the inner
// handler panics, the defer-released slot must still go back to the
// pool. Models the "request crash mid-flight" path.
func TestAdmit_MiddlewareUnderHandlerPanic_ReleasesSlot(t *testing.T) {
	t.Parallel()
	l := admit.New("prom", 1)

	h := l.Middleware(1, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("simulated handler panic")
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)

	assert.Panics(t, func() {
		h.ServeHTTP(rec, req)
	}, "inner handler must propagate its panic; middleware does not catch")

	// The slot must be free again — subsequent Acquire succeeds.
	rel, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatal("slot leaked after handler panic")
	}
	defer rel()
}

// TestAdmit_Cancel_BeforeAcquire_NoRelease — Acquire on a cancelled
// context still returns the same boolean (TryAcquire is non-blocking;
// it doesn't consult ctx). Pins the documented behaviour.
func TestAdmit_Cancel_BeforeAcquire_NoRelease(t *testing.T) {
	t.Parallel()
	l := admit.New("prom", 1)

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // pre-cancel

	rel, ok := l.Acquire(ctx)
	if !ok {
		t.Fatal("Acquire on cancelled ctx: TryAcquire ignores ctx; want ok")
	}
	defer rel()
}

// TestAdmit_HeadIdentifier_Stable — Head() reports the head name even
// after a rejection has fired (which writes the head into the counter).
func TestAdmit_HeadIdentifier_Stable(t *testing.T) {
	t.Parallel()
	l := admit.New("loki", 1)
	if got := l.Head(); got != "loki" {
		t.Fatalf("Head: got %q, want loki", got)
	}
	rel, _ := l.Acquire(t.Context())
	defer rel()
	if _, ok := l.Acquire(t.Context()); ok {
		t.Fatal("acquire while saturated: want reject")
	}
	if got := l.Head(); got != "loki" {
		t.Errorf("Head after reject: got %q, want loki", got)
	}
}

// TestAdmit_NilLimiter_NoRetryAfter — disabled-admission path must pass
// requests through with no Retry-After header.
func TestAdmit_NilLimiter_NoRetryAfter(t *testing.T) {
	t.Parallel()
	var l *admit.Limiter
	hitCount := 0
	h := l.Middleware(1, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		hitCount++
	}))

	for range 5 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		h.ServeHTTP(rec, req)
		if rec.Header().Get("Retry-After") != "" {
			t.Errorf("nil limiter set Retry-After: %q", rec.Header().Get("Retry-After"))
		}
	}
	if hitCount != 5 {
		t.Errorf("hits: got %d, want 5", hitCount)
	}
}

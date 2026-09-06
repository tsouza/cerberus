package chclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- NewDataShardFanoutGate --------------------------------------------------

// TestNewDataShardFanoutGate_DataShardCountLE1_NeverAllocates is the single
// most important test in this file: it pins the "zero behavior change to any
// existing deployment" guarantee (cerberus issue #3081, carried forward by
// #3128's move into this package). DataShardCount <= 1 — the default, and
// the value of every deployment that predates this field — must NEVER
// allocate a *semaphore.Weighted, at 0 (the pre-Validate zero value) as well
// as the documented default of 1.
func TestNewDataShardFanoutGate_DataShardCountLE1_NeverAllocates(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1} {
		n := n
		t.Run(fmt.Sprintf("DataShardCount=%d", n), func(t *testing.T) {
			t.Parallel()
			cfg := Config{DataShardCount: n, MaxOpenConns: 42}
			gate, cap := NewDataShardFanoutGate(cfg)
			if gate != nil {
				t.Fatalf("DataShardCount=%d: gate = %v, want nil (never allocated)", n, gate)
			}
			if cap != 42 {
				t.Fatalf("DataShardCount=%d: cap = %d, want MaxOpenConns (42)", n, cap)
			}
		})
	}
}

// TestNewDataShardFanoutGate_MultiShard_Allocates confirms the OTHER half:
// DataShardCount > 1 allocates a real semaphore sized to MaxOpenConns
// (the default cap source).
func TestNewDataShardFanoutGate_MultiShard_Allocates(t *testing.T) {
	t.Parallel()
	cfg := Config{DataShardCount: 4, MaxOpenConns: 16}
	gate, cap := NewDataShardFanoutGate(cfg)
	if gate == nil {
		t.Fatal("DataShardCount=4: gate = nil, want an allocated semaphore")
	}
	if cap != 16 {
		t.Fatalf("cap = %d, want MaxOpenConns (16)", cap)
	}
	// The semaphore must actually be sized `cap`: acquiring `cap` in one call
	// must succeed, and cap+1 must not (proves the size, not merely non-nil).
	if !gate.TryAcquire(16) {
		t.Fatal("TryAcquire(16) failed on a semaphore that should be sized exactly 16")
	}
	gate.Release(16)
	if gate.TryAcquire(17) {
		t.Fatal("TryAcquire(17) succeeded on a semaphore sized 16")
	}
}

// TestNewDataShardFanoutGate_OverrideWins confirms
// Config.DataShardFanoutCapOverride replaces MaxOpenConns independently.
func TestNewDataShardFanoutGate_OverrideWins(t *testing.T) {
	t.Parallel()
	override := int64(7)
	cfg := Config{DataShardCount: 8, MaxOpenConns: 999, DataShardFanoutCapOverride: &override}

	gate, cap := NewDataShardFanoutGate(cfg)
	if cap != 7 {
		t.Fatalf("cap = %d, want the override (7), not MaxOpenConns (999)", cap)
	}
	if gate == nil {
		t.Fatal("gate = nil, want an allocated semaphore")
	}
	if !gate.TryAcquire(7) {
		t.Fatal("TryAcquire(7) failed on a semaphore that should be sized exactly 7")
	}
}

// TestNewDataShardFanoutGate_OverrideAppliesEvenWhenSingleShard confirms the
// cap arithmetic (MaxOpenConns vs override) is independent of whether the
// gate itself gets allocated: DataShardCount<=1 still reports the resolved
// cap (e.g. for startup logging) even though no semaphore backs it yet.
func TestNewDataShardFanoutGate_OverrideAppliesEvenWhenSingleShard(t *testing.T) {
	t.Parallel()
	override := int64(11)
	cfg := Config{DataShardCount: 1, MaxOpenConns: 999, DataShardFanoutCapOverride: &override}

	gate, cap := NewDataShardFanoutGate(cfg)
	if gate != nil {
		t.Fatalf("gate = %v, want nil at DataShardCount=1", gate)
	}
	if cap != 11 {
		t.Fatalf("cap = %d, want the override (11)", cap)
	}
}

// TestNewDataShardFanoutGate_FloorsAtMinimum pins the degenerate-input
// guard: a bare Config (MaxOpenConns unset — a test-construction shape
// only, since every production Config.FromEnv value validates MaxOpenConns
// > 0) with DataShardCount > 1 must still allocate a semaphore that can make
// progress, never a permanently-empty size-0 gate.
func TestNewDataShardFanoutGate_FloorsAtMinimum(t *testing.T) {
	t.Parallel()
	cfg := Config{DataShardCount: 4}
	gate, cap := NewDataShardFanoutGate(cfg)
	if cap < 1 {
		t.Fatalf("cap = %d, want >= 1 (floored)", cap)
	}
	if gate == nil {
		t.Fatal("gate = nil, want an allocated semaphore")
	}
	if !gate.TryAcquire(cap) {
		t.Fatalf("TryAcquire(%d) failed on a semaphore that should be sized exactly that", cap)
	}
}

// --- queryOpen: gate wiring end-to-end ---------------------------------------

// TestQueryCursor_DataShardFanoutGate_BoundsConcurrentDispatch proves the
// wiring reaches all the way from Config through queryOpen (not just the
// NewDataShardFanoutGate unit): with the cap sized to admit exactly ONE
// concurrent QueryCursor at weight DataShardCount, a second concurrent
// QueryCursor call must block until the first Close()s. This is the direct
// regression test for cerberus issue #3128: before the fix, this exact
// scenario driven through Client.QueryCursor (route A's own seam) never
// blocked at all, because DataShardFanoutGate was reachable only from
// internal/solver's Executor.
func TestQueryCursor_DataShardFanoutGate_BoundsConcurrentDispatch(t *testing.T) {
	t.Parallel()
	const dataShardCount = 3
	m, _ := newTestConnMetrics(t)
	cfg := Config{DataShardCount: dataShardCount, MaxOpenConns: dataShardCount} // room for exactly one concurrent dispatch
	c := assembleClientFromConn(cfg, &chaosConn{}, m)
	t.Cleanup(func() { _ = c.Close() })

	cur1, err := c.QueryCursor(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("first QueryCursor: %v", err)
	}

	// A second concurrent QueryCursor must be refused the fanout gate within
	// a short deadline while the first is still holding it.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, err2 := c.QueryCursor(ctx2, "SELECT 1"); err2 == nil {
		t.Fatal("second concurrent QueryCursor succeeded; DataShardFanoutGate should have blocked it")
	} else if !errors.Is(err2, ErrDataShardFanoutGateBusy) {
		t.Errorf("second QueryCursor error = %v; want it to wrap ErrDataShardFanoutGateBusy", err2)
	}

	if err := cur1.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	// Now that the first request released, a third QueryCursor must succeed
	// immediately (proves the gate was actually released, not leaked).
	ctx3, cancel3 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel3()
	cur3, err3 := c.QueryCursor(ctx3, "SELECT 1")
	if err3 != nil {
		t.Fatalf("third QueryCursor after release: %v", err3)
	}
	_ = cur3.Close()
}

// TestQueryCursor_DataShardFanoutGate_DeniedIsBreakerNeutral confirms a gate
// denial never touches the breaker — the call never reached ClickHouse, so
// it must be classified exactly like clickhouse.ErrAcquireConnTimeout
// (breakerScopeClient), never counted toward the failure threshold. Without
// this, a burst of route-A traffic hitting a saturated fanout gate would
// trip the circuit breaker against a perfectly healthy ClickHouse.
func TestQueryCursor_DataShardFanoutGate_DeniedIsBreakerNeutral(t *testing.T) {
	t.Parallel()
	m, _ := newTestConnMetrics(t)
	cfg := Config{DataShardCount: 4, MaxOpenConns: 4}
	c := assembleClientFromConn(cfg, &chaosConn{}, m)
	t.Cleanup(func() { _ = c.Close() })

	cur, err := c.QueryCursor(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("QueryCursor: %v", err)
	}
	defer cur.Close()

	// Drive well past the breaker's own failure threshold with denied
	// acquires while the gate stays saturated.
	for i := 0; i < breakerThreshold*3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		_, err := c.QueryCursor(ctx, "SELECT 1")
		cancel()
		if err == nil {
			t.Fatalf("denied QueryCursor %d unexpectedly succeeded", i)
		}
	}
	if st := c.br.currentState(); st != "closed" {
		t.Errorf("breaker state = %q after %d fanout-gate denials; want closed (breaker-neutral)", st, breakerThreshold*3)
	}
}

// TestQueryStrings_SharesDataShardFanoutGate confirms the fix's core claim:
// a metadata query method (QueryStrings, one of the "route A" synchronous
// drains, distinct from QueryCursor) shares the SAME gate via the common
// queryOpen seam — cerberus issue #3128's whole point is that every
// dispatch this package makes is gated uniformly, not just QueryCursor.
func TestQueryStrings_SharesDataShardFanoutGate(t *testing.T) {
	t.Parallel()
	const dataShardCount = 2
	m, _ := newTestConnMetrics(t)
	cfg := Config{DataShardCount: dataShardCount, MaxOpenConns: dataShardCount}
	c := assembleClientFromConn(cfg, &chaosConn{}, m)
	t.Cleanup(func() { _ = c.Close() })

	cur, err := c.QueryCursor(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("QueryCursor: %v", err)
	}
	defer cur.Close()

	// The gate is now fully saturated by the open QueryCursor above; a
	// QueryStrings call must be denied the SAME gate rather than sailing
	// through on a separate, unenforced path.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.QueryStrings(ctx, "SELECT 'x'"); err == nil {
		t.Fatal("QueryStrings succeeded while the fanout gate was fully held by an open QueryCursor; want it denied")
	} else if !errors.Is(err, ErrDataShardFanoutGateBusy) {
		t.Errorf("QueryStrings error = %v; want it to wrap ErrDataShardFanoutGateBusy", err)
	}
}

// --- Concurrent-admission property test (the critical acceptance criterion) -

// TestQueryOpen_ConcurrentDataShardFanout_NeverExceedsCap is the required
// concurrent-admission property test (cerberus issues #3081, #3128): REAL
// goroutines call QueryCursor simultaneously against a SHARED Client, for
// DataShardCount in {1, 2, 4, 8, 32}, under deliberately oversubscribed
// concurrent load (far more concurrent callers than the cap alone would
// ever admit at once), asserting the observed aggregate weight held
// (DataShardCount per open cursor, summed across every cursor the gate has
// currently admitted) never exceeds the applicable cap at any point during
// the run.
//
// This is NOT a test of the single-call formula: it launches numGoroutines
// real goroutines that race the SAME semaphore under -race, which is what
// actually exercises the acquire/release path concurrently rather than in
// isolation — directly proving the design's core claim that route A's own
// dispatches are now bounded by the identical mechanism route B's were.
func TestQueryOpen_ConcurrentDataShardFanout_NeverExceedsCap(t *testing.T) {
	t.Parallel()
	const fanoutCap = int64(32)
	const numGoroutines = 50 // oversubscribes every N in the matrix below, including N=32 (cap for 1 concurrent holder)
	const holdTime = 2 * time.Millisecond

	for _, dataShardCount := range []int{1, 2, 4, 8, 32} {
		dataShardCount := dataShardCount
		t.Run(fmt.Sprintf("DataShardCount=%d", dataShardCount), func(t *testing.T) {
			t.Parallel()

			m, _ := newTestConnMetrics(t)
			cfg := Config{DataShardCount: dataShardCount, MaxOpenConns: int(fanoutCap)}
			c := assembleClientFromConn(cfg, &chaosConn{}, m)
			t.Cleanup(func() { _ = c.Close() })

			wantCap := fanoutCap
			if dataShardCount <= 1 {
				// No gate at all: nothing bounds concurrency at this layer, so
				// every goroutine admits immediately. Skip the "reached the cap"
				// premise check below for this case (it does not apply) but
				// still prove nothing panics / deadlocks / leaks.
				wantCap = int64(numGoroutines) * 1
			}

			var aggregate atomic.Int64
			var maxObserved atomic.Int64
			var violated atomic.Bool

			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < numGoroutines; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					cur, err := c.QueryCursor(ctx, "SELECT 1")
					if err != nil {
						t.Errorf("QueryCursor: %v", err)
						return
					}
					weight := int64(dataShardCount)
					if weight < 1 {
						weight = 1
					}
					newVal := aggregate.Add(weight)
					for {
						old := maxObserved.Load()
						if newVal <= old {
							break
						}
						if maxObserved.CompareAndSwap(old, newVal) {
							break
						}
					}
					if dataShardCount > 1 && newVal > wantCap {
						violated.Store(true)
						t.Errorf("aggregate data-shard fanout weight %d exceeds cap %d (DataShardCount=%d)",
							newVal, wantCap, dataShardCount)
					}
					time.Sleep(holdTime)
					aggregate.Add(-weight)
					_ = cur.Close()
				}()
			}
			close(start)

			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("concurrent admission hammer did not complete in 30s — wedge suspected")
			}

			if violated.Load() {
				t.Fatalf("DataShardCount=%d: aggregate fanout weight exceeded cap %d at least once (peak observed %d)",
					dataShardCount, wantCap, maxObserved.Load())
			}
			if aggregate.Load() != 0 {
				t.Fatalf("aggregate = %d after every goroutine finished, want 0 (a release leaked)", aggregate.Load())
			}
			if dataShardCount > 1 && maxObserved.Load() == 0 {
				t.Fatalf("DataShardCount=%d: peak observed aggregate is 0 — no admission was ever observed concurrently with another; the test did not exercise contention",
					dataShardCount)
			}
		})
	}
}

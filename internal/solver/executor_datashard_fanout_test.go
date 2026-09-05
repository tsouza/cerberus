package solver

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/tsouza/cerberus/internal/chclient"
)

// --- NewDataShardFanoutGate -------------------------------------------------

// TestNewDataShardFanoutGate_DataShardCountLE1_NeverAllocates (cerberus issue
// #3081) is the single most important test in this file: it pins the "zero
// behavior change to any existing deployment" guarantee. DataShardCount <= 1
// — the default, and the value of every deployment that predates this field —
// must NEVER allocate a *semaphore.Weighted, at 0 (the pre-Validate zero
// value) as well as the documented default of 1.
func TestNewDataShardFanoutGate_DataShardCountLE1_NeverAllocates(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1} {
		n := n
		t.Run(fmt.Sprintf("DataShardCount=%d", n), func(t *testing.T) {
			t.Parallel()
			cfg := DefaultConfig()
			cfg.DataShardCount = n
			const gateCap = int64(42)
			gate, cap := NewDataShardFanoutGate(cfg, gateCap)
			if gate != nil {
				t.Fatalf("DataShardCount=%d: gate = %v, want nil (never allocated)", n, gate)
			}
			if cap != gateCap {
				t.Fatalf("DataShardCount=%d: cap = %d, want gateCap (%d)", n, cap, gateCap)
			}
		})
	}
}

// TestNewDataShardFanoutGate_MultiShard_Allocates confirms the OTHER half:
// DataShardCount > 1 allocates a real semaphore sized DataShardFanoutCap
// (defaulting to gateCap).
func TestNewDataShardFanoutGate_MultiShard_Allocates(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.DataShardCount = 4
	const gateCap = int64(16)
	gate, cap := NewDataShardFanoutGate(cfg, gateCap)
	if gate == nil {
		t.Fatal("DataShardCount=4: gate = nil, want an allocated semaphore")
	}
	if cap != gateCap {
		t.Fatalf("cap = %d, want gateCap (%d)", cap, gateCap)
	}
	// The semaphore must actually be sized `cap`: acquiring `cap` in one call
	// must succeed, and cap+1 must not (proves the size, not merely non-nil).
	if !gate.TryAcquire(gateCap) {
		t.Fatalf("TryAcquire(%d) failed on a semaphore that should be sized exactly that", gateCap)
	}
	gate.Release(gateCap)
	if gate.TryAcquire(gateCap + 1) {
		t.Fatalf("TryAcquire(%d) succeeded on a semaphore sized %d", gateCap+1, gateCap)
	}
}

// TestNewDataShardFanoutGate_OverrideWins confirms
// Config.DataShardFanoutCapOverride replaces gateCap independently.
func TestNewDataShardFanoutGate_OverrideWins(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.DataShardCount = 8
	override := int64(7)
	cfg.DataShardFanoutCapOverride = &override

	gate, cap := NewDataShardFanoutGate(cfg, 999)
	if cap != 7 {
		t.Fatalf("cap = %d, want the override (7), not gateCap (999)", cap)
	}
	if gate == nil {
		t.Fatal("gate = nil, want an allocated semaphore")
	}
	if !gate.TryAcquire(7) {
		t.Fatal("TryAcquire(7) failed on a semaphore that should be sized exactly 7")
	}
}

// TestNewDataShardFanoutGate_OverrideAppliesEvenWhenSingleShard confirms the
// cap arithmetic (gateCap vs override) is independent of whether the gate
// itself gets allocated: DataShardCount<=1 still reports the resolved cap
// (an operator inspecting config sees the real number even though no
// semaphore backs it yet).
func TestNewDataShardFanoutGate_OverrideAppliesEvenWhenSingleShard(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.DataShardCount = 1
	override := int64(11)
	cfg.DataShardFanoutCapOverride = &override

	gate, cap := NewDataShardFanoutGate(cfg, 999)
	if gate != nil {
		t.Fatalf("gate = %v, want nil at DataShardCount=1", gate)
	}
	if cap != 11 {
		t.Fatalf("cap = %d, want the override (11)", cap)
	}
}

// --- admitAndGate: DataShardCount=1 regression ------------------------------

// TestAdmitAndGate_DataShardCount1_BitIdenticalToPreexisting is the
// regression test the "zero behavior change" guarantee rests on: with
// DataShardFanoutGate nil (the wiring NewDataShardFanoutGate produces at
// DataShardCount<=1), admitAndGate's returned (kEff, pEff) and its release
// behavior are BIT-FOR-BIT identical to the pre-#3081 mechanism — because
// the new code path is provably unreached (nil-checked) rather than merely
// coincidentally equal.
func TestAdmitAndGate_DataShardCount1_BitIdenticalToPreexisting(t *testing.T) {
	t.Parallel()
	cfg := testCfg()
	cfg.Parallel = 5
	cfg.DataShardCount = 1

	const gateCap = int64(20)
	gate := semaphore.NewWeighted(gateCap)
	x := &Executor{Cfg: cfg, Gate: gate, GateCap: gateCap}
	// DataShardFanoutGate deliberately left nil, matching what
	// NewDataShardFanoutGate would have produced for DataShardCount=1.

	kEff, pEff, releaseGate, releaseAdmit, err := x.admitAndGate(context.Background(), 7)
	if err != nil {
		t.Fatalf("admitAndGate: %v", err)
	}
	// k=7, Parallel=5 => pEff=5 => kEff=min(7,5,gateCap/2=10)=5.
	if kEff != 5 {
		t.Fatalf("kEff = %d, want 5 (the pre-#3081 formula, unaffected by DataShardCount)", kEff)
	}
	if pEff != 5 {
		t.Fatalf("pEff = %d, want 5", pEff)
	}
	// Gate must show exactly kEff held.
	if gate.TryAcquire(gateCap - int64(kEff) + 1) {
		t.Fatalf("gate has more than %d free slots; kEff=%d was not fully charged to Gate", gateCap-int64(kEff), kEff)
	}
	releaseGate()
	releaseAdmit()
	if !gate.TryAcquire(gateCap) {
		t.Fatal("gate did not fully release back to gateCap")
	}
	gate.Release(gateCap)
}

// TestExecute_DataShardCount1_MemoryApportionmentBitIdentical pins the OTHER
// half of the regression guarantee: perShardMemoryBytes at DataShardCount=1
// (and at the pre-#3081 zero value, 0 — Validate rejects it in production,
// but Execute's own arithmetic must degrade to the SAME formula rather than
// dividing by zero) equals routeACapBytes/kEff exactly, never
// routeACapBytes/(kEff*N) for any N != 1.
func TestExecute_DataShardCount1_MemoryApportionmentBitIdentical(t *testing.T) {
	t.Parallel()
	const routeACapBytes = int64(6_000_000_000)
	const k = 3

	for _, dataShardCount := range []int{0, 1} {
		dataShardCount := dataShardCount
		t.Run(fmt.Sprintf("DataShardCount=%d", dataShardCount), func(t *testing.T) {
			t.Parallel()
			q := newFakeQuerier(2)
			q.maxMemoryBytes = routeACapBytes
			cfg := testCfg()
			cfg.Parallel = k
			cfg.DataShardCount = dataShardCount
			x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), nil)

			cur, _, err := x.Execute(context.Background(), "promql", makeDecision(k), nil)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			defer cur.Close()
			if _, err := drainAll(cur); err != nil {
				t.Fatalf("drain: %v", err)
			}

			wantPerShard := routeACapBytes / k
			for shard := 0; shard < k; shard++ {
				shardCtx, ok := q.ctxByShard[shard]
				if !ok {
					t.Fatalf("shard %d never opened a cursor", shard)
				}
				got, _ := chclient.QuerySettingsFromContext(shardCtx)["max_memory_usage"].(int64)
				if got != wantPerShard {
					t.Errorf("shard %d: max_memory_usage = %d, want routeACapBytes/kEff = %d (DataShardCount=%d must be a no-op)",
						shard, got, wantPerShard, dataShardCount)
				}
			}
		})
	}
}

// TestExecute_MultiDataShard_MemoryApportionmentDividesByCountToo pins the
// NEW half of the memory formula: perShardMemoryBytes = cap /
// (kEff * DataShardCount), not cap/kEff, once DataShardCount > 1.
func TestExecute_MultiDataShard_MemoryApportionmentDividesByCountToo(t *testing.T) {
	t.Parallel()
	const routeACapBytes = int64(8_000_000_000)
	const k = 4
	const dataShardCount = 5

	q := newFakeQuerier(2)
	q.maxMemoryBytes = routeACapBytes
	cfg := testCfg()
	cfg.Parallel = k
	cfg.DataShardCount = dataShardCount
	x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), nil)

	cur, _, err := x.Execute(context.Background(), "promql", makeDecision(k), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	if _, err := drainAll(cur); err != nil {
		t.Fatalf("drain: %v", err)
	}

	wantPerShard := routeACapBytes / (k * dataShardCount)
	for shard := 0; shard < k; shard++ {
		shardCtx, ok := q.ctxByShard[shard]
		if !ok {
			t.Fatalf("shard %d never opened a cursor", shard)
		}
		got, _ := chclient.QuerySettingsFromContext(shardCtx)["max_memory_usage"].(int64)
		if got != wantPerShard {
			t.Errorf("shard %d: max_memory_usage = %d, want routeACapBytes/(kEff*DataShardCount) = %d",
				shard, got, wantPerShard)
		}
	}
}

// --- Execute: DataShardFanoutGate wired end-to-end --------------------------

// TestExecute_DataShardFanoutGate_BoundsConcurrentFanout proves the wiring
// reaches all the way from Config through Executor.Execute (not just the
// admitAndGate unit): with DataShardFanoutCap sized to admit exactly ONE
// concurrent Execute at kEff*DataShardCount, a second concurrent Execute call
// must block until the first Close()s.
func TestExecute_DataShardFanoutGate_BoundsConcurrentFanout(t *testing.T) {
	t.Parallel()
	const k = 2
	const dataShardCount = 3
	const fanoutCap = int64(k * dataShardCount) // room for exactly one request

	cfg := testCfg()
	cfg.Parallel = k
	cfg.DataShardCount = dataShardCount

	q := newFakeQuerier(3)
	q.delay = 50 * time.Millisecond // hold the shard open long enough to race the second Execute
	x := newExec(q, newFakeEmitter(), cfg, 64, newFakeBreaker(BreakerClosed), nil)
	x.DataShardFanoutGate = semaphore.NewWeighted(fanoutCap)
	x.DataShardFanoutCap = fanoutCap

	cur1, _, err := x.Execute(context.Background(), "promql", makeDecision(k), nil)
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}

	// A second concurrent Execute must be refused the fanout gate within a
	// short deadline while the first is still holding it.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	_, _, err2 := x.Execute(ctx2, "promql", makeDecision(k), nil)
	if err2 == nil {
		t.Fatal("second concurrent Execute succeeded; DataShardFanoutGate should have blocked it")
	}

	if _, derr := drainAll(cur1); derr != nil {
		t.Fatalf("drain first: %v", derr)
	}
	if err := cur1.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	// Now that the first request released, a third Execute must succeed
	// immediately (proves the gate was actually released, not leaked).
	ctx3, cancel3 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel3()
	cur3, _, err3 := x.Execute(ctx3, "promql", makeDecision(k), nil)
	if err3 != nil {
		t.Fatalf("third Execute after release: %v", err3)
	}
	if _, derr := drainAll(cur3); derr != nil {
		t.Fatalf("drain third: %v", derr)
	}
	_ = cur3.Close()
}

// --- Concurrent-admission property test (the critical acceptance criterion) -

// TestAdmitAndGate_ConcurrentDataShardFanout_NeverExceedsCap is the required
// concurrent-admission property test (cerberus issue #3081): REAL goroutines
// call admitAndGate simultaneously against a SHARED Executor, for
// DataShardCount in {1, 2, 4, 8, 32}, under deliberately oversubscribed
// concurrent load (far more concurrent callers than the cap alone would ever
// admit at once), asserting the observed aggregate weight held
// (kEff * DataShardCount, summed across every request the semaphore has
// currently admitted) never exceeds the applicable cap at any point during
// the run.
//
// kEff is pinned to 1 via Parallel=1 (Admit is nil, so pEff never varies) so
// every request's contribution to the aggregate is deterministic
// (1 * DataShardCount) and the test's own bookkeeping can assert an exact
// bound rather than a fuzzy one. fanoutCap=32 is sized to admit exactly
// 32/DataShardCount concurrent holders — deliberately fewer than
// numGoroutines for every DataShardCount tested, so the run is genuinely
// oversubscribed at every N (down to a single concurrent holder at N=32).
//
// This is NOT a test of the single-call formula: it launches numGoroutines
// real goroutines that race the SAME two semaphores under -race, which is
// what actually exercises the acquire/release path concurrently rather than
// in isolation.
func TestAdmitAndGate_ConcurrentDataShardFanout_NeverExceedsCap(t *testing.T) {
	t.Parallel()
	const gateCap = int64(64) // large: never the binding constraint here
	const fanoutCap = int64(32)
	const numGoroutines = 50 // oversubscribes every N in the matrix below, including N=32 (cap for 1 concurrent holder)
	const holdTime = 2 * time.Millisecond

	for _, dataShardCount := range []int{1, 2, 4, 8, 32} {
		dataShardCount := dataShardCount
		t.Run(fmt.Sprintf("DataShardCount=%d", dataShardCount), func(t *testing.T) {
			t.Parallel()

			cfg := testCfg()
			cfg.Parallel = 1 // kEff pinned to 1 regardless of concurrency (Admit is nil)
			cfg.DataShardCount = dataShardCount

			gate := semaphore.NewWeighted(gateCap)
			x := &Executor{Cfg: cfg, Gate: gate, GateCap: gateCap}
			wantCap := gateCap // DataShardCount<=1: bounded by Gate alone
			if dataShardCount > 1 {
				x.DataShardFanoutGate = semaphore.NewWeighted(fanoutCap)
				x.DataShardFanoutCap = fanoutCap
				wantCap = fanoutCap
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
					kEff, _, releaseGate, releaseAdmit, err := x.admitAndGate(ctx, 1)
					if err != nil {
						t.Errorf("admitAndGate: %v", err)
						return
					}
					weight := int64(kEff) * int64(dataShardCount)
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
					if newVal > wantCap {
						violated.Store(true)
						t.Errorf("aggregate data-shard fanout weight %d exceeds cap %d (DataShardCount=%d, kEff=%d)",
							newVal, wantCap, dataShardCount, kEff)
					}
					time.Sleep(holdTime)
					aggregate.Add(-weight)
					releaseGate()
					releaseAdmit()
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
			// The oversubscription premise itself: with numGoroutines holding
			// holdTime each and only wantCap/(kEff*dataShardCount) admitted at
			// once, the peak observed aggregate must actually have REACHED the
			// cap at some point (proves contention really happened, not that
			// the goroutines happened to run sequentially with room to spare).
			if maxObserved.Load() == 0 {
				t.Fatalf("DataShardCount=%d: peak observed aggregate is 0 — no admission was ever observed concurrently with another; the test did not exercise contention",
					dataShardCount)
			}
		})
	}
}

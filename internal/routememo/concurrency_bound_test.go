package routememo

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// concurrentBurstMultiplier sets the same-key burst size as a small multiple
// of maxConcurrentRoutedDispatches: comfortably more goroutines than the
// dispatch-token budget can ever admit, so the test actually exercises
// contention on the semaphore instead of trivially passing because nobody
// raced for a token.
const concurrentBurstMultiplier = 5

// TestAmplificationBound_ConcurrentSameKeyFailuresCapAtDispatchBudget pins
// the invariant that a burst of concurrent route-A failures on the SAME key
// — however large — can never mint more admitted dispatches than the
// process-wide dispatch-token budget allows. A client retry storm hammering
// one hot, already-failing key is exactly the shape this guards against: if
// the corroboration-check-then-admit step were not atomic with the state
// read it decides on, N concurrent callers could all observe "not yet
// admitted" before any of them updated shared state, each independently
// grab a token, and the single non-blocking semaphore would stop bounding
// anything. The fix is that ObserveRouteAFailureAndMaybeBeginProbe holds the
// memo's lock for its entire read-decide-admit sequence, so this test
// exercises that with real goroutines rather than a sequential loop that
// could never observe the race either way.
func TestAmplificationBound_ConcurrentSameKeyFailuresCapAtDispatchBudget(t *testing.T) {
	m := New(testPressureWindow)
	clk := newFakeClock()
	m.SetNowForTest(clk.now)

	k := testKey("hot-same-key")

	// Sequentially drive the key to exactly the corroboration floor: after
	// minCorroboratingFailures consecutive route-A resource failures with no
	// intervening success, the key is admit-eligible (Unknown state,
	// corroboration == minCorroboratingFailures) but no dispatch has been
	// admitted yet — plain Observe only records, it never admits.
	for i := 0; i < minCorroboratingFailures; i++ {
		m.Observe(k, RouteA, OutcomeResourceFailure)
	}

	burstSize := concurrentBurstMultiplier * maxConcurrentRoutedDispatches

	var admitted int32
	var releasesMu sync.Mutex
	var releases []func()

	var wg sync.WaitGroup
	wg.Add(burstSize)
	for i := 0; i < burstSize; i++ {
		go func() {
			defer wg.Done()
			release, ok := m.ObserveRouteAFailureAndMaybeBeginProbe(k)
			if ok {
				atomic.AddInt32(&admitted, 1)
				releasesMu.Lock()
				releases = append(releases, release)
				releasesMu.Unlock()
			}
		}()
	}
	wg.Wait()

	for _, release := range releases {
		release()
	}

	got := int(admitted)
	if got > maxConcurrentRoutedDispatches {
		t.Fatalf("amplification bound violated: %d of %d concurrent same-key failures were admitted, budget is maxConcurrentRoutedDispatches=%d",
			got, burstSize, maxConcurrentRoutedDispatches)
	}
	// The key was already admit-eligible before the burst started and none
	// of the burst goroutines release their token mid-burst, so the token
	// budget is the only thing that can say no: exactly
	// maxConcurrentRoutedDispatches of them must succeed, not fewer.
	if got != maxConcurrentRoutedDispatches {
		t.Fatalf("expected exactly maxConcurrentRoutedDispatches=%d admissions from a same-key burst that exceeds the budget, got %d",
			maxConcurrentRoutedDispatches, got)
	}
}

// distinctKeyBurstMultiplier sets how many brand-new distinct keys fire
// concurrently once the cluster is already sitting right at the pressure
// trip line: comfortably more than pressureFailureThreshold so the burst is
// guaranteed to push cluster-wide pressure over the line, not just graze it.
const distinctKeyBurstMultiplier = 5

// TestClusterPressureBound_ConcurrentDistinctKeyFailuresBlockAllDispatch
// pins the cluster-wide pressure damper under real concurrency: once more
// than pressureFailureThreshold distinct keys have failed within one
// pressure window, every further ObserveRouteAFailureAndMaybeBeginProbe
// call — for any key, admit-eligible or not — must be refused, AND must
// leave no memo state behind for the key it was called with. That second
// half is the sharper property: the pressure check in
// ObserveRouteAFailureAndMaybeBeginProbe runs strictly before
// getLiveLocked/observeRouteAResourceFailureLocked, so a throttled failure
// must never even accumulate corroboration bookkeeping. If the ordering
// were reversed (state written first, pressure checked after), a bursty
// noisy-neighbour event wouldn't just fail to rescue traffic — it would
// actively poison the memo with corroboration counts for keys that were
// never actually confirmed to fail on their own merits.
func TestClusterPressureBound_ConcurrentDistinctKeyFailuresBlockAllDispatch(t *testing.T) {
	m := New(testPressureWindow)
	clk := newFakeClock()
	m.SetNowForTest(clk.now)

	// Prime the pressure tracker to exactly pressureFailureThreshold
	// distinct keys, sequentially, so the cluster sits right at the trip
	// line without crossing it yet (the gate is a strict
	// countFresh > pressureFailureThreshold).
	for i := 0; i < pressureFailureThreshold; i++ {
		m.Observe(testKey(fmt.Sprintf("prime-%d", i)), RouteA, OutcomeResourceFailure)
	}
	if m.UnderPressure() {
		t.Fatalf("setup invariant broken: priming exactly pressureFailureThreshold distinct keys must not itself trip pressure")
	}

	burstSize := distinctKeyBurstMultiplier * pressureFailureThreshold
	burstKeys := make([]Key, burstSize)
	for i := range burstKeys {
		burstKeys[i] = testKey(fmt.Sprintf("burst-%d", i))
	}

	admittedOK := make([]bool, burstSize)

	var wg sync.WaitGroup
	wg.Add(burstSize)
	for i, k := range burstKeys {
		i, k := i, k
		go func() {
			defer wg.Done()
			_, ok := m.ObserveRouteAFailureAndMaybeBeginProbe(k)
			admittedOK[i] = ok // each goroutine owns a distinct index — no shared write
		}()
	}
	wg.Wait()

	if !m.UnderPressure() {
		t.Fatalf("expected cluster-wide pressure to be tripped after more than pressureFailureThreshold distinct keys failed concurrently")
	}

	for i, k := range burstKeys {
		if admittedOK[i] {
			t.Fatalf("key %+v was admitted despite cluster-wide pressure being tripped", k)
		}

		state, stale := m.Lookup(k)
		if state != Unknown || stale {
			t.Fatalf("key %+v: expected Lookup to still report Unknown/non-stale (no memo state written) after being throttled by pressure, got state=%v stale=%v",
				k, state, stale)
		}

		m.mu.Lock()
		_, wrote := m.entries[k]
		m.mu.Unlock()
		if wrote {
			t.Fatalf("key %+v: the pressure gate must short-circuit BEFORE any state mutation, but a memo entry was written", k)
		}
	}
}

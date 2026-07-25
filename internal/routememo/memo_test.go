package routememo

import (
	"sync"
	"testing"
	"time"
)

const testPressureWindow = time.Minute

func testKey(tag string) Key {
	return Key{RootKind: tag}
}

// fakeClock lets tests move memo time deterministically.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestMemo() (*Memo, *fakeClock) {
	m := New(testPressureWindow)
	clk := newFakeClock()
	m.now = clk.now
	return m, clk
}

// --- Non-negotiable: retry fires only after corroboration, memoized once ---

func TestCorroborationRequiresTwoConsecutiveFailuresNoInterveningSuccess(t *testing.T) {
	m, _ := newTestMemo()
	k := testKey("A")

	// A single failure must NOT make the key probe-eligible.
	m.Observe(k, RouteA, OutcomeResourceFailure)
	if _, ok := m.BeginProbe(k); ok {
		t.Fatalf("single resource failure must not grant probe eligibility")
	}

	// A second, consecutive failure DOES.
	m.Observe(k, RouteA, OutcomeResourceFailure)
	release, ok := m.BeginProbe(k)
	if !ok {
		t.Fatalf("two consecutive resource failures must grant probe eligibility")
	}
	release()
}

func TestInterveningSuccessResetsCorroboration(t *testing.T) {
	m, _ := newTestMemo()
	k := testKey("A")

	m.Observe(k, RouteA, OutcomeResourceFailure)
	m.Observe(k, RouteA, OutcomeSuccess) // resets
	m.Observe(k, RouteA, OutcomeResourceFailure)

	if _, ok := m.BeginProbe(k); ok {
		t.Fatalf("an intervening success must reset the corroboration counter — this failure is only the first since the reset")
	}
}

func TestSuccessfulRouteBRetryIsMemoizedAndSubsequentQueryRoutesDirectly(t *testing.T) {
	m, _ := newTestMemo()
	k := testKey("A")

	m.Observe(k, RouteA, OutcomeResourceFailure)
	m.Observe(k, RouteA, OutcomeResourceFailure)
	release, ok := m.BeginProbe(k)
	if !ok {
		t.Fatalf("expected probe eligibility after corroboration")
	}
	m.Observe(k, RouteB, OutcomeSuccess)
	release()

	state, stale := m.Lookup(k)
	if state != PreferB || stale {
		t.Fatalf("Lookup(k) = (%v, stale=%v), want (PreferB, stale=false)", state, stale)
	}
}

func TestFailedRouteBRetryIsMemoizedNegativelyAndNotRetriedAgain(t *testing.T) {
	m, _ := newTestMemo()
	k := testKey("A")

	m.Observe(k, RouteA, OutcomeResourceFailure)
	m.Observe(k, RouteA, OutcomeResourceFailure)
	release, ok := m.BeginProbe(k)
	if !ok {
		t.Fatalf("expected probe eligibility after corroboration")
	}
	m.Observe(k, RouteB, OutcomeResourceFailure)
	release()

	state, _ := m.Lookup(k)
	if state != BothFail {
		t.Fatalf("Lookup(k) = %v, want BothFail", state)
	}
	// BothFail must never grant a second probe.
	if _, ok := m.BeginProbe(k); ok {
		t.Fatalf("a bothFail key must never be re-probed")
	}
}

// --- Negative-evidence discipline (Major-6): NoEvidence never writes ---

func TestNoEvidenceOutcomeNeverWritesMemoState(t *testing.T) {
	m, _ := newTestMemo()
	k := testKey("A")

	m.Observe(k, RouteA, OutcomeResourceFailure)
	m.Observe(k, RouteA, OutcomeResourceFailure)
	release, ok := m.BeginProbe(k)
	if !ok {
		t.Fatalf("expected probe eligibility")
	}
	// A route-B failure that is NOT a resource failure (e.g. a solver
	// timeout classified upstream as NoEvidence) must not flip the entry to
	// bothFail.
	m.Observe(k, RouteB, OutcomeNoEvidence)
	release()

	state, _ := m.Lookup(k)
	if state != Unknown {
		t.Fatalf("Lookup(k) = %v after a NoEvidence probe outcome, want Unknown (still probe-eligible)", state)
	}
	if _, ok := m.BeginProbe(k); !ok {
		t.Fatalf("a NoEvidence probe outcome must leave the key probe-eligible for a future attempt")
	}
}

// --- TTL / re-validation (Major-5) ---

func TestTTLExpiresFromCreationNotFromLookup(t *testing.T) {
	m, clk := newTestMemo()
	k := testKey("A")

	m.Observe(k, RouteA, OutcomeResourceFailure)
	m.Observe(k, RouteA, OutcomeResourceFailure)
	release, _ := m.BeginProbe(k)
	m.Observe(k, RouteB, OutcomeSuccess)
	release()

	// Repeated lookups before the TTL must NOT refresh the clock.
	clk.advance(memoEntryTTL - time.Second)
	for i := 0; i < 5; i++ {
		if state, _ := m.Lookup(k); state != PreferB {
			t.Fatalf("expected PreferB to still be live just under the TTL, got %v", state)
		}
	}

	clk.advance(2 * time.Second) // now past memoEntryTTL from creation
	if state, _ := m.Lookup(k); state != Unknown {
		t.Fatalf("expected the entry to have expired at memoEntryTTL from CREATION, got %v", state)
	}
}

func TestReValidationAtMidpointSuccessDropsEntry(t *testing.T) {
	m, clk := newTestMemo()
	k := testKey("A")

	m.Observe(k, RouteA, OutcomeResourceFailure)
	m.Observe(k, RouteA, OutcomeResourceFailure)
	release, _ := m.BeginProbe(k)
	m.Observe(k, RouteB, OutcomeSuccess)
	release()

	clk.advance(memoEntryTTL/reValidationFraction + time.Second)
	state, stale := m.Lookup(k)
	if state != PreferB || !stale {
		t.Fatalf("Lookup(k) at the re-validation midpoint = (%v, stale=%v), want (PreferB, stale=true)", state, stale)
	}

	// Per the documented protocol: a stale verdict routes the NEXT request
	// through A instead of memo-hitting; A succeeding drops the entry.
	m.Observe(k, RouteA, OutcomeSuccess)
	if state, _ := m.Lookup(k); state != Unknown {
		t.Fatalf("expected route-A success on a stale entry to drop it, got %v", state)
	}
}

// TestReValidationAtMidpointFailureRefreshesVerdictState pins the plain
// Observe(RouteA, ResourceFailure) call's STATE-TRANSITION behavior on a
// stale PreferB entry: it stays PreferB and its clock refreshes. This is
// unconditionally correct on its own and is unaffected by the atomic
// record-and-admit method (ObserveRouteAFailureAndMaybeBeginProbe, see
// TestReValidationRescue_* below) — that method reuses this exact same
// state transition internally. What this test does NOT cover is whether a
// caller gets a RESCUE dispatch for the request that produced this
// failure; see TestObserveThenBeginProbeSeparately_MissesStaleRescue and
// TestReValidationRescue_AdmitsStalePreferBFailure for that half.
func TestReValidationAtMidpointFailureRefreshesVerdictState(t *testing.T) {
	m, clk := newTestMemo()
	k := testKey("A")

	m.Observe(k, RouteA, OutcomeResourceFailure)
	m.Observe(k, RouteA, OutcomeResourceFailure)
	release, _ := m.BeginProbe(k)
	m.Observe(k, RouteB, OutcomeSuccess)
	release()

	clk.advance(memoEntryTTL/reValidationFraction + time.Second)
	if state, stale := m.Lookup(k); state != PreferB || !stale {
		t.Fatalf("expected stale PreferB at the midpoint, got (%v, %v)", state, stale)
	}

	m.Observe(k, RouteA, OutcomeResourceFailure)
	if state, stale := m.Lookup(k); state != PreferB || stale {
		t.Fatalf("expected the entry refreshed to non-stale PreferB, got (%v, stale=%v)", state, stale)
	}

	// And the refreshed clock means it survives well past the ORIGINAL TTL.
	clk.advance(memoEntryTTL - 2*time.Second)
	if state, _ := m.Lookup(k); state != PreferB {
		t.Fatalf("expected the refreshed entry to still be live, got %v", state)
	}
}

// TestObserveThenBeginProbeSeparately_MissesStaleRescue documents the exact
// ordering hazard ObserveRouteAFailureAndMaybeBeginProbe exists to close: a
// caller that observes and admits as two SEPARATE calls can never rescue a
// stale-PreferB re-validation failure, because Observe's own refresh of
// createdAt un-stales the entry before BeginProbe (Unknown-only) ever looks
// at it. This is a guard against "simplifying" the engine's call site back
// to the two-call form — if this test ever starts passing with ok=true, the
// atomic method has stopped being necessary or something has regressed the
// separate calls' documented behavior.
func TestObserveThenBeginProbeSeparately_MissesStaleRescue(t *testing.T) {
	m, clk := newTestMemo()
	k := testKey("A")

	m.Observe(k, RouteA, OutcomeResourceFailure)
	m.Observe(k, RouteA, OutcomeResourceFailure)
	release, _ := m.BeginProbe(k)
	m.Observe(k, RouteB, OutcomeSuccess)
	release()

	clk.advance(memoEntryTTL/reValidationFraction + time.Second)
	if state, stale := m.Lookup(k); state != PreferB || !stale {
		t.Fatalf("expected stale PreferB at the midpoint, got (%v, %v)", state, stale)
	}

	// The two-call sequence: Observe refreshes (un-stales) the entry, THEN
	// BeginProbe looks — and refuses, because state is PreferB, not Unknown.
	m.Observe(k, RouteA, OutcomeResourceFailure)
	_, ok := m.BeginProbe(k)
	if ok {
		t.Fatal("BeginProbe admitted after a separate Observe call — the ordering hazard this test documents no longer reproduces; if the two-call form is now safe, ObserveRouteAFailureAndMaybeBeginProbe may be unnecessary and this test's premise should be revisited")
	}
}

// TestReValidationRescue_AdmitsStalePreferBFailure pins the actual fix: the
// atomic method rescues the request that triggers a stale PreferB entry's
// re-validation, using the SAME setup as
// TestObserveThenBeginProbeSeparately_MissesStaleRescue to make the
// before/after contrast direct.
func TestReValidationRescue_AdmitsStalePreferBFailure(t *testing.T) {
	m, clk := newTestMemo()
	k := testKey("A")

	m.Observe(k, RouteA, OutcomeResourceFailure)
	m.Observe(k, RouteA, OutcomeResourceFailure)
	release, _ := m.BeginProbe(k)
	m.Observe(k, RouteB, OutcomeSuccess)
	release()

	clk.advance(memoEntryTTL/reValidationFraction + time.Second)
	if state, stale := m.Lookup(k); state != PreferB || !stale {
		t.Fatalf("expected stale PreferB at the midpoint, got (%v, %v)", state, stale)
	}

	rescueRelease, ok := m.ObserveRouteAFailureAndMaybeBeginProbe(k)
	if !ok {
		t.Fatal("ObserveRouteAFailureAndMaybeBeginProbe did not rescue a stale PreferB entry's re-validation failure")
	}
	rescueRelease()

	// The rescue also performed the same state refresh the plain Observe
	// call does — still PreferB, no longer stale.
	if state, stale := m.Lookup(k); state != PreferB || stale {
		t.Fatalf("state after rescue = (%v, stale=%v), want (PreferB, stale=false)", state, stale)
	}
}

// TestReValidationRescue_FreshPreferBNotAdmitted defensively pins the Memo
// API's own correctness independent of caller discipline: a PreferB entry
// that has NOT crossed the re-validation midpoint must not be rescued, even
// if something calls the atomic method for it (the real engine call sites
// only ever reach this method after Lookup already reported non-fresh, but
// the Memo API itself should not rely on that).
func TestReValidationRescue_FreshPreferBNotAdmitted(t *testing.T) {
	m, _ := newTestMemo()
	k := testKey("A")

	m.Observe(k, RouteA, OutcomeResourceFailure)
	m.Observe(k, RouteA, OutcomeResourceFailure)
	release, _ := m.BeginProbe(k)
	m.Observe(k, RouteB, OutcomeSuccess)
	release()

	if state, stale := m.Lookup(k); state != PreferB || stale {
		t.Fatalf("expected a fresh (non-stale) PreferB entry, got (%v, stale=%v)", state, stale)
	}

	_, ok := m.ObserveRouteAFailureAndMaybeBeginProbe(k)
	if ok {
		t.Fatal("rescued a FRESH PreferB entry's failure — re-probing an already-trusted, non-stale verdict is unnecessary and wasteful")
	}
}

// TestReValidationRescue_UnknownKeyCorroborationStillWorks pins that the
// atomic method preserves the ORIGINAL first-probe contract for a fresh
// Unknown key — the case ObserveRouteAFailureAndMaybeBeginProbe replaces
// Observe+BeginProbe for, not just the new stale-rescue case.
func TestReValidationRescue_UnknownKeyCorroborationStillWorks(t *testing.T) {
	m, _ := newTestMemo()
	k := testKey("A")

	release1, ok1 := m.ObserveRouteAFailureAndMaybeBeginProbe(k)
	if ok1 {
		t.Fatal("admitted on the FIRST failure — corroboration requires more than one")
	}
	if release1 != nil {
		release1()
	}

	release2, ok2 := m.ObserveRouteAFailureAndMaybeBeginProbe(k)
	if !ok2 {
		t.Fatal("did not admit on the 2nd consecutive failure (minCorroboratingFailures)")
	}
	release2()
}

func TestBothFailHasNoReValidationAndExpiresAtPlainTTL(t *testing.T) {
	m, clk := newTestMemo()
	k := testKey("A")

	m.Observe(k, RouteA, OutcomeResourceFailure)
	m.Observe(k, RouteA, OutcomeResourceFailure)
	release, _ := m.BeginProbe(k)
	m.Observe(k, RouteB, OutcomeResourceFailure)
	release()

	clk.advance(memoEntryTTL/reValidationFraction + time.Second)
	// No re-validation staleness concept applies to bothFail.
	if state, stale := m.Lookup(k); state != BothFail || stale {
		t.Fatalf("bothFail at the PreferB re-validation midpoint = (%v, stale=%v), want (BothFail, false)", state, stale)
	}

	clk.advance(memoEntryTTL / reValidationFraction)
	if state, _ := m.Lookup(k); state != Unknown {
		t.Fatalf("expected bothFail to expire at plain memoEntryTTL, got %v", state)
	}
}

// --- Bounded LRU eviction ---

// fillDistinctPreferB writes n distinct PreferB entries directly via a
// route-B success Observe — bypassing the route-A corroboration/pressure
// machinery entirely (a route-B Success is unconditional evidence,
// unaffected by the pressure damper), so these LRU tests exercise eviction
// in isolation rather than interacting with pressure-window bookkeeping.
func fillDistinctPreferB(m *Memo, n int, keyAt func(i int) Key) {
	for i := 0; i < n; i++ {
		m.Observe(keyAt(i), RouteB, OutcomeSuccess)
	}
}

func TestLRUEvictionBoundsMemoSize(t *testing.T) {
	m, _ := newTestMemo()

	fillDistinctPreferB(m, memoMaxEntries+10, func(i int) Key { return Key{RootKind: testKeyName(i)} })

	stats := m.Stats()
	if stats.Entries > memoMaxEntries {
		t.Fatalf("memo grew to %d entries, want <= %d", stats.Entries, memoMaxEntries)
	}
	if stats.Entries == 0 {
		t.Fatalf("memo evicted everything; want the most recent memoMaxEntries to survive")
	}
}

func TestLRUEvictsLeastRecentlyTouchedFirst(t *testing.T) {
	m, _ := newTestMemo()

	first := testKey("oldest")
	m.Observe(first, RouteB, OutcomeSuccess)

	// Fill the memo past capacity with distinct keys, never touching
	// `first` again.
	fillDistinctPreferB(m, memoMaxEntries, func(i int) Key { return Key{RootKind: testKeyName(i)} })

	m.mu.Lock()
	_, stillPresent := m.entries[first]
	m.mu.Unlock()
	if stillPresent {
		t.Fatalf("expected the least-recently-touched key to have been evicted first")
	}
}

func testKeyName(i int) string {
	// Deterministic, distinct closed-vocabulary-shaped tag per iteration —
	// not a real node-kind string, just a unique bucket for the test.
	digits := make([]byte, 0, 8)
	if i == 0 {
		digits = append(digits, '0')
	}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return "*fixture.Node#" + string(digits)
}

// --- Cluster-wide pressure damper (Critical-3) ---

func TestPressureDamperSuppressesBothActionAndLearning(t *testing.T) {
	m, _ := newTestMemo()

	// Drive enough DISTINCT keys into resource failure to cross the
	// pressure threshold. None of them individually reaches corroboration.
	for i := 0; i < pressureFailureThreshold+1; i++ {
		k := Key{RootKind: testKeyName(i)}
		m.Observe(k, RouteA, OutcomeResourceFailure)
	}

	if !m.UnderPressure() {
		t.Fatalf("expected UnderPressure() once more than pressureFailureThreshold distinct keys failed within the window")
	}

	// While under pressure: no probe is admitted even for an
	// otherwise-eligible key, and no write happens.
	hot := testKey("hot")
	m.Observe(hot, RouteA, OutcomeResourceFailure)
	m.Observe(hot, RouteA, OutcomeResourceFailure)
	if _, ok := m.BeginProbe(hot); ok {
		t.Fatalf("BeginProbe must be refused while UnderPressure()")
	}

	m.mu.Lock()
	_, wrote := m.entries[hot]
	m.mu.Unlock()
	if wrote {
		t.Fatalf("Observe must not write memo state for `hot` while UnderPressure()")
	}
}

func TestPressureDamperDecaysAfterWindow(t *testing.T) {
	m, clk := newTestMemo()

	for i := 0; i < pressureFailureThreshold+1; i++ {
		k := Key{RootKind: testKeyName(i)}
		m.Observe(k, RouteA, OutcomeResourceFailure)
	}
	if !m.UnderPressure() {
		t.Fatalf("expected UnderPressure() immediately after the burst")
	}

	clk.advance(testPressureWindow + time.Second)
	if m.UnderPressure() {
		t.Fatalf("expected the pressure window to have decayed")
	}
}

// --- Admission budget covers probe, retry, AND memo-hit alike (Critical-2) ---

func TestAdmissionBudgetCapsConcurrentDispatchesAcrossProbeAndHit(t *testing.T) {
	m, _ := newTestMemo()

	var releases []func()
	for i := 0; i < maxConcurrentRoutedDispatches; i++ {
		release, ok := m.AdmitDispatch()
		if !ok {
			t.Fatalf("expected admission %d/%d to succeed", i+1, maxConcurrentRoutedDispatches)
		}
		releases = append(releases, release)
	}

	if _, ok := m.AdmitDispatch(); ok {
		t.Fatalf("expected admission to be denied once the budget is exhausted")
	}

	releases[0]()
	if _, ok := m.AdmitDispatch(); !ok {
		t.Fatalf("expected admission to succeed again after a release")
	}
}

// --- Concurrent read/write safety ---

func TestConcurrentLookupObserveAdmitDispatchIsRace_free(t *testing.T) {
	m, _ := newTestMemo()
	keys := make([]Key, 8)
	for i := range keys {
		keys[i] = Key{RootKind: testKeyName(i)}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	worker := func(seed int) {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			k := keys[(seed+i)%len(keys)]
			switch i % 4 {
			case 0:
				m.Observe(k, RouteA, OutcomeResourceFailure)
			case 1:
				m.Lookup(k)
			case 2:
				if release, ok := m.AdmitDispatch(); ok {
					release()
				}
			case 3:
				if release, ok := m.BeginProbe(k); ok {
					m.Observe(k, RouteB, OutcomeSuccess)
					release()
				}
			}
		}
	}

	const workers = 16
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go worker(w)
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

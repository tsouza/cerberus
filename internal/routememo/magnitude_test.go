package routememo

import (
	"testing"
	"time"
)

func TestRecordActualMagnitude_NoOpWithoutLiveEntry(t *testing.T) {
	m := New(testPressureWindow)
	k := testKey("agg")

	// No entry at all yet.
	m.RecordActualMagnitude(k, 1000)
	if _, _, ok := m.MagnitudeFor(k); ok {
		t.Fatal("RecordActualMagnitude must not create an entry")
	}
}

func TestRecordActualMagnitude_UpdatesLiveEntryOnly(t *testing.T) {
	m := New(testPressureWindow)
	k := testKey("agg")

	// Establish a live PreferB entry the normal way.
	m.Observe(k, RouteB, OutcomeSuccess)
	if state, _ := m.Lookup(k); state != PreferB {
		t.Fatalf("expected PreferB after a route-B success, got %v", state)
	}

	m.RecordActualMagnitude(k, 500)
	rows, obs, ok := m.MagnitudeFor(k)
	if !ok {
		t.Fatal("expected a magnitude reading on a live entry")
	}
	if rows != 500 || obs != 1 {
		t.Fatalf("expected (500, 1), got (%v, %d)", rows, obs)
	}

	// A second, wildly different observation moves the EMA but does not
	// jump straight to it (bounded influence — magnitudeEMAAlpha < 1).
	m.RecordActualMagnitude(k, 50_000)
	rows, obs, ok = m.MagnitudeFor(k)
	if !ok {
		t.Fatal("expected a magnitude reading after the second observation")
	}
	if obs != 2 {
		t.Fatalf("expected 2 observations, got %d", obs)
	}
	wantEMA := 500 + magnitudeEMAAlpha*(50_000-500)
	if rows != wantEMA {
		t.Fatalf("expected the bounded EMA step to %v, got %v", wantEMA, rows)
	}
	if rows >= 50_000 {
		t.Fatalf("a single new observation must not fully move the EMA, got %v", rows)
	}
}

func TestRecordActualMagnitude_NeverChangesRoutingState(t *testing.T) {
	m := New(testPressureWindow)
	k := testKey("agg")
	m.Observe(k, RouteB, OutcomeSuccess)

	stateBefore, staleBefore := m.Lookup(k)
	statsBefore := m.Stats()

	m.RecordActualMagnitude(k, 1)
	m.RecordActualMagnitude(k, 1_000_000)

	stateAfter, staleAfter := m.Lookup(k)
	statsAfter := m.Stats()

	if stateBefore != stateAfter || staleBefore != staleAfter {
		t.Fatalf("RecordActualMagnitude must never change LookupState: before=(%v,%v) after=(%v,%v)",
			stateBefore, staleBefore, stateAfter, staleAfter)
	}
	if statsBefore != statsAfter {
		t.Fatalf("RecordActualMagnitude must never change Stats: before=%+v after=%+v", statsBefore, statsAfter)
	}
}

// TestRecordActualMagnitude_SurvivesRouteBLockUnlockCycle pins that the
// magnitude axis is carried across every Verdict replacement
// observeRouteBLocked performs: a route-B resource failure demoting
// PreferB to Unknown, the second consecutive failure locking out to
// BothFail, and the route-B success that re-promotes to PreferB. Before
// inheritMagnitude, each of those replaced the Verdict wholesale and
// silently zeroed the EMA, leaving the engine's trivialRevalidation gate
// (which needs MinCorroboratingFailures readings) permanently short on
// exactly the churny keys it exists for.
func TestRecordActualMagnitude_SurvivesRouteBLockUnlockCycle(t *testing.T) {
	m := New(testPressureWindow)
	k := testKey("agg")

	m.Observe(k, RouteB, OutcomeSuccess)
	m.RecordActualMagnitude(k, 100)
	m.RecordActualMagnitude(k, 100)
	wantRows, wantObs, ok := m.MagnitudeFor(k)
	if !ok || wantObs != 2 {
		t.Fatalf("setup: expected 2 readings on the live PreferB entry, got (%v, %d, %v)", wantRows, wantObs, ok)
	}

	assertCarried := func(step string, wantState LookupState) {
		t.Helper()
		if state, _ := m.Lookup(k); state != wantState {
			t.Fatalf("%s: expected LookupState %v, got %v", step, wantState, state)
		}
		rows, obs, ok := m.MagnitudeFor(k)
		if !ok || rows != wantRows || obs != wantObs {
			t.Fatalf("%s: magnitude must survive the Verdict replacement, want (%v, %d, true) got (%v, %d, %v)",
				step, wantRows, wantObs, rows, obs, ok)
		}
	}

	// Lock: a first route-B failure demotes PreferB to Unknown (carrying
	// corroboration), replacing the Verdict.
	m.Observe(k, RouteB, OutcomeResourceFailure)
	assertCarried("first route-B failure (PreferB -> Unknown)", Unknown)

	// Unlock: a route-B success re-promotes, replacing the Verdict again.
	m.Observe(k, RouteB, OutcomeSuccess)
	assertCarried("route-B success (Unknown -> PreferB)", PreferB)

	// Lock-out: MinCorroboratingFailures consecutive route-B failures reach
	// BothFail through one more replacement each.
	for i := 0; i < MinCorroboratingFailures; i++ {
		m.Observe(k, RouteB, OutcomeResourceFailure)
	}
	assertCarried("consecutive route-B failures (-> BothFail)", BothFail)
}

// TestRecordActualMagnitude_ClearedWithTheEntry pins the other half of the
// contract: the magnitude is a property of the ENTRY, so the transitions
// that delete the entry outright — a route-A success contradicting the
// verdict, and TTL expiry — take the magnitude with them, and a fresh entry
// created afterwards starts with no reading.
func TestRecordActualMagnitude_ClearedWithTheEntry(t *testing.T) {
	t.Run("route-A success deletes the entry", func(t *testing.T) {
		m := New(testPressureWindow)
		k := testKey("agg")
		m.Observe(k, RouteB, OutcomeSuccess)
		m.RecordActualMagnitude(k, 100)

		m.Observe(k, RouteA, OutcomeSuccess)
		if _, _, ok := m.MagnitudeFor(k); ok {
			t.Fatal("a route-A success deletes the entry; the magnitude must go with it")
		}
		m.Observe(k, RouteB, OutcomeSuccess)
		if _, _, ok := m.MagnitudeFor(k); ok {
			t.Fatal("a fresh entry after deletion must start with no reading")
		}
	})

	t.Run("TTL expiry", func(t *testing.T) {
		m, clk := newTestMemo()
		k := testKey("agg")
		m.Observe(k, RouteB, OutcomeSuccess)
		m.RecordActualMagnitude(k, 100)

		clk.advance(m.entryTTL)
		if _, _, ok := m.MagnitudeFor(k); ok {
			t.Fatal("an expired entry must report no magnitude")
		}
		m.Observe(k, RouteB, OutcomeSuccess)
		if _, _, ok := m.MagnitudeFor(k); ok {
			t.Fatal("a fresh entry after expiry must start with no reading")
		}
	})
}

func TestMagnitudeFor_NoReadingIsOK(t *testing.T) {
	m := New(testPressureWindow)
	k := testKey("agg")
	m.Observe(k, RouteB, OutcomeSuccess)

	if _, _, ok := m.MagnitudeFor(k); ok {
		t.Fatal("expected MagnitudeFor to report ok=false before any magnitude is ever recorded")
	}
}

func TestRecordActualMagnitude_StampsObservedAt(t *testing.T) {
	m := New(testPressureWindow)
	k := testKey("agg")
	now := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	m.SetNowForTest(func() time.Time { return now })

	m.Observe(k, RouteB, OutcomeSuccess)
	m.RecordActualMagnitude(k, 42)
	if got := m.magnitudeObservedAtFor(k); !got.Equal(now) {
		t.Fatalf("expected magnitudeObservedAt to be stamped to %v, got %v", now, got)
	}
}

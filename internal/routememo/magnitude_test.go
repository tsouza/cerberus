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

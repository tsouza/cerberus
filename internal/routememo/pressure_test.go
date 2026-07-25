package routememo

import (
	"testing"
	"time"
)

func TestPressureTrackerCountsDistinctKeysOnly(t *testing.T) {
	p := newPressureTracker()
	now := time.Unix(1_700_000_000, 0)

	p.record(Key{RootKind: "a"}, now)
	p.record(Key{RootKind: "a"}, now) // same key twice — still one distinct key
	p.record(Key{RootKind: "b"}, now)

	if got := p.countFresh(now, time.Minute); got != 2 {
		t.Fatalf("countFresh = %d, want 2 distinct keys", got)
	}
}

// TestPressureTrackerBoundedUnderDistinctKeyBurst pins the size-cap backstop:
// a burst of distinct keys arriving faster than they can age out of the
// window must not grow the tracker without bound. Every record call in this
// test lands inside the window, so time-based pruning alone cannot cap the
// resident size — only the pressureTrackerMaxEntries backstop can.
func TestPressureTrackerBoundedUnderDistinctKeyBurst(t *testing.T) {
	p := newPressureTracker()
	base := time.Unix(1_700_000_000, 0)

	for i := 0; i < pressureTrackerMaxEntries*4; i++ {
		p.record(Key{AnchorLg: i, FanoutLg: i / 256}, base)
	}

	if got := len(p.lastFailure); got > pressureTrackerMaxEntries {
		t.Fatalf("resident tracker size = %d after a burst of %d distinct keys, want <= %d (pressureTrackerMaxEntries)",
			got, pressureTrackerMaxEntries*4, pressureTrackerMaxEntries)
	}
	if got, want := p.order.Len(), len(p.lastFailure); got != want {
		t.Fatalf("order list len = %d, map len = %d — eviction left the two out of sync", got, want)
	}
	if got, want := len(p.index), len(p.lastFailure); got != want {
		t.Fatalf("index map len = %d, lastFailure map len = %d — eviction left the two out of sync", got, want)
	}
}

func TestPressureTrackerPrunesStaleEntries(t *testing.T) {
	p := newPressureTracker()
	base := time.Unix(1_700_000_000, 0)

	p.record(Key{RootKind: "a"}, base)
	p.record(Key{RootKind: "b"}, base.Add(30*time.Second))

	// At base+59s, both entries are still inside a 1-minute window.
	if got := p.countFresh(base.Add(59*time.Second), time.Minute); got != 2 {
		t.Fatalf("countFresh at +59s = %d, want 2", got)
	}

	// At base+61s, "a" (recorded at base) has aged out; "b" has not.
	if got := p.countFresh(base.Add(61*time.Second), time.Minute); got != 1 {
		t.Fatalf("countFresh at +61s = %d, want 1 (only %q still fresh)", got, "b")
	}
}

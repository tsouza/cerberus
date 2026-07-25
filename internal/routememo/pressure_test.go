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

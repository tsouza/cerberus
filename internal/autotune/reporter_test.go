package autotune

import "testing"

func TestReporter_SetSnapshot(t *testing.T) {
	r := NewReporter(Status{Enabled: true, Reason: ReasonStatusActive})

	if s := r.Snapshot(); !s.Enabled || s.Reason != ReasonStatusActive {
		t.Fatalf("initial snapshot = %+v", s)
	}

	r.Set(Status{Ticks: 5})
	if s := r.Snapshot(); s.Ticks != 5 || s.Enabled {
		t.Fatalf("post-set snapshot = %+v", s)
	}

	// Snapshot returns a copy: mutating it must not affect the stored value.
	s := r.Snapshot()
	s.Ticks = 99
	if got := r.Snapshot().Ticks; got != 5 {
		t.Errorf("Snapshot leaked a live value: Ticks = %d, want 5", got)
	}
}

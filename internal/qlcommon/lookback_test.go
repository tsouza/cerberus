package qlcommon

import (
	"testing"
	"time"
)

// TestInstantLookbackMatchesPromDefault pins the exact staleness window
// InstantLookback exposes to its three consumers. A drift here silently
// changes how far back every instant-vector selector, every subquery, and
// Loki's instant-query handler look for the latest sample per series.
func TestInstantLookbackMatchesPromDefault(t *testing.T) {
	t.Parallel()

	if InstantLookback != 5*time.Minute {
		t.Fatalf("InstantLookback = %v, want %v", InstantLookback, 5*time.Minute)
	}
}

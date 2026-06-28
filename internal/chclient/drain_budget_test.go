package chclient

import (
	"errors"
	"testing"
)

// TestDrainBudgetExceeded pins the metadata-drain sample budget: the matrix
// path bounds Go-side buffering through the cursor's SampleBudget, and the
// metadata drains (QueryStrings / QueryLabelSets / QueryMetricMeta /
// QueryIndexVolume / ...) now share that bound via drainBudgetExceeded — so a
// high-cardinality DISTINCT over a wide window aborts with ErrTooManySamples
// instead of OOMing the process.
func TestDrainBudgetExceeded(t *testing.T) {
	t.Parallel()

	c := &Client{maxSamples: 3}
	if err := c.drainBudgetExceeded(3); err != nil {
		t.Fatalf("at the limit (3): want nil, got %v", err)
	}
	err := c.drainBudgetExceeded(4)
	if !errors.Is(err, ErrTooManySamples) {
		t.Fatalf("over the limit (4): want ErrTooManySamples, got %v", err)
	}
	var tms *TooManySamplesError
	if !errors.As(err, &tms) || tms.Limit != 3 {
		t.Fatalf("want TooManySamplesError{Limit:3}, got %v", err)
	}

	// 0 disables the budget — no rejection at any count.
	disabled := &Client{maxSamples: 0}
	if err := disabled.drainBudgetExceeded(1_000_000); err != nil {
		t.Fatalf("disabled budget: want nil, got %v", err)
	}
}

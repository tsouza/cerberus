package chplan

import "testing"

// TestIsClassicBucketBudgetMessage pins the classic-histogram stage's guard
// messages as the only ones IsClassicBucketBudgetMessage recognises: the
// merge and per-series window guards, and neither another guard's message,
// the memory-limit message the head answers with, nor a prefix of a guard
// message.
func TestIsClassicBucketBudgetMessage(t *testing.T) {
	t.Parallel()
	for msg, want := range map[string]bool{
		ClassicBucketMergeBudgetMessage:                                          true,
		ClassicBucketWindowSeriesBudgetMessage:                                   true,
		HistogramMergeBudgetMessage:                                              false,
		ClassicHistogramMemoryLimitMessage:                                       false,
		ClassicBucketMergeBudgetMessage[:len(ClassicBucketMergeBudgetMessage)-1]: false,
		"": false,
	} {
		if got := IsClassicBucketBudgetMessage(msg); got != want {
			t.Errorf("IsClassicBucketBudgetMessage(%q) = %v, want %v", msg, got, want)
		}
	}
}

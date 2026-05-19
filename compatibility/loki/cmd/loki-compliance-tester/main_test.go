package main

import "testing"

func TestScoreCounts_ExcludesOverlaySkips(t *testing.T) {
	t.Parallel()
	results := []Result{
		{},                                   // pass
		{},                                   // pass
		{Diff: "x"},                          // diff
		{UnexpectedFailure: "y"},             // unexpected failure
		{UnexpectedSuccess: true},            // unexpected success
		{SkipReason: "documented exclusion"}, // excluded — outside total
		{SkipReason: "harness limit"},        // excluded — outside total
	}
	passed, total := scoreCounts(results)
	if total != 5 {
		t.Fatalf("total = %d, want 5 (excluded cases not counted)", total)
	}
	if passed != 2 {
		t.Fatalf("passed = %d, want 2", passed)
	}
}

func TestScoreCounts_AllPassing(t *testing.T) {
	t.Parallel()
	results := []Result{{}, {}, {}}
	passed, total := scoreCounts(results)
	if passed != 3 || total != 3 {
		t.Fatalf("(passed, total) = (%d, %d), want (3, 3)", passed, total)
	}
}

func TestScoreCounts_AllSkipped(t *testing.T) {
	t.Parallel()
	results := []Result{
		{SkipReason: "a"},
		{SkipReason: "b"},
	}
	passed, total := scoreCounts(results)
	if passed != 0 || total != 0 {
		t.Fatalf("(passed, total) = (%d, %d), want (0, 0) when every case is excluded", passed, total)
	}
}

func TestScoreCounts_Empty(t *testing.T) {
	t.Parallel()
	passed, total := scoreCounts(nil)
	if passed != 0 || total != 0 {
		t.Fatalf("(passed, total) = (%d, %d), want (0, 0)", passed, total)
	}
}

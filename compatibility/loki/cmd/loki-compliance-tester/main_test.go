package main

import (
	"testing"

	bench "github.com/tsouza/cerberus/compatibility/loki/upstream/loki-bench"
)

// TestIsExpectedEmptyCase pins the corpus-tag contract that powers
// `compareOne`'s empty-result branch. A drift in the upstream YAML tag
// vocabulary (e.g. renaming the tag) would silently flip the failing
// `fast/basic-selectors.yaml#Log query with impossible filter` case
// back into a `baseline returned empty` row, so we anchor the predicate
// against the tag literal here.
func TestIsExpectedEmptyCase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		tags []string
		want bool
	}{
		{"tagged-empty-result", []string{"line-filter", "empty-result", "cache-test"}, true},
		{"tag-set-without-empty", []string{"line-filter", "regex"}, false},
		{"nil-tags", nil, false},
		{"empty-tags", []string{}, false},
		{"only-empty-result", []string{"empty-result"}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isExpectedEmptyCase(bench.TestCase{Tags: tc.tags})
			if got != tc.want {
				t.Fatalf("isExpectedEmptyCase(tags=%v) = %v, want %v", tc.tags, got, tc.want)
			}
		})
	}
}

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

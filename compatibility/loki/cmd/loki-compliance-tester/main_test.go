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

// TestScoreCounts asserts the score denominator includes every case
// the driver attempted, with the numerator being only the cases that
// passed. There is no allow-list / overlay exclusion: the harness
// carries no `should_skip` consumer code.
func TestScoreCounts(t *testing.T) {
	t.Parallel()
	results := []Result{
		{},                        // pass
		{},                        // pass
		{Diff: "x"},               // diff
		{UnexpectedFailure: "y"},  // unexpected failure
		{UnexpectedSuccess: true}, // unexpected success
	}
	passed, total := scoreCounts(results)
	if total != 5 {
		t.Fatalf("total = %d, want 5 (every attempted case counts)", total)
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

func TestScoreCounts_Empty(t *testing.T) {
	t.Parallel()
	passed, total := scoreCounts(nil)
	if passed != 0 || total != 0 {
		t.Fatalf("(passed, total) = (%d, %d), want (0, 0)", passed, total)
	}
}

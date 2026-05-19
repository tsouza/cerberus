package main

import (
	"testing"
)

func TestReportResult_Passed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		r    reportResult
		want bool
	}{
		{"all clean", reportResult{}, true},
		{"diff non-empty", reportResult{Diff: "vector length differs"}, false},
		{"unexpected failure", reportResult{UnexpectedFailure: "status=500"}, false},
		{"unexpected success", reportResult{UnexpectedSuccess: true}, false},
		{"diff + unexpected failure", reportResult{Diff: "x", UnexpectedFailure: "y"}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.r.passed(); got != tc.want {
				t.Fatalf("passed() = %v, want %v (r=%+v)", got, tc.want, tc.r)
			}
		})
	}
}

func TestTally(t *testing.T) {
	t.Parallel()
	results := []reportResult{
		{},                                   // pass
		{},                                   // pass
		{Diff: "x"},                          // fail
		{UnexpectedFailure: "y"},             // fail
		{UnexpectedSuccess: true},            // fail
		{Diff: "x", UnexpectedSuccess: true}, // fail (single fail counts once)
	}
	passed, total := tally(results)
	if total != 6 {
		t.Fatalf("total = %d, want 6", total)
	}
	if passed != 2 {
		t.Fatalf("passed = %d, want 2", passed)
	}
}

func TestTally_EmptyReport(t *testing.T) {
	t.Parallel()
	passed, total := tally(nil)
	if passed != 0 || total != 0 {
		t.Fatalf("expected (0, 0), got (%d, %d)", passed, total)
	}
}

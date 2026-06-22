//go:build chdb

package chclienttest

import (
	"strings"
	"testing"
)

// TestBoundsDrainViolation_Predicate pins the three rejections that make the
// O(output)-not-O(input) principle falsifiable, exercising the pure predicate
// every RunBoundsDrain row reasons over. This is a UNIT test of the predicate,
// not a stand-in for a handler test: it proves the gate's arithmetic rejects an
// unbounded drain (so a real behavioural row — e.g. the PromQL query_range row
// in api/prom — cannot pass on the OOM bug it guards) and accepts a bounded
// one. The behavioural proof that a handler's measured drain actually feeds
// these numbers lives in the harness rows themselves.
func TestBoundsDrainViolation_Predicate(t *testing.T) {
	// Shared scale: a bound well below the full match set, so the degeneracy
	// guard is satisfied and bounded vs unbounded are distinguishable — the
	// exact configuration the two production OOMs lacked.
	const (
		outputBound = 100
		fullSeed    = 600 // 6× the bound, mirroring the PromQL row's density axis
	)

	cases := []struct {
		name     string
		drain    int64
		bound    int64
		fullSeed int64
		wantOK   bool
		wantWhy  string // substring the rejection reason must contain
	}{
		{
			// The fixed handler: drains exactly its output bound.
			name:  "bounded drain accepted",
			drain: outputBound,
			bound: outputBound, fullSeed: fullSeed,
			wantOK: true,
		},
		{
			// Falsifiability: the neutered handler drains the full match set.
			// The predicate MUST reject it, or the gate cannot fail on the bug.
			name:  "unbounded drain rejected",
			drain: fullSeed,
			bound: outputBound, fullSeed: fullSeed,
			wantOK:  false,
			wantWhy: "buffered O(input)",
		},
		{
			// A drain at the fudge ceiling passes; one row past it fails — the
			// fudge admits a small constant factor, not an O(input) blow-up.
			name:  "drain at fudge ceiling accepted",
			drain: outputBound * boundsDrainFudge,
			bound: outputBound, fullSeed: fullSeed,
			wantOK: true,
		},
		{
			name:  "drain past fudge ceiling rejected",
			drain: outputBound*boundsDrainFudge + 1,
			bound: outputBound, fullSeed: fullSeed,
			wantOK:  false,
			wantWhy: "exceeds bound",
		},
		{
			// Degeneracy guard: a seed that does not exceed the bound makes a
			// bounded and an unbounded handler indistinguishable — itself a
			// violation, regardless of the drain count.
			name:  "degenerate seed rejected",
			drain: outputBound,
			bound: outputBound, fullSeed: outputBound,
			wantOK:  false,
			wantWhy: "fullSeed",
		},
		{
			// A "bound" that keeps the whole match set is no bound: a drain
			// within the fudge ceiling but not below fullSeed must still fail.
			// Requires fullSeed > bound (pass the degeneracy guard) and
			// drain in [fullSeed, bound*fudge], so the drain>=fullSeed branch is
			// the one that fires: bound=100, fullSeed=150, drain=150 (≤ 200).
			name:  "no reduction below full seed rejected",
			drain: outputBound + outputBound/2,                        // 150
			bound: outputBound, fullSeed: outputBound + outputBound/2, // fullSeed 150 > bound 100
			wantOK:  false,
			wantWhy: "did not reduce below full seed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := BoundsDrainViolation(tc.drain, tc.bound, tc.fullSeed)
			if ok != tc.wantOK {
				t.Fatalf("BoundsDrainViolation(drain=%d, bound=%d, fullSeed=%d) ok=%v, want %v (reason=%q)",
					tc.drain, tc.bound, tc.fullSeed, ok, tc.wantOK, reason)
			}
			if !tc.wantOK && !strings.Contains(reason, tc.wantWhy) {
				t.Fatalf("rejection reason %q does not mention %q", reason, tc.wantWhy)
			}
		})
	}
}

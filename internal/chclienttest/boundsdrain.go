//go:build chdb

package chclienttest

import (
	"fmt"
	"testing"
)

// boundsDrainFudge is the slack the bound assertion allows above the
// declared output bound. A result-buffering handler is O(output), not
// O(input) — but "O(output)" admits a small constant factor: the matrix
// pivot may pull a handful of rows that round to empty windows, the
// trace-limit pushdown keeps `limit` traces but each carries a few spans,
// etc. The harness asserts drain <= outputBound * boundsDrainFudge so a
// genuinely-bounded handler passes while an O(input) drain (which scales
// with the seed, far past any constant factor) fails. Keep it small: a
// large fudge would let an unbounded drain slip through on a small seed.
const boundsDrainFudge = 2

// BoundsDrainCase is one row of the bounds-drain harness. It encodes the
// principle the two production OOMs violated: a result-buffering handler
// must drain O(output), not O(input). A case proves that by VARYING the
// axis the input scales on (dataset size / window / cardinality) while
// holding the output bound (limit / step-count) fixed, then asserting the
// drained-row count tracks output, not input.
//
// Run performs the head-specific work — seed the table at scale, drive the
// handler, and return the two numbers the harness reasons over:
//
//   - drain: the rows the handler pulled into its in-process buffer (the
//     quantity that OOMed twice). Read it from the uniform drain counter:
//     Tempo's SearchMetrics.InspectedTraces, the eager engine.Result.Inspected,
//     or the PromQL streaming hook (cursor.Inspected() via SetOnRangeDrain).
//   - fullSeed: the total rows the unbounded handler WOULD have drained —
//     i.e. the full match set the seed planted. This is the degeneracy
//     guard: without asserting drain < fullSeed, a too-small seed lets a
//     broken (unbounded) handler pass vacuously, because on a tiny table
//     "drain everything" and "drain the bound" coincide.
//
// OutputBound is the handler's declared output ceiling — the trace limit,
// the (series × step) matrix cell count, etc. The harness asserts
// drain <= OutputBound * boundsDrainFudge.
type BoundsDrainCase struct {
	Name        string
	OutputBound int64
	Run         func(t *testing.T) (drain, fullSeed int64)
}

// BoundsDrainViolation is the pure predicate the harness applies to one
// case's measured numbers. It encodes the two checks that make the
// O(output)-not-O(input) principle falsifiable:
//
//  1. Degeneracy guard: fullSeed must exceed outputBound, else a bounded and
//     an unbounded handler drain the same count and the bound check is
//     vacuous — the blind spot that shipped both OOMs (fixed tiny seeds never
//     varied the input axis). A misconfigured seed is itself a violation.
//  2. Bound: drain <= outputBound * boundsDrainFudge AND drain < fullSeed.
//     The handler buffered only (a small constant factor of) its output, and
//     that buffer is a REAL reduction below what an unbounded drain would
//     have pulled.
//
// It returns ok=false with a human-readable reason on any violation. Keeping
// it pure (no *testing.T) lets the falsifiability test assert directly that a
// bounded count passes and an unbounded count fails, without spinning a
// nested test runner.
func BoundsDrainViolation(drain, outputBound, fullSeed int64) (ok bool, reason string) {
	if fullSeed <= outputBound {
		return false, fmt.Sprintf("seed misconfigured: fullSeed=%d must exceed outputBound=%d, "+
			"otherwise a bounded and an unbounded handler are indistinguishable "+
			"(the blind spot that shipped both OOMs)", fullSeed, outputBound)
	}
	maxDrain := outputBound * boundsDrainFudge
	if drain > maxDrain {
		return false, fmt.Sprintf("drain=%d exceeds bound %d (outputBound=%d × fudge=%d): "+
			"the handler buffered O(input), not O(output) — on a seed of %d this is the "+
			"unbounded drain that OOMs", drain, maxDrain, outputBound, boundsDrainFudge, fullSeed)
	}
	if drain >= fullSeed {
		return false, fmt.Sprintf("drain=%d did not reduce below full seed %d: the handler "+
			"drained the whole match set (the OOM bug); a bound that keeps everything is no bound",
			drain, fullSeed)
	}
	return true, ""
}

// RunBoundsDrain executes a table of BoundsDrainCase rows, applying
// BoundsDrainViolation to each case's measured (drain, fullSeed) against its
// declared OutputBound. Every case must scale its seed so fullSeed
// comfortably exceeds OutputBound; the degeneracy guard fails the row loudly
// if it does not, so a future edit that shrinks a seed below the bound cannot
// silently defang it.
func RunBoundsDrain(t *testing.T, cases []BoundsDrainCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			drain, fullSeed := tc.Run(t)
			if ok, reason := BoundsDrainViolation(drain, tc.OutputBound, fullSeed); !ok {
				t.Errorf("%s: %s", tc.Name, reason)
			}
		})
	}
}

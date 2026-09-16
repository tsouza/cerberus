// Package fixtures holds small, self-contained synthetic targets used only
// to exercise the semantic-mutation runner (issue #3448) end-to-end. Nothing
// here is production code: this package is never imported by cmd/cerberus or
// by any internal/** package, and no function here embodies a real per-head
// query contract. Real, contract-linked mutations arrive in #3449-#3451;
// these fixtures exist solely so test/semantic/mutants/*.json has a real
// file to patch, compile and run a detector against, without touching
// production behaviour.
package fixtures

// ClampUpper returns hi when v exceeds hi, and v otherwise. It does not
// clamp a lower bound — this fixture only needs one operator to mutate.
// MUTANT-SYNTH-KILLED-INVERT inverts the comparison (`>` -> `<`), which
// TestClampUpperCatchesInversion below distinguishes at v > hi.
func ClampUpper(v, hi int) int {
	if v > hi {
		return hi
	}
	return v
}

// PreallocCapacity is the slice capacity a caller would pre-allocate for n
// buckets: n elements plus one slot of growth headroom.
// MUTANT-SYNTH-SURVIVED-CAPACITY drops the "+1" headroom term. The fixture's
// own detector only asserts the capacity is at least n (enough to hold n
// elements without a reallocation), never the exact headroom, so it cannot
// see the difference — a genuine, documented test gap, not a runner defect.
func PreallocCapacity(n int) int {
	return n*2 + 1
}

// CommutativeSum adds its two arguments. MUTANT-SYNTH-EQUIV-COMMUTE swaps
// the operand order (a+b -> b+a), which two's-complement addition makes
// byte-for-byte identical for every pair of inputs — see that record's
// equivalence_review.
func CommutativeSum(a, b int) int {
	return a + b
}

// SpinUntil sums 0..n-1. MUTANT-SYNTH-TIMEOUT-LOOP inverts the loop's
// advance so i never reaches n; the loop body allocates nothing, so a stuck
// run costs CPU only, never memory, and the runner's own per-detector
// wall-clock ceiling is what must catch it.
func SpinUntil(n int) int {
	total := 0
	for i := 0; i < n; i++ {
		total += i
	}
	return total
}

// ExitAbruptly returns v unchanged. MUTANT-SYNTH-INFRA-EXIT replaces the
// return with a direct os.Exit(2) call, which bypasses go test's own
// pass/fail reporting entirely — the crash case the runner must classify as
// infrastructure-error, never as a kill.
func ExitAbruptly(v int) int {
	return v
}

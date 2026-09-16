package fixtures

import "testing"

// TestClampUpperCatchesInversion is MUTANT-SYNTH-KILLED-INVERT's detector:
// at v > hi the correct implementation clamps to hi; the `>` -> `<` mutant
// falls through and returns v unclamped.
func TestClampUpperCatchesInversion(t *testing.T) {
	if got := ClampUpper(15, 10); got != 10 {
		t.Fatalf("ClampUpper(15, 10) = %d, want 10", got)
	}
}

// TestPreallocCapacityAtLeastN is MUTANT-SYNTH-SURVIVED-CAPACITY's detector.
// It deliberately only asserts a lower bound, not the exact headroom, so the
// dropped "+1" term does not change the observed result — see the record's
// own rationale for why this is a genuine, documented gap.
func TestPreallocCapacityAtLeastN(t *testing.T) {
	if got := PreallocCapacity(5); got < 5 {
		t.Fatalf("PreallocCapacity(5) = %d, want >= 5", got)
	}
}

// TestCommutativeSumMatchesOperandOrder is MUTANT-SYNTH-EQUIV-COMMUTE's
// detector; it passes under both operand orders because addition commutes.
func TestCommutativeSumMatchesOperandOrder(t *testing.T) {
	if got := CommutativeSum(3, 4); got != 7 {
		t.Fatalf("CommutativeSum(3, 4) = %d, want 7", got)
	}
}

// TestSpinUntilTerminates is MUTANT-SYNTH-TIMEOUT-LOOP's detector. Under the
// mutant it never returns; the runner's own per-detector wall-clock ceiling,
// not this test's timeout, is what must end the run.
func TestSpinUntilTerminates(t *testing.T) {
	if got := SpinUntil(5); got != 10 {
		t.Fatalf("SpinUntil(5) = %d, want 10", got)
	}
}

// TestExitAbruptlyReturnsInput is MUTANT-SYNTH-INFRA-EXIT's detector. Under
// the mutant the process exits before this assertion ever runs.
func TestExitAbruptlyReturnsInput(t *testing.T) {
	if got := ExitAbruptly(7); got != 7 {
		t.Fatalf("ExitAbruptly(7) = %d, want 7", got)
	}
}

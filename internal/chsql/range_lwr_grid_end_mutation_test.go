package chsql

import (
	"testing"
	"time"
)

// TestMutation_StartAnchoredGridEnd_ZeroGuardIsOr defends
// startAnchoredGridEnd's `start.IsZero() || end.IsZero()` guard
// (range_lwr.go) against an INVERT_LOGICAL mutant that turns the `||` into
// `&&`. With `&&`, the guard only fires when BOTH times are zero, so a
// call with exactly one zero argument would fall through to
// `start.Add((numAnchors-1)*stepNS)` instead of returning end — computing a
// bogus anchor from a zero-value Time rather than reporting the caller's
// actual (already invalid) span. Each case below exercises exactly one
// argument being zero and asserts the OR's early return.
func TestMutation_StartAnchoredGridEnd_ZeroGuardIsOr(t *testing.T) {
	const stepNS = int64(30 * time.Second)
	const numAnchors = int64(11)
	nonZero := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

	t.Run("zero start, non-zero end returns end", func(t *testing.T) {
		got := startAnchoredGridEnd(time.Time{}, nonZero, stepNS, numAnchors)
		if !got.Equal(nonZero) {
			t.Fatalf("startAnchoredGridEnd(zero, %s, ...) = %s, want %s (the OR must fire on start alone)",
				nonZero, got, nonZero)
		}
	})

	t.Run("non-zero start, zero end returns end", func(t *testing.T) {
		got := startAnchoredGridEnd(nonZero, time.Time{}, stepNS, numAnchors)
		if !got.IsZero() {
			t.Fatalf("startAnchoredGridEnd(%s, zero, ...) = %s, want the zero Time (the OR must fire on end alone)",
				nonZero, got)
		}
	})

	t.Run("both zero returns end", func(t *testing.T) {
		got := startAnchoredGridEnd(time.Time{}, time.Time{}, stepNS, numAnchors)
		if !got.IsZero() {
			t.Fatalf("startAnchoredGridEnd(zero, zero, ...) = %s, want the zero Time", got)
		}
	})

	t.Run("neither zero computes the start-anchored end", func(t *testing.T) {
		// The span is NOT a multiple of the step (5m15s over 30s), so the
		// Start-anchored newest anchor (start + 10 steps = start + 5m) and
		// the raw end differ by the 15s remainder: a `return end` here — a
		// wholesale revert of the Start anchoring — is caught, which an
		// exact-multiple span (where both coincide) could not do.
		start := nonZero
		end := start.Add(5*time.Minute + 15*time.Second)
		want := start.Add(time.Duration((numAnchors - 1) * stepNS))
		if want.Equal(end) {
			t.Fatalf("the case's span must not be a step multiple, or the anchoring is indistinguishable from the raw end")
		}
		got := startAnchoredGridEnd(start, end, stepNS, numAnchors)
		if !got.Equal(want) {
			t.Fatalf("startAnchoredGridEnd(%s, %s, ...) = %s, want %s", start, end, got, want)
		}
	})
}

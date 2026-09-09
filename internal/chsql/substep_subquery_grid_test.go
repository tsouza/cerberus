package chsql

import (
	"testing"
	"time"
)

// TestStepAlignedAnchorCountFor_SubStepWindow pins the anchor count of an
// epoch-aligned subquery grid whose window is narrower than one step.
//
// Reference Prometheus answers the EMPTY matrix. promql/engine.go's
// evalSubquery leaves `newEv.endTimestamp` unsnapped and sets
// `newEv.startTimestamp` to the snapped base bumped by one interval, so a
// window narrower than one step leaves start > end, and
// `if ev.endTimestamp < ev.startTimestamp { return Matrix{}, nil }`
// returns nothing. This helper used to return 1 and justified it with the
// claim that "reference clamps start to end" — the same false claim
// internal/promql's own subquery grid carried (cerberus issue #3183). The
// two paths are reached by different queries: `(up * 2)[1s:1m]` goes
// through the promql grid, the bare `up[1s:1m]` comes straight here, so
// fixing one and not the other leaves the issue's own example wrong.
//
// The count feeds `anchorFanoutFrag`'s `range(0, n)`, so 0 is not a
// sentinel: it is the empty array the arrayJoin needs to produce no rows.
func TestStepAlignedAnchorCountFor_SubStepWindow(t *testing.T) {
	t.Parallel()

	const step = time.Minute
	// 01:04:41 is off the minute grid, so δ = 41s.
	offGrid := time.Date(2026, time.January, 1, 1, 4, 41, 0, time.UTC)
	// 01:05:00 is exactly on it, so δ = 0.
	onGrid := time.Date(2026, time.January, 1, 1, 5, 0, 0, time.UTC)

	cases := []struct {
		name       string
		end        time.Time
		outerRange time.Duration
		want       int64
	}{
		{
			// The issue's own example. Window (end-1s, end] holds no
			// phase-0 instant.
			name: "sub-step window, off grid", end: offGrid,
			outerRange: time.Second, want: 0,
		},
		{
			// δ = 41s already exceeds the 30s range, so the snapped base
			// is outside the window: still no anchor.
			name: "range shorter than the phase offset", end: offGrid,
			outerRange: 30 * time.Second, want: 0,
		},
		{
			// δ = 0 and range = step: the window is left-OPEN, so the
			// snapped base is the only anchor inside it.
			name: "exactly one step, on grid", end: onGrid,
			outerRange: step, want: 1,
		},
		{
			// The boundary from the other side: one nanosecond less than
			// the phase offset still spans nothing, one more spans one.
			name: "one ns short of the phase offset", end: offGrid,
			outerRange: 41 * time.Second, want: 0,
		},
		{
			name: "one ns past the phase offset", end: offGrid,
			outerRange: 41*time.Second + time.Nanosecond, want: 1,
		},
		{
			// A zero End means the base is now64(9), unknowable at emit
			// time, so the inclusive count passes through untouched.
			name: "unknown base falls back to the caller's count", end: time.Time{},
			outerRange: time.Second, want: 7,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const inclusive = 7 // deliberately not any correct answer below
			got := stepAlignedAnchorCountFor(tc.end, 0, tc.outerRange, step.Nanoseconds(), inclusive)
			if got != tc.want {
				t.Fatalf("stepAlignedAnchorCountFor(end=%s, range=%s, step=%s) = %d, want %d",
					tc.end, tc.outerRange, step, got, tc.want)
			}
		})
	}
}

package main

import (
	"testing"
	"time"
)

// The margin derivation is the whole correctness argument of the bounded
// stale-row cleanup: the DELETE cutoff is `max(<time column>) - margin`, so
// a margin NARROWER than the family's own window would delete the tick that
// just inserted it — the freshly written oldest row sits a full span behind
// the freshly written newest one. That failure is invisible in a one-shot
// seed and only shows up under the rolling re-seeder, as rows quietly going
// missing, which is why it is pinned here rather than left to the e2e lane.
func TestStaleMarginExceedsTheWindowItProtects(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		span   time.Duration
		margin time.Duration
	}{
		{"metrics narrow", windowSpan(metricsNarrowCadence, metricsNarrowSamples), metricsNarrowStaleMargin},
		{"metrics wide", windowSpan(metricsWideCadence, metricsWideSamples), metricsWideStaleMargin},
		{"logs", windowSpan(logsCadence, logsSamples), logsStaleMargin},
		{"traces", tracesMaxOffset - tracesMinOffset, tracesStaleMargin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.margin <= tc.span {
				t.Fatalf("margin %v does not exceed the window span %v: a tick would delete its own oldest row",
					tc.margin, tc.span)
			}
		})
	}
}

func TestWindowSpanMeasuresTheDistanceBetweenFirstAndLastSample(t *testing.T) {
	t.Parallel()

	// N samples at cadence c span (N-1)*c, not N*c: the span is the distance
	// between the endpoints, not the count of intervals plus one.
	if got, want := windowSpan(15*time.Second, 40), 585*time.Second; got != want {
		t.Fatalf("windowSpan(15s, 40) = %v, want %v", got, want)
	}
	if got := windowSpan(time.Second, 1); got != 0 {
		t.Fatalf("a single-sample window spans %v, want 0", got)
	}
	if got := windowSpan(time.Second, 0); got > 0 {
		t.Fatalf("an empty window spans %v, want a non-positive span", got)
	}
}

func TestStaleMarginAddsTheHeadroomOnTopOfTheSpan(t *testing.T) {
	t.Parallel()

	if got, want := staleMargin(10*time.Second), 10*time.Second+staleMarginHeadroom; got != want {
		t.Fatalf("staleMargin(10s) = %v, want %v", got, want)
	}
}

// The DELETE templates bind the margin as a UInt64 second count, so a
// margin that is not a whole number of seconds is truncated — always
// downward, which would narrow the very bound staleMargin just widened.
// Every derived margin must therefore already be whole seconds.
func TestMarginSecondsIsExactForEveryDerivedMargin(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		margin time.Duration
	}{
		{"metrics narrow", metricsNarrowStaleMargin},
		{"metrics wide", metricsWideStaleMargin},
		{"logs", logsStaleMargin},
		{"traces", tracesStaleMargin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			secs := marginSeconds(tc.margin)
			if time.Duration(secs)*time.Second != tc.margin { //nolint:gosec // G115: secs is a small derived margin
				t.Fatalf("marginSeconds(%v) = %d s, which is not the margin itself — the bind truncates it",
					tc.margin, secs)
			}
		})
	}
}

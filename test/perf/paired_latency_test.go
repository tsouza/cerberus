package perf

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// latencyComparison is comparePairedLatency's result: each variant's median
// over its own timed samples, and the median of the per-pair AFTER/BEFORE
// ratios, which is the number a regression guard compares against its bound.
type latencyComparison struct {
	Before, After time.Duration
	// Ratio is the median over the timed pairs of after/before: above 1
	// means AFTER is slower, below 1 means AFTER is faster.
	Ratio float64
	// PairRatios holds every timed pair's after/before, in run order, for a
	// failure message to show the whole distribution.
	PairRatios []float64
}

// comparePairedLatency times beforeQuery and afterQuery as reps adjacent
// pairs through run, discarding the first warmup pairs. The pairs alternate
// which variant runs first (BEFORE then AFTER on even pairs, AFTER then
// BEFORE on odd ones); with an even number of timed pairs each order is
// timed equally often, so a first-in-pair advantage cancels out. Each pair's
// two samples are taken back to back, so a host slowdown that spans a pair
// inflates both and leaves that pair's ratio intact; Ratio moves only when
// such a one-sided slowdown lands in at least half of the timed pairs.
func comparePairedLatency(reps, warmup int, run func(query string) time.Duration, beforeQuery, afterQuery string) latencyComparison {
	if reps <= warmup {
		panic(fmt.Sprintf("comparePairedLatency: %d reps leave no timed pair after %d warmup pairs", reps, warmup))
	}
	var before, after []time.Duration
	var ratios []float64
	for i := range reps {
		var b, a time.Duration
		if i%2 == 0 {
			b = run(beforeQuery)
			a = run(afterQuery)
		} else {
			a = run(afterQuery)
			b = run(beforeQuery)
		}
		if i < warmup {
			continue
		}
		before = append(before, b)
		after = append(after, a)
		ratios = append(ratios, float64(a)/float64(maxInt1Duration(b)))
	}
	return latencyComparison{
		Before:     median(before),
		After:      median(after),
		Ratio:      median(ratios),
		PairRatios: ratios,
	}
}

// median returns the upper median of xs without reordering it.
func median[T float64 | time.Duration](xs []T) T {
	sorted := slices.Clone(xs)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

// maxInt1Duration floors d at 1ns so a latency ratio never divides by zero
// on an implausibly fast sample.
func maxInt1Duration(d time.Duration) time.Duration {
	if d < 1 {
		return 1
	}
	return d
}

const (
	pairedTestReps   = 6
	pairedTestWarmup = 2
	pairedTestBase   = 100 * time.Millisecond
	pairedTestBefore = "before"
	pairedTestAfter  = "after"
)

// scriptedRun returns a comparePairedLatency runner that answers each
// variant's n-th call with that variant's n-th scripted duration.
func scriptedRun(t *testing.T, before, after []time.Duration) func(string) time.Duration {
	t.Helper()
	var nb, na int
	return func(query string) time.Duration {
		t.Helper()
		switch query {
		case pairedTestBefore:
			nb++
			return before[nb-1]
		case pairedTestAfter:
			na++
			return after[na-1]
		}
		t.Fatalf("unexpected query %q", query)
		return 0
	}
}

// TestComparePairedLatency_AlternatesVariantsWithinEachPair pins the run
// order: the two variants alternate call by call, and the variant that runs
// first flips every pair, warmup included.
func TestComparePairedLatency_AlternatesVariantsWithinEachPair(t *testing.T) {
	var order []string
	run := func(query string) time.Duration {
		order = append(order, query)
		return pairedTestBase
	}
	comparePairedLatency(pairedTestReps, pairedTestWarmup, run, pairedTestBefore, pairedTestAfter)

	var want []string
	for i := range pairedTestReps {
		if i%2 == 0 {
			want = append(want, pairedTestBefore, pairedTestAfter)
		} else {
			want = append(want, pairedTestAfter, pairedTestBefore)
		}
	}
	if !slices.Equal(order, want) {
		t.Fatalf("run order = %v, want %v", order, want)
	}
}

// TestComparePairedLatency_ContiguousSlowdownLeavesRatioIntact replays the
// failure shape that sequential sampling is exposed to: a host slowdown that
// covers the second half of the timed calls. Sampled as all-BEFORE then
// all-AFTER, that window would land on AFTER alone and triple its median.
// Paired, it inflates both members of the pairs it covers, so the ratio of
// two equally fast variants stays exactly 1.
func TestComparePairedLatency_ContiguousSlowdownLeavesRatioIntact(t *testing.T) {
	const slowdown = 3
	calls := 0
	slowFrom := pairedTestReps // the second half of the reps*2 calls
	run := func(string) time.Duration {
		calls++
		if calls > slowFrom {
			return slowdown * pairedTestBase
		}
		return pairedTestBase
	}
	got := comparePairedLatency(pairedTestReps, pairedTestWarmup, run, pairedTestBefore, pairedTestAfter)
	if got.Ratio != 1 {
		t.Fatalf("Ratio = %v under a slowdown spanning whole pairs, want 1 (pairs %v)", got.Ratio, got.PairRatios)
	}
}

// TestComparePairedLatency_RatioIsTheMedianOfPairRatios pins that Ratio is
// the median of the per-pair ratios, not the ratio of the two medians: on
// this script the pair ratios are 1.5, 0.5, 1.2 (median 1.2), while
// median(AFTER)/median(BEFORE) is 250ms/300ms.
func TestComparePairedLatency_RatioIsTheMedianOfPairRatios(t *testing.T) {
	const timedPairs = 3
	before := []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 300 * time.Millisecond}
	after := []time.Duration{150 * time.Millisecond, 250 * time.Millisecond, 360 * time.Millisecond}
	const wantRatio = 1.2
	got := comparePairedLatency(timedPairs, 0, scriptedRun(t, before, after), pairedTestBefore, pairedTestAfter)
	if got.Ratio != wantRatio {
		t.Fatalf("Ratio = %v, want %v (the median of pair ratios %v)", got.Ratio, wantRatio, got.PairRatios)
	}
}

// TestComparePairedLatency_DiscardsTheWarmupPairs pins that the FIRST warmup
// pairs are dropped from every output. The warmup pairs are slow, with a 10x
// AFTER spike; the timed pairs step up from pairedTestBase so that two extra
// slow samples would move each variant's upper median by one step. None of
// the warmup may reach Before, After, Ratio or PairRatios.
func TestComparePairedLatency_DiscardsTheWarmupPairs(t *testing.T) {
	const warmupSpike = 10
	cold := warmupSpike * pairedTestBase * pairedTestReps
	var before, after []time.Duration
	for i := range pairedTestReps {
		if i < pairedTestWarmup {
			before = append(before, cold)
			after = append(after, warmupSpike*cold)
			continue
		}
		step := time.Duration(i-pairedTestWarmup+1) * pairedTestBase
		before = append(before, step)
		after = append(after, step)
	}
	got := comparePairedLatency(pairedTestReps, pairedTestWarmup, scriptedRun(t, before, after), pairedTestBefore, pairedTestAfter)

	wantPairs := slices.Repeat([]float64{1}, pairedTestReps-pairedTestWarmup)
	if !slices.Equal(got.PairRatios, wantPairs) {
		t.Fatalf("PairRatios = %v, want %v: the warmup pairs leaked into the timed set", got.PairRatios, wantPairs)
	}
	wantMedian := median(before[pairedTestWarmup:])
	if got.Before != wantMedian || got.After != wantMedian || got.Ratio != 1 {
		t.Fatalf("Before=%s After=%s Ratio=%v, want %s, %s, 1: the warmup pairs leaked into the medians",
			got.Before, got.After, got.Ratio, wantMedian, wantMedian)
	}
}

// TestComparePairedLatency_ReportsARealRegression proves the comparison still
// bites: an AFTER that is genuinely twice as slow on every call reports a
// ratio of 2, whichever variant runs first in the pair.
func TestComparePairedLatency_ReportsARealRegression(t *testing.T) {
	const regression = 2
	run := func(query string) time.Duration {
		if query == pairedTestAfter {
			return regression * pairedTestBase
		}
		return pairedTestBase
	}
	got := comparePairedLatency(pairedTestReps, pairedTestWarmup, run, pairedTestBefore, pairedTestAfter)
	if got.Ratio != regression {
		t.Fatalf("Ratio = %v, want %v", got.Ratio, float64(regression))
	}
	if got.Before != pairedTestBase || got.After != regression*pairedTestBase {
		t.Fatalf("medians = (%s, %s), want (%s, %s)", got.Before, got.After, pairedTestBase, regression*pairedTestBase)
	}
}

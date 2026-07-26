package tolerant

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/test/e2e/migration/tolerances"
)

// linearCounter builds a perfectly-linear counter series: one sample every
// step from start to end inclusive, growing by rate per step.
func linearCounter(start, end time.Time, step time.Duration, rate float64) []Sample {
	var out []Sample
	v := 0.0
	for t := start; !t.After(end); t = t.Add(step) {
		out = append(out, Sample{T: t, V: v})
		v += rate
	}
	return out
}

func TestDownsampleCounterAwareBucketsAndCarriesForward(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(9 * time.Minute)
	raw := linearCounter(start, end, time.Minute, 1)

	got, err := DownsampleCounterAware(raw, start, end, 3*time.Minute)
	if err != nil {
		t.Fatalf("DownsampleCounterAware: %v", err)
	}
	// 9 minutes over 3-minute buckets: [0,3) [3,6) [6,9) [9,9] — the trailing
	// partial bucket at the window's exact end still gets its own point.
	if len(got) < 3 {
		t.Fatalf("got %d buckets, want at least 3: %+v", len(got), got)
	}
	// Bucket 0 covers raw samples at minute 0,1,2 — max is minute 2's value.
	if got[0].V != raw[2].V {
		t.Errorf("bucket 0 = %v, want max-in-bucket %v", got[0].V, raw[2].V)
	}
}

func TestDownsampleCounterAwareCarriesForwardAnEmptyBucket(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// One sample at minute 0, then a gap, then nothing until minute 10 — the
	// bucket spanning minutes 4-6 has no raw sample at all.
	raw := []Sample{
		{T: start, V: 10},
		{T: start.Add(10 * time.Minute), V: 20},
	}
	got, err := DownsampleCounterAware(raw, start, start.Add(10*time.Minute), 2*time.Minute)
	if err != nil {
		t.Fatalf("DownsampleCounterAware: %v", err)
	}
	// The bucket covering minutes 2-4 (index 1) has no raw sample; it must
	// carry forward bucket 0's value of 10 rather than reporting a gap.
	if len(got) < 2 {
		t.Fatalf("got %d buckets, want at least 2: %+v", len(got), got)
	}
	if got[1].V != 10 {
		t.Errorf("empty bucket carried forward %v, want 10", got[1].V)
	}
}

func TestIncreaseNeedsAtLeastTwoPoints(t *testing.T) {
	if _, ok := Increase(nil); ok {
		t.Error("Increase(nil) reported growth measured")
	}
	if _, ok := Increase([]Sample{{V: 1}}); ok {
		t.Error("Increase of a single point reported growth measured")
	}
	got, ok := Increase([]Sample{{V: 1}, {V: 4}})
	if !ok || got != 3 {
		t.Errorf("Increase = (%v, %v), want (3, true)", got, ok)
	}
}

// TestCompareWithinDeclaredBand exercises the exact bucket/window shape
// MIG20Downsample's derivation declares (a 2-minute bucket over a 20-minute
// window) against a near-linear counter, and asserts the comparator reports
// the reconstruction within the declared band — the same property the live
// Tier-1 scenario checks against a real fixture, pinned here against
// synthetic data so it needs no Docker stack to verify.
func TestCompareWithinDeclaredBand(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	window := 20 * time.Minute
	end := start.Add(window)
	raw := linearCounter(start, end, time.Minute, 2)
	rawIncrease, ok := Increase(raw)
	if !ok {
		t.Fatal("linear counter fixture measured no growth")
	}

	downsampled, err := DownsampleCounterAware(raw, start, end, tolerances.MIG20DownsampleBucket)
	if err != nil {
		t.Fatalf("DownsampleCounterAware: %v", err)
	}

	res, err := Compare(rawIncrease, downsampled, tolerances.MIG20Downsample)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !res.WithinBand {
		t.Errorf("Compare reported outside the declared band: %s", res.Detail)
	}
	if res.RelativeDelta <= 0 {
		t.Errorf("Compare reported a zero relative delta for a downsampled reconstruction that structurally loses the leading edge: %s", res.Detail)
	}
}

// TestCompareOutsideBandForACoarseBucket proves the comparator actually
// discriminates: a bucket far coarser than what the band was derived for
// pushes the relative delta past it.
func TestCompareOutsideBandForACoarseBucket(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	window := 20 * time.Minute
	end := start.Add(window)
	raw := linearCounter(start, end, time.Minute, 2)
	rawIncrease, ok := Increase(raw)
	if !ok {
		t.Fatal("linear counter fixture measured no growth")
	}

	// A bucket as wide as the whole window collapses the reconstruction to
	// two points spanning almost the entire range, losing nearly all of the
	// measured growth to the leading edge — far past the band derived for a
	// bucket ten times narrower.
	downsampled, err := DownsampleCounterAware(raw, start, end, window)
	if err != nil {
		t.Fatalf("DownsampleCounterAware: %v", err)
	}
	res, err := Compare(rawIncrease, downsampled, tolerances.MIG20Downsample)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if res.WithinBand {
		t.Errorf("Compare reported within band for a bucket spanning the whole window: %s", res.Detail)
	}
}

func TestCompareRejectsAnUndersizedReconstruction(t *testing.T) {
	if _, err := Compare(10, []Sample{{V: 1}}, tolerances.MIG20Downsample); err == nil {
		t.Error("Compare accepted a reconstruction with fewer than two points")
	}
}

func TestCompareRejectsZeroRawIncrease(t *testing.T) {
	if _, err := Compare(0, []Sample{{V: 1}, {V: 2}}, tolerances.MIG20Downsample); err == nil {
		t.Error("Compare accepted a zero raw increase, which makes the relative delta undefined")
	}
}

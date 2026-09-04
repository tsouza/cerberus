package smoke

import (
	"context"
	"testing"
	"time"
)

// TestSumUint64 pins the exact bucket-shape total nativeHistogramBucketShape
// bakes into Sentinel 1's seeded Count/Sum, so a future edit to the shape (or
// to sumUint64 itself) is caught here rather than only showing up as a
// changed memory number in the real-ClickHouse integration lane.
func TestSumUint64(t *testing.T) {
	const want = 1922 // sum of nativeHistogramBucketShape()'s 24 buckets
	if got := sumUint64(nativeHistogramBucketShape()); got != want {
		t.Fatalf("sumUint64(nativeHistogramBucketShape()) = %d, want %d", got, want)
	}
}

func TestChUint64ArrayLiteral(t *testing.T) {
	cases := []struct {
		name string
		in   []uint64
		want string
	}{
		{"empty", nil, "[]"},
		{"single", []uint64{7}, "[7]"},
		{"multiple", []uint64{1, 2, 3}, "[1,2,3]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chUint64ArrayLiteral(tc.in); got != tc.want {
				t.Fatalf("chUint64ArrayLiteral(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNativeHistogramBucketShape asserts the shape stays the non-degenerate,
// symmetric bell the seed.go doc comment promises (deliberately NOT the
// near-empty 3-bucket arrays test/spec's chDB fixtures use).
func TestNativeHistogramBucketShape(t *testing.T) {
	shape := nativeHistogramBucketShape()
	const wantLen = 24
	if len(shape) != wantLen {
		t.Fatalf("len(nativeHistogramBucketShape()) = %d, want %d", len(shape), wantLen)
	}
	if shape[0] != 1 || shape[len(shape)-1] != 1 {
		t.Fatalf("nativeHistogramBucketShape() = %v, want to start and end at 1", shape)
	}
	peak := shape[9]
	for _, v := range shape {
		if v > peak {
			t.Fatalf("nativeHistogramBucketShape() = %v, want bucket 9 (%d) to be the peak", shape, peak)
		}
	}
}

func TestSeedNativeHistogramAtScale_RejectsNonPositiveSeriesCount(t *testing.T) {
	start := time.Unix(0, 0)
	end := start.Add(time.Hour)
	if _, err := SeedNativeHistogramAtScale(context.Background(), nil, NativeHistogramMetric, 0, start, end); err == nil {
		t.Fatal("expected an error for a non-positive seriesCount")
	}
}

func TestSeedNativeHistogramAtScale_RejectsNonPositiveSampleWindow(t *testing.T) {
	start := time.Unix(0, 3600)
	end := start.Add(-2 * time.Hour) // end before start collapses the sample window
	if _, err := SeedNativeHistogramAtScale(context.Background(), nil, NativeHistogramMetric, 1, start, end); err == nil {
		t.Fatal("expected an error for a non-positive sample window")
	}
}

func TestSeedHighCardinalityCounter_RejectsNonPositiveSeriesCount(t *testing.T) {
	start := time.Unix(0, 0)
	end := start.Add(time.Hour)
	if _, err := SeedHighCardinalityCounter(context.Background(), nil, WideCounterMetric, 0, start, end); err == nil {
		t.Fatal("expected an error for a non-positive seriesCount")
	}
}

func TestSeedHighCardinalityCounter_RejectsNonPositiveSampleWindow(t *testing.T) {
	start := time.Unix(0, 3600)
	end := start.Add(-2 * time.Hour)
	if _, err := SeedHighCardinalityCounter(context.Background(), nil, WideCounterMetric, 1, start, end); err == nil {
		t.Fatal("expected an error for a non-positive sample window")
	}
}

func TestSeedSortedSlabOverTimeGauge_RejectsNonPositiveSeriesCount(t *testing.T) {
	start := time.Unix(0, 0)
	end := start.Add(time.Hour)
	if _, err := SeedSortedSlabOverTimeGauge(context.Background(), nil, SortedSlabOverTimeGaugeMetric, 0, start, end); err == nil {
		t.Fatal("expected an error for a non-positive seriesCount")
	}
}

func TestSeedSortedSlabOverTimeGauge_RejectsNonPositiveSampleWindow(t *testing.T) {
	start := time.Unix(0, 3600)
	end := start.Add(-2 * time.Hour)
	if _, err := SeedSortedSlabOverTimeGauge(context.Background(), nil, SortedSlabOverTimeGaugeMetric, 1, start, end); err == nil {
		t.Fatal("expected an error for a non-positive sample window")
	}
}

func TestSeedWideAttributeTraces_RejectsNonPositiveTraceCount(t *testing.T) {
	start := time.Unix(0, 0)
	end := start.Add(time.Hour)
	if _, err := SeedWideAttributeTraces(context.Background(), nil, 0, start, end); err == nil {
		t.Fatal("expected an error for a non-positive traceCount")
	}
}

func TestSeedWideAttributeTraces_RejectsNonPositiveSpan(t *testing.T) {
	start := time.Unix(0, 0)
	end := start // end == start, so span <= 0
	if _, err := SeedWideAttributeTraces(context.Background(), nil, 1, start, end); err == nil {
		t.Fatal("expected an error for a non-positive span")
	}
}

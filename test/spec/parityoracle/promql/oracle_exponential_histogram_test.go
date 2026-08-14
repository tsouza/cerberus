package promql

import (
	"math"
	"testing"
)

func TestEqualExponentialHistogramInterpolationValues_IssueValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		reference float64
		cerberus  float64
		wantULPs  uint64
	}{
		{"histogram_fraction_exp", 0.45575480796967555, 0.45575480796967544, 2},
		{"histogram_fraction_exp_negative_bounds", 0.6226721157729304, 0.6226721157729302, 1},
		{"histogram_fraction_exp_negative_range", 0.1043187546327135, 0.10431875463271348, 2},
		{"histogram_quantile_native_latest_sample", 59.71411145835569, 59.71411145835565, 5},
		{"histogram_quantile_native_bare_offset_range", 59.71411145835569, 59.71411145835565, 5},
		{"histogram_quantile_native_multi_series", 6.727171322029717, 6.727171322029716, 1},
		{"histogram_quantile_native_negative_p50", 3.3635856610148593, 3.363585661014858, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := ulpDistance(tc.reference, tc.cerberus); got != tc.wantULPs {
				t.Fatalf("ulpDistance(%v, %v) = %d, want %d", tc.reference, tc.cerberus, got, tc.wantULPs)
			}
			if !EqualExponentialHistogramInterpolationValues(tc.reference, tc.cerberus) {
				t.Fatalf("comparator rejected the measured %d-ULP pair", tc.wantULPs)
			}
			if !EqualExponentialHistogramInterpolationValues(tc.cerberus, tc.reference) {
				t.Fatalf("comparator is not symmetric for %v and %v", tc.reference, tc.cerberus)
			}
		})
	}
}

func TestEqualExponentialHistogramInterpolationValues_Boundary(t *testing.T) {
	t.Parallel()

	const base = 59.71411145835569
	fiveULPs := base
	for range exponentialHistogramInterpolationULPTolerance {
		fiveULPs = math.Nextafter(fiveULPs, math.Inf(1))
	}
	sixULPs := math.Nextafter(fiveULPs, math.Inf(1))

	if got := ulpDistance(base, fiveULPs); got != exponentialHistogramInterpolationULPTolerance {
		t.Fatalf("ulpDistance(base, fiveULPs) = %d, want %d", got, exponentialHistogramInterpolationULPTolerance)
	}
	if !EqualExponentialHistogramInterpolationValues(base, fiveULPs) {
		t.Fatalf("comparator rejected its %d-ULP boundary", exponentialHistogramInterpolationULPTolerance)
	}
	if EqualExponentialHistogramInterpolationValues(base, sixULPs) {
		t.Fatalf("comparator accepted six ULPs, above its documented bound")
	}
	if EqualValues(base, fiveULPs) {
		t.Fatal("EqualValues accepted a non-identical pair")
	}
}

func TestEqualExponentialHistogramInterpolationValues_NaN(t *testing.T) {
	t.Parallel()

	nan := math.NaN()
	if !EqualExponentialHistogramInterpolationValues(nan, nan) {
		t.Fatal("two NaN values must compare equal")
	}
	if EqualExponentialHistogramInterpolationValues(nan, 1) || EqualExponentialHistogramInterpolationValues(1, nan) {
		t.Fatal("a NaN and a real value must not compare equal")
	}
}

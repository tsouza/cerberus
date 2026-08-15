//go:build chdb

package spec

import (
	"math"
	"testing"
)

const expHistogramSeed = "CREATE TABLE otel_metrics_exponential_histogram (x UInt8) ENGINE = Memory"

const classicHistogramSeed = "CREATE TABLE otel_metrics_histogram (x UInt8) ENGINE = Memory"

const mixedHistogramSeed = expHistogramSeed + ";\n" + classicHistogramSeed

const selectorTestULPs = 5

func TestCompareValues(t *testing.T) {
	t.Parallel()

	const base = 59.71411145835569
	fiveULPs := base
	for range selectorTestULPs {
		fiveULPs = math.Nextafter(fiveULPs, math.Inf(1))
	}
	twoULPs := math.Nextafter(math.Nextafter(base, math.Inf(1)), math.Inf(1))
	oneULP := math.Nextafter(base, math.Inf(1))

	cases := []struct {
		name  string
		query string
		seed  string
		a     float64
		b     float64
		want  bool
	}{
		{"native histogram_fraction", "histogram_fraction(0.5, 3, latency_exp_hist)", expHistogramSeed, base, fiveULPs, true},
		{"native histogram_quantile", "histogram_quantile(0.95, latency_exp_hist)", expHistogramSeed, base, fiveULPs, true},
		{"classic histogram_quantile stays exact", "histogram_quantile(0.95, latency_bucket)", classicHistogramSeed, base, oneULP, false},
		{"mixed tables classic quantile stays exact", "histogram_quantile(0.95, latency_bucket)", mixedHistogramSeed, base, oneULP, false},
		{"classic histogram_quantile over rate gets ULP tolerance", "histogram_quantile(0.5, rate(latency_bucket[5m]))", classicHistogramSeed, base, twoULPs, true},
		{"classic histogram_quantile over increase gets ULP tolerance", "histogram_quantile(0.5, increase(latency_bucket[5m]))", classicHistogramSeed, base, twoULPs, true},
		{"classic histogram_quantile over rate rejects beyond tolerance", "histogram_quantile(0.5, rate(latency_bucket[5m]))", classicHistogramSeed, base, fiveULPs, false},
		{"classic histogram_quantile over rate, sum by(le) wrapper, gets ULP tolerance", "histogram_quantile(0.5, sum by(le) (rate(latency_bucket[5m])))", classicHistogramSeed, base, twoULPs, true},
		{"histogram_quantile over non-bucket rate stays exact", "histogram_quantile(0.5, rate(latency[5m]))", classicHistogramSeed, base, oneULP, false},
		{"composed classic rate quantile stays exact", "histogram_quantile(0.5, rate(latency_bucket[5m])) + up", classicHistogramSeed, base, oneULP, false},
		{"composed native quantile stays exact", "histogram_quantile(0.95, latency_exp_hist) + up", expHistogramSeed, base, oneULP, false},
		{"ordinary log2 stays exact", "log2(up)", expHistogramSeed, base, oneULP, false},
		{"ordinary power stays exact", "up ^ 2", expHistogramSeed, base, oneULP, false},
		{"atan2 retains one ULP bound", "2 atan2 up", expHistogramSeed, base, twoULPs, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			compare, err := compareValues(tc.query, tc.seed)
			if err != nil {
				t.Fatalf("compareValues: %v", err)
			}
			if got := compare(tc.a, tc.b); got != tc.want {
				t.Fatalf("compareValues(%q)(%v, %v) = %v, want %v", tc.query, tc.a, tc.b, got, tc.want)
			}
		})
	}
}

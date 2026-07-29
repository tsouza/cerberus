//go:build chdb

// chDB-backed end-to-end coverage for order-insensitive series identity
// on the HISTOGRAM paths — the third family, after the vector-join /
// set-op match keys (handler_chdb_map_key_order_test.go) and the plain
// selector / aggregation / range-function paths
// (handler_chdb_map_key_order_nonjoin_test.go).
//
// The histogram lowerings are their own family because they never touch
// the selector projection: `histogram_quantile` and the
// `histogram_count` / `_sum` / `_avg` value functions group straight off
// `otel_metrics_histogram` / `otel_metrics_exponential_histogram` to pick
// each series' newest sample. That group key IS the series identity, and
// a ClickHouse `Map(String, String)` compares POSITIONALLY over its
// (keys, values) arrays — so `map('job','api','dc','eu')` and
// `map('dc','eu','job','api')` are unequal despite carrying the same
// label set. The OTel-CH exporter preserves OTLP wire order and never
// sorts, so one logical histogram whose samples landed under two
// physical key orders splits in two before the quantile is even
// computed, and each half keeps only its own subset's newest sample.
//
// Both storage shapes are covered, because they take different lowering
// paths: the classic `BucketCounts` × `ExplicitBounds` table and the
// native exponential table. Both evaluation modes are covered too — the
// instant path collapses through an Aggregate, the range path through a
// RangeBucketFanout.
//
// The seed makes the split visible as a WRONG VALUE, not just a doubled
// series count: the stale (earlier) row is written under one key order
// and the live (later) row under the other, so a split series answers
// with the stale row's distribution.

package prom_test

import (
	"fmt"
	"testing"
	"time"
)

// mapKeyOrderHistogramDDL carries both OTel-CH histogram tables with the
// columns the value functions read (Count / Sum are absent from the
// range-mode tests' minimal DDLs). Plain `Map(String, String)` and
// MergeTree, matching gaugeDDL, so PREWHERE promotion runs for real.
const mapKeyOrderHistogramDDL = `CREATE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);

CREATE TABLE otel_metrics_exponential_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    Scale Int32,
    ZeroCount UInt64,
    PositiveOffset Int32,
    PositiveBucketCounts Array(UInt64),
    NegativeOffset Int32,
    NegativeBucketCounts Array(UInt64)
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

// The histogram seed writes ONE logical series per table as two samples a
// minute apart, the earlier under ascending key order and the later under
// descending, and gives the two samples different distributions so a
// split shows up in the value and not only in the series count.
const (
	histKeyOrderStaleCount = 10
	histKeyOrderLiveCount  = 25
	// The two sums are deliberately NOT in the same ratio to their
	// counts (5 vs 10), so histogram_avg — which divides one by the
	// other — also separates the stale row from the live one.
	histKeyOrderStaleSum  = 50
	histKeyOrderLiveSum   = 250
	histKeyOrderRangeSpan = 3 * time.Minute
	histKeyOrderAnchors   = 4
)

// histKeyOrderEvalTime is the instant every case evaluates at: the
// timestamp of the live (later) sample.
var histKeyOrderEvalTime = mapKeyOrderSeedTime.Add(nonJoinSampleGap)

// histogramDivergentSeed writes the classic and native histograms as two
// samples each, under two physical key orders carrying the same label
// set. Nothing else distinguishes the rows.
func histogramDivergentSeed() string {
	const (
		asc  = `map('dc', 'eu', 'job', 'api')`
		desc = `map('job', 'api', 'dc', 'eu')`
	)
	stale := mapKeyOrderSeedTime.Format("2006-01-02 15:04:05.000000000")
	live := histKeyOrderEvalTime.Format("2006-01-02 15:04:05.000000000")

	return mapKeyOrderHistogramDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_histogram VALUES
    ('mko_hist', %[1]s, toDateTime64('%[3]s', 9), %[5]d, %[7]d, [%[5]d, 0, 0], [1.0, 2.0]),
    ('mko_hist', %[2]s, toDateTime64('%[4]s', 9), %[6]d, %[8]d, [0, 0, %[6]d], [1.0, 2.0]);

INSERT INTO otel_metrics_exponential_histogram VALUES
    ('mko_exp_hist', %[1]s, toDateTime64('%[3]s', 9), %[5]d, %[7]d, 0, 0, 0, [%[5]d], 0, []),
    ('mko_exp_hist', %[2]s, toDateTime64('%[4]s', 9), %[6]d, %[8]d, 0, 0, 0, [0, %[6]d], 0, []);`,
		asc, desc, stale, live,
		histKeyOrderStaleCount, histKeyOrderLiveCount,
		histKeyOrderStaleSum, histKeyOrderLiveSum)
}

// TestHistogramDivergentMapKeyOrder_ChDB drives the classic and native
// histogram shapes end to end against series stored under two physical
// key orders. Every case must answer with ONE series carrying the live
// sample's value.
func TestHistogramDivergentMapKeyOrder_ChDB(t *testing.T) {
	cases := []struct {
		name  string
		query string
		// want is the single output series' value, rendered the way the
		// Prom wire format writes it.
		want string
		// broken records what the un-canonicalised engine answered, so a
		// regression is recognisable from the failure message alone.
		broken string
	}{
		{
			name:  "classic_quantile_collapses_key_orders",
			query: "histogram_quantile(0.9, mko_hist_bucket)",
			// The live row puts all 25 observations in the `+Inf`
			// bucket, so the 0.9 quantile clamps to the last explicit
			// bound, 2. The stale row puts all 10 in the `<=1` bucket:
			// rank 9 of 10 interpolates to 0.9 inside [0, 1].
			want:   "2",
			broken: "0.9",
		},
		{
			name:  "native_quantile_collapses_key_orders",
			query: "histogram_quantile(0.9, mko_exp_hist)",
			// Scale 0 and offset 0, so positive bucket i spans
			// (2^i, 2^(i+1)]. The live row's counts are [0, 25]: rank
			// 22.5 lands in the second bucket and log-linear
			// interpolation gives 2^(1 + 22.5/25) = 2^1.9. The stale
			// row's [10] gives 2^(0 + 9/10) = 2^0.9 ≈ 1.866.
			want:   "3.7321319661472296",
			broken: "1.8660659830736148",
		},
		{
			name:   "native_count_collapses_key_orders",
			query:  "histogram_count(mko_exp_hist)",
			want:   "25",
			broken: "10",
		},
		{
			name:   "native_sum_collapses_key_orders",
			query:  "histogram_sum(mko_exp_hist)",
			want:   "250",
			broken: "50",
		},
		{
			name:  "native_avg_collapses_key_orders",
			query: "histogram_avg(mko_exp_hist)",
			// 250 / 25 against the stale row's 50 / 10.
			want:   "10",
			broken: "5",
		},
	}

	seed := histogramDivergentSeed()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("instant", func(t *testing.T) {
				// A fresh session + server per case: a shared chDB
				// session carries seed state across cases.
				srv, _ := newChDBServer(t, seed)
				vec := runMapKeyOrderInstantAt(t, srv.URL, tc.query, histKeyOrderEvalTime)
				if len(vec) != 1 {
					t.Fatalf("series count: got %d, want 1 (un-canonicalised answer was %s): %+v",
						len(vec), tc.broken, vec)
				}
				if got := vec[0].Value[1]; got != tc.want {
					t.Errorf("value: got %v, want %q (un-canonicalised answer was %s)",
						got, tc.want, tc.broken)
				}
			})

			t.Run("range", func(t *testing.T) {
				srv, _ := newChDBServer(t, seed)
				matrix := runRangeModeQueryRange(t, srv.URL, tc.query,
					histKeyOrderEvalTime, histKeyOrderEvalTime.Add(histKeyOrderRangeSpan),
					mapKeyOrderRangeStep)
				if len(matrix) != 1 {
					t.Fatalf("series count: got %d, want 1 (un-canonicalised answer was %s): %+v",
						len(matrix), tc.broken, matrix)
				}
				if len(matrix[0].Values) != histKeyOrderAnchors {
					t.Fatalf("sample count: got %d, want %d: %+v",
						len(matrix[0].Values), histKeyOrderAnchors, matrix[0].Values)
				}
				// The live sample is the newest one inside every
				// anchor's lookback, so the value is flat across steps.
				for i, v := range matrix[0].Values {
					if got := v[1]; got != tc.want {
						t.Errorf("step %d: value: got %v, want %q (un-canonicalised answer was %s)",
							i, got, tc.want, tc.broken)
					}
				}
			})
		})
	}
}

// TestHistogramDivergentMapKeyOrder_LabelSetIsUnchanged_ChDB pins the
// other half of the contract on the histogram paths: canonicalising the
// Map must reorder keys and nothing else. The response labels are the
// union of both physical orders' keys with their original values — no
// key dropped, none invented, none rewritten.
func TestHistogramDivergentMapKeyOrder_LabelSetIsUnchanged_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, histogramDivergentSeed())
	vec := runMapKeyOrderInstantAt(t, srv.URL, "histogram_count(mko_exp_hist)", histKeyOrderEvalTime)
	if len(vec) != 1 {
		t.Fatalf("series count: got %d, want 1: %+v", len(vec), vec)
	}
	want := map[string]string{"dc": "eu", "job": "api"}
	got := vec[0].Metric
	if len(got) != len(want) {
		t.Fatalf("label count: got %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("label %q: got %q, want %q (full set %v)", k, got[k], w, got)
		}
	}
}

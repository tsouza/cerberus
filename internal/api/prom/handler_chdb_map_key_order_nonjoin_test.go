//go:build chdb

// chDB-backed end-to-end coverage for order-insensitive series identity
// on the NON-JOIN path: plain selectors, `by(...)` / `without(...)`
// aggregation, and range functions.
//
// The sibling handler_chdb_map_key_order_test.go covers the vector-join
// and set-op match keys. Those are only one of the places a
// `Map(String, String)` acts as a series identity. Every selector also
// collapses its raw rows onto one row per series — the LWR staleness
// pick — and every range function partitions its samples the same way,
// both keyed on the whole `Attributes` Map. A CH Map compares
// POSITIONALLY over its (keys, values) arrays, so
// `map('job','api','dc','eu')` and `map('dc','eu','job','api')` are
// unequal despite carrying the same label set, and the OTel-CH exporter
// preserves OTLP wire order without ever sorting. One logical series
// whose samples landed under two physical key orders therefore split in
// two before any join is involved.
//
// The three failure signatures this pins, all of them 200 OK:
//
//   - a DOUBLED count — `count()` sees two series where there is one;
//   - an INFLATED sum — the same series is added twice, once per order;
//   - an EMPTY range function — each partition holds a single sample,
//     and `increase` / `rate` need two.
//
// The fourth is the nastiest and has no join analogue: a range function
// like `max_over_time` returns a WRONG VALUE rather than nothing,
// because the two split series carry identical label sets and the
// handler's wire assembly silently collapses them, keeping whichever
// arrived first.

package prom_test

import (
	"fmt"
	"testing"
	"time"
)

// The non-join seed writes ONE logical series as two samples a minute
// apart, each under a different physical Map key order. Evaluation
// happens at the second sample's instant, so a correctly-collapsed
// series holds both samples and a split one holds a single sample each.
const (
	nonJoinSampleGap    = time.Minute
	nonJoinFirstValue   = 10
	nonJoinSecondValue  = 20
	nonJoinRangeSpan    = 3 * time.Minute
	nonJoinRangeAnchors = 4
)

// nonJoinEvalTime is the instant every case evaluates at: the timestamp
// of the second (later) sample.
var nonJoinEvalTime = mapKeyOrderSeedTime.Add(nonJoinSampleGap)

// nonJoinDivergentSeed writes one logical `mko_a` series as two samples,
// the earlier one under ascending key order and the later one under
// descending. Nothing else distinguishes the rows, so any result that
// reports two series has split on key order alone.
func nonJoinDivergentSeed() string {
	const (
		asc  = `map('dc', 'eu', 'job', 'api')`
		desc = `map('job', 'api', 'dc', 'eu')`
	)
	first := mapKeyOrderSeedTime.Format("2006-01-02 15:04:05.000000000")
	second := nonJoinEvalTime.Format("2006-01-02 15:04:05.000000000")
	return gaugeDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge VALUES
    ('mko_a', %s, toDateTime64('%s', 9), %d),
    ('mko_a', %s, toDateTime64('%s', 9), %d);`,
		asc, first, nonJoinFirstValue,
		desc, second, nonJoinSecondValue)
}

// TestNonJoinDivergentMapKeyOrder_ChDB drives the selector / aggregation
// / range-function shapes end to end through the real `/api/v1/query`
// handler against a series stored under two physical key orders.
func TestNonJoinDivergentMapKeyOrder_ChDB(t *testing.T) {
	cases := []struct {
		name  string
		query string
		// want is the single output series' value, rendered the way the
		// Prom wire format writes it.
		want string
		// broken records what the un-canonicalised engine answered, so a
		// regression is recognisable from the failure message alone.
		broken string
		// rangeStable marks the shapes whose value is identical at every
		// range-mode anchor. `increase` / `rate` are excluded: their
		// extrapolation factor depends on where the anchor sits relative
		// to the two samples, so they vary per step by design.
		rangeStable bool
	}{
		{
			name:  "count_collapses_key_orders",
			query: "count(mko_a)",
			// One logical series, so one. Two means the LWR collapse
			// keyed on the raw Map and kept both physical orders.
			want:        "1",
			broken:      "2",
			rangeStable: true,
		},
		{
			name:        "count_by_job_collapses_key_orders",
			query:       "count by (job) (mko_a)",
			want:        "1",
			broken:      "2",
			rangeStable: true,
		},
		{
			name:  "sum_by_map_valued_grouping",
			query: "sum by (job, dc) (mko_a)",
			// `by(...)` lowers to per-label subscripts, which are
			// order-insensitive on their own — but the LWR collapse
			// UNDERNEATH already emitted two rows, so the sum adds the
			// stale 10 to the live 20.
			want:        "20",
			broken:      "30",
			rangeStable: true,
		},
		{
			name:  "sum_no_grouping",
			query: "sum(mko_a)",
			// The no-grouping shape, which takes a different emitter
			// path from `by(...)` and must land on the same answer.
			want:        "20",
			broken:      "30",
			rangeStable: true,
		},
		{
			name:  "max_over_time_returns_wrong_value",
			query: "max_over_time(mko_a[5m])",
			// The wrong-VALUE direction. Split partitions hold one
			// sample each; both carry the same label set, so the wire
			// assembly collapses them and keeps the stale 10.
			want:        "20",
			broken:      "10",
			rangeStable: true,
		},
		{
			name:  "increase_needs_two_samples_in_one_partition",
			query: "increase(mko_a[5m])",
			// Delta 10 over a 60s sampled interval, extrapolated to the
			// 5m window: the head extends by half the average sample
			// spacing (30s) and the tail is already at the anchor, so
			// the factor is 90/60 and the answer is 15. A split series
			// gives each partition one sample and returns NOTHING.
			want:   "15",
			broken: "<empty result>",
		},
		{
			name:  "rate_needs_two_samples_in_one_partition",
			query: "rate(mko_a[5m])",
			// The same extrapolated increase over the 300s window.
			want:   "0.05",
			broken: "<empty result>",
		},
	}

	seed := nonJoinDivergentSeed()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("instant", func(t *testing.T) {
				// A fresh session + server per case: a shared chDB
				// session carries seed state across cases.
				srv, _ := newChDBServer(t, seed)
				vec := runMapKeyOrderInstantAt(t, srv.URL, tc.query, nonJoinEvalTime)
				if len(vec) != 1 {
					t.Fatalf("series count: got %d, want 1 (un-canonicalised answer was %s): %+v",
						len(vec), tc.broken, vec)
				}
				if got := vec[0].Value[1]; got != tc.want {
					t.Errorf("value: got %v, want %q (un-canonicalised answer was %s)",
						got, tc.want, tc.broken)
				}
			})

			if !tc.rangeStable {
				return
			}

			t.Run("range", func(t *testing.T) {
				srv, _ := newChDBServer(t, seed)
				matrix := runRangeModeQueryRange(t, srv.URL, tc.query,
					nonJoinEvalTime, nonJoinEvalTime.Add(nonJoinRangeSpan),
					mapKeyOrderRangeStep)
				if len(matrix) != 1 {
					t.Fatalf("series count: got %d, want 1 (un-canonicalised answer was %s): %+v",
						len(matrix), tc.broken, matrix)
				}
				if len(matrix[0].Values) != nonJoinRangeAnchors {
					t.Fatalf("sample count: got %d, want %d: %+v",
						len(matrix[0].Values), nonJoinRangeAnchors, matrix[0].Values)
				}
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

// TestNonJoinDivergentMapKeyOrder_LabelSetIsUnchanged_ChDB pins the
// other half of the contract: canonicalising the Map must reorder keys
// and nothing else. The response labels are the union of both physical
// orders' keys with their original values — no key dropped, none
// invented, none rewritten.
func TestNonJoinDivergentMapKeyOrder_LabelSetIsUnchanged_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, nonJoinDivergentSeed())
	vec := runMapKeyOrderInstantAt(t, srv.URL, "mko_a", nonJoinEvalTime)
	if len(vec) != 1 {
		t.Fatalf("series count: got %d, want 1: %+v", len(vec), vec)
	}
	want := map[string]string{"__name__": "mko_a", "dc": "eu", "job": "api"}
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

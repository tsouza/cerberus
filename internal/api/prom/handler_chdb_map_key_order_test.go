//go:build chdb

// chDB-backed end-to-end coverage for order-insensitive vector matching.
//
// A ClickHouse `Map(String, String)` compares and groups positionally
// over its (keys, values) arrays, so `map('job','api','dc','eu')` and
// `map('dc','eu','job','api')` are unequal despite carrying the same
// label set. Every V-V binop match key is built from the `Attributes`
// Map — the raw column for default matching, a `mapFilter` projection
// for `on(...)` / `ignoring(...)` — so two operands stored under
// divergent key orders would fail to match.
//
// Divergent key order reaches the store from two independent
// directions: the OTel-CH exporter preserves OTLP wire order and never
// sorts, and cerberus's own `label_replace` / `label_join` lower to CH
// `mapUpdate`, which appends a previously-absent destination key at the
// tail. Both are covered below.
//
// The failure signature is a 200 response carrying `"result": []` — a
// successful-looking empty answer. These tests therefore assert the
// exact series count AND the exact sample values through the real
// `/api/v1/query` and `/api/v1/query_range` handlers; a status-code
// assertion alone would be hollow.
//
// Every case runs against a FRESH chDB session + server (a session
// shared across cases contaminates results across seeds) and is
// asserted in BOTH instant and range mode.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
)

// mapKeyOrderSeedTime is the single sample timestamp every case seeds.
// Instant queries evaluate at this instant; range queries evaluate over
// the window starting here, so the 5-minute LWR staleness window
// carries the sample onto every step anchor.
var mapKeyOrderSeedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// mapKeyOrderRangeStep / mapKeyOrderRangeSpan describe the range-mode
// grid: a 1-minute step over a 4-minute window, i.e. 5 anchors, every
// one of them inside the staleness window of the single seeded sample.
const (
	mapKeyOrderRangeStep    = time.Minute
	mapKeyOrderRangeSpan    = 4 * time.Minute
	mapKeyOrderRangeAnchors = 5
)

// mapKeyOrderSeed builds the two-metric gauge seed: `mko_a` carrying
// aAttrs and `mko_b` carrying bAttrs, one sample each at
// mapKeyOrderSeedTime.
func mapKeyOrderSeed(aAttrs, bAttrs string, aVal, bVal float64) string {
	ts := mapKeyOrderSeedTime.Format("2006-01-02 15:04:05.000000000")
	return gaugeDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge VALUES
    ('mko_a', %s, toDateTime64('%s', 9), %g),
    ('mko_b', %s, toDateTime64('%s', 9), %g);`,
		aAttrs, ts, aVal, bAttrs, ts, bVal)
}

// runMapKeyOrderInstant drives the real `/api/v1/query` handler and
// decodes the vector result.
func runMapKeyOrderInstant(t *testing.T, baseURL, query string) []prom.VectorSample {
	t.Helper()
	reqURL := fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
		baseURL, url.QueryEscape(query), mapKeyOrderSeedTime.Unix())
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatalf("GET %s: %v", reqURL, err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status=%q errorType=%q error=%q", parsed.Status, parsed.ErrorType, parsed.Error)
	}
	if parsed.Data.ResultType != "vector" {
		t.Fatalf("resultType=%q, want vector; body=%s", parsed.Data.ResultType, body)
	}
	rawResult, _ := json.Marshal(parsed.Data.Result)
	var vec []prom.VectorSample
	if err := json.Unmarshal(rawResult, &vec); err != nil {
		t.Fatalf("decode vector: %v (raw=%s)", err, rawResult)
	}
	return vec
}

// mapKeyOrderCase is one query shape asserted in both evaluation modes.
type mapKeyOrderCase struct {
	name string
	// query is the PromQL expression under test.
	query string
	// aAttrs / bAttrs are the CH `map(...)` literals seeded for `mko_a`
	// and `mko_b`. Cases that reproduce the store-side divergence write
	// the same label set under two physical key orders.
	aAttrs, bAttrs string
	aVal, bVal     float64
	// wantValue is the per-sample value the matched series must carry,
	// formatted the way the Prom wire format renders it.
	wantValue string
	// wantLabels is the exact label set of the single output series.
	wantLabels map[string]string
}

func (tc mapKeyOrderCase) assertLabels(t *testing.T, got map[string]string) {
	t.Helper()
	if len(got) != len(tc.wantLabels) {
		t.Fatalf("label count: got %d (%v), want %d (%v)",
			len(got), got, len(tc.wantLabels), tc.wantLabels)
	}
	for k, want := range tc.wantLabels {
		if got[k] != want {
			t.Errorf("label %q: got %q, want %q (full set %v)", k, got[k], want, got)
		}
	}
}

// TestVectorMatch_DivergentMapKeyOrder_ChDB drives every V-V match-key
// branch end to end with operands whose label sets are identical but
// whose physical Map key orders differ.
func TestVectorMatch_DivergentMapKeyOrder_ChDB(t *testing.T) {
	// Ascending and descending physical orders of the SAME two-label
	// set, plus the three-label variants used by the ignoring() cases.
	const (
		twoAsc    = `map('dc', 'eu', 'job', 'api')`
		twoDesc   = `map('job', 'api', 'dc', 'eu')`
		threeAsc  = `map('dc', 'eu', 'env', 'x', 'job', 'api')`
		threeDesc = `map('job', 'api', 'dc', 'eu', 'env', 'y')`
	)
	twoLabels := map[string]string{"dc": "eu", "job": "api"}

	cases := []mapKeyOrderCase{
		{
			name:       "arithmetic_default_matching",
			query:      "mko_a * mko_b",
			aAttrs:     twoDesc,
			bAttrs:     twoAsc,
			aVal:       3,
			bVal:       4,
			wantValue:  "12",
			wantLabels: twoLabels,
		},
		{
			name:  "arithmetic_ignoring",
			query: "mko_a * ignoring(env) mko_b",
			// Three labels so TWO survive the filter in divergent
			// order — a single surviving label matches even unfixed.
			aAttrs:     threeDesc,
			bAttrs:     threeAsc,
			aVal:       3,
			bVal:       4,
			wantValue:  "12",
			wantLabels: twoLabels,
		},
		{
			name:       "setop_and_default_matching",
			query:      "mko_a and mko_b",
			aAttrs:     twoDesc,
			bAttrs:     twoAsc,
			aVal:       3,
			bVal:       4,
			wantValue:  "3",
			wantLabels: map[string]string{"__name__": "mko_a", "dc": "eu", "job": "api"},
		},
		{
			name:       "setop_and_on_labels",
			query:      "mko_a and on(job, dc) mko_b",
			aAttrs:     twoDesc,
			bAttrs:     twoAsc,
			aVal:       3,
			bVal:       4,
			wantValue:  "3",
			wantLabels: map[string]string{"__name__": "mko_a", "dc": "eu", "job": "api"},
		},
		{
			name:       "setop_and_ignoring",
			query:      "mko_a and ignoring(env) mko_b",
			aAttrs:     threeDesc,
			bAttrs:     threeAsc,
			aVal:       3,
			bVal:       4,
			wantValue:  "3",
			wantLabels: map[string]string{"__name__": "mko_a", "dc": "eu", "env": "y", "job": "api"},
		},
		{
			name:  "setop_unless_default_matching",
			query: "mko_a unless mko_b",
			// `unless` inverts the direction: the divergently-ordered
			// right arm MUST suppress the left one, so the correct
			// answer is zero series. Asserted via wantLabels == nil
			// below rather than through the shared one-series path.
			aAttrs: twoDesc,
			bAttrs: twoAsc,
			aVal:   3,
			bVal:   4,
		},
		{
			name:  "label_replace_manufactured_divergence",
			query: `label_replace(mko_a, "dc", "eu", "", "") * mko_b`,
			// BOTH operands are stored in canonical ascending order.
			// The divergence is manufactured inside cerberus: the
			// `mapUpdate` that label_replace lowers to appends the
			// previously-absent `dc` key at the map's tail.
			aAttrs:     `map('job', 'api')`,
			bAttrs:     twoAsc,
			aVal:       3,
			bVal:       4,
			wantValue:  "12",
			wantLabels: twoLabels,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// `unless` asserts the complementary direction — the left
			// arm is suppressed, so the correct result is empty.
			wantSeries := 1
			if tc.wantLabels == nil {
				wantSeries = 0
			}

			t.Run("instant", func(t *testing.T) {
				// A fresh session + server PER CASE: a shared chDB
				// session carries seed state across cases and produces
				// spurious instant-vs-range asymmetries.
				srv, _ := newChDBServer(t, mapKeyOrderSeed(tc.aAttrs, tc.bAttrs, tc.aVal, tc.bVal))
				vec := runMapKeyOrderInstant(t, srv.URL, tc.query)
				if len(vec) != wantSeries {
					t.Fatalf("series count: got %d, want %d: %+v", len(vec), wantSeries, vec)
				}
				if wantSeries == 0 {
					return
				}
				tc.assertLabels(t, vec[0].Metric)
				if got := vec[0].Value[1]; got != tc.wantValue {
					t.Errorf("value: got %v, want %q", got, tc.wantValue)
				}
			})

			t.Run("range", func(t *testing.T) {
				srv, _ := newChDBServer(t, mapKeyOrderSeed(tc.aAttrs, tc.bAttrs, tc.aVal, tc.bVal))
				matrix := runRangeModeQueryRange(t, srv.URL, tc.query,
					mapKeyOrderSeedTime, mapKeyOrderSeedTime.Add(mapKeyOrderRangeSpan),
					mapKeyOrderRangeStep)
				if len(matrix) != wantSeries {
					t.Fatalf("series count: got %d, want %d: %+v", len(matrix), wantSeries, matrix)
				}
				if wantSeries == 0 {
					return
				}
				tc.assertLabels(t, matrix[0].Metric)
				if len(matrix[0].Values) != mapKeyOrderRangeAnchors {
					t.Fatalf("sample count: got %d, want %d: %+v",
						len(matrix[0].Values), mapKeyOrderRangeAnchors, matrix[0].Values)
				}
				for i, v := range matrix[0].Values {
					if got := v[1]; got != tc.wantValue {
						t.Errorf("step %d: value: got %v, want %q", i, got, tc.wantValue)
					}
				}
			})
		})
	}
}

// TestVectorSetOpOr_DivergentMapKeyOrder_ChDB pins the wrong-answer
// direction the empty-result cases cannot reach. Under `or` a failed
// match does not empty the result — it DOUBLES it, because neither arm
// recognises the other as already covering the label set. The correct
// answer is exactly one series, carrying the left arm's value.
func TestVectorSetOpOr_DivergentMapKeyOrder_ChDB(t *testing.T) {
	const query = "mko_a or mko_b"
	seed := mapKeyOrderSeed(
		`map('job', 'api', 'dc', 'eu')`,
		`map('dc', 'eu', 'job', 'api')`,
		3, 4,
	)
	wantLabels := map[string]string{"__name__": "mko_a", "dc": "eu", "job": "api"}

	t.Run("instant", func(t *testing.T) {
		srv, _ := newChDBServer(t, seed)
		vec := runMapKeyOrderInstant(t, srv.URL, query)
		if len(vec) != 1 {
			t.Fatalf("series count: got %d, want 1 (a failed match leaves BOTH arms): %+v",
				len(vec), vec)
		}
		for k, want := range wantLabels {
			if vec[0].Metric[k] != want {
				t.Errorf("label %q: got %q, want %q", k, vec[0].Metric[k], want)
			}
		}
		if got := vec[0].Value[1]; got != "3" {
			t.Errorf("value: got %v, want %q (the left arm wins)", got, "3")
		}
	})

	t.Run("range", func(t *testing.T) {
		srv, _ := newChDBServer(t, seed)
		matrix := runRangeModeQueryRange(t, srv.URL, query,
			mapKeyOrderSeedTime, mapKeyOrderSeedTime.Add(mapKeyOrderRangeSpan),
			mapKeyOrderRangeStep)
		if len(matrix) != 1 {
			t.Fatalf("series count: got %d, want 1 (a failed match leaves BOTH arms): %+v",
				len(matrix), matrix)
		}
		for k, want := range wantLabels {
			if matrix[0].Metric[k] != want {
				t.Errorf("label %q: got %q, want %q", k, matrix[0].Metric[k], want)
			}
		}
		if len(matrix[0].Values) != mapKeyOrderRangeAnchors {
			t.Fatalf("sample count: got %d, want %d: %+v",
				len(matrix[0].Values), mapKeyOrderRangeAnchors, matrix[0].Values)
		}
		for i, v := range matrix[0].Values {
			if got := v[1]; got != "3" {
				t.Errorf("step %d: value: got %v, want %q (the left arm wins)", i, got, "3")
			}
		}
	})
}

// mapKeyOrderSumDDL declares the second metrics table. A selector spans
// both via `merge(otel_metrics_gauge|otel_metrics_sum)`, which is the
// natural producer of ONE join arm carrying two physical key orders for
// a single logical series.
const mapKeyOrderSumDDL = `CREATE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

// TestVectorJoinArmDivergentMapKeyOrder_ChDB pins the arm-side half of
// the match-key class. The join's ON is canonicalised, but each arm also
// collapses its own rows onto a Map-valued series-identity key first. If
// THAT key stays raw, an arm holding both key orders of one logical
// series emits two rows and the canonical ON pairs both of them with the
// other side — a silently duplicated cross-product.
//
// The duplication is invisible in the bare binop (both output rows carry
// the same label set, so the wire assembly collapses them); only an
// enclosing aggregation exposes it, so the sum is the load-bearing
// assertion here.
func TestVectorJoinArmDivergentMapKeyOrder_ChDB(t *testing.T) {
	ts := mapKeyOrderSeedTime.Format("2006-01-02 15:04:05.000000000")
	// `vja_a` is written once per table under OPPOSITE key orders — one
	// logical series, two physical rows. `vja_b` is the other arm.
	seed := gaugeDDL + "\n" + mapKeyOrderSumDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge VALUES
    ('vja_a', map('job', 'api', 'dc', 'eu'), toDateTime64('%s', 9), 3),
    ('vja_b', map('job', 'api', 'dc', 'eu'), toDateTime64('%s', 9), 5);
INSERT INTO otel_metrics_sum VALUES
    ('vja_a', map('dc', 'eu', 'job', 'api'), toDateTime64('%s', 9), 3);`, ts, ts, ts)

	// Both shapes route through the vector-join emitter, whose arms this
	// covers. One matched series at 3*5, so a duplicated pairing shows
	// up as a multiple of 15.
	cases := []struct{ query, want string }{
		{"sum(vja_a * vja_b)", "15"},
		{"sum(vja_a * on(job) group_left vja_b)", "15"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			srv, _ := newChDBServer(t, seed)
			vec := runMapKeyOrderInstant(t, srv.URL, tc.query)
			if len(vec) != 1 {
				t.Fatalf("series count: got %d, want 1: %+v", len(vec), vec)
			}
			if got := vec[0].Value[1]; got != tc.want {
				t.Errorf("value: got %v, want %q (a larger value means the arm "+
					"emitted one row per physical key order)", got, tc.want)
			}
		})
	}
}

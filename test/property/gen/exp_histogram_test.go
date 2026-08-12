package gen

import (
	"math"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
)

// TestExpHistogramMetricNamesRoute pins the generator's metric-name
// pool to the routing rule the whole native-histogram property test
// depends on.
//
// If a name stops matching [schema.Metrics.IsExpHistogramMetric],
// cerberus reads the classic-histogram table instead of the
// exponential one. The differential does NOT go quiet on that — the
// oracle reads the in-memory mirror and never consults the suffix, so
// it still answers rows while cerberus answers none or fails on a
// missing table, and the comparator reports the mismatch. This test
// exists to turn that into a one-line diagnosis in the generator's own
// package rather than a puzzling table-not-found error surfacing from
// a shrunk property counterexample.
func TestExpHistogramMetricNamesRoute(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	if len(ExpHistogramMetricNamePool) == 0 {
		t.Fatal("ExpHistogramMetricNamePool is empty: the generator would draw no series")
	}
	for _, name := range ExpHistogramMetricNamePool {
		if !s.IsExpHistogramMetric(name) {
			t.Errorf("metric %q does not route to the exponential-histogram table "+
				"(schema suffix %q): every generated query would read the wrong table and return no rows",
				name, s.ExpHistogramSuffix)
		}
	}
}

// TestExpHistogramDatasetInvariants asserts the properties every drawn
// dataset must hold for the differential comparison to be well-posed.
//
// The DDL ⇔ model lock-step the Dataset contract requires is not
// re-derived here (re-implementing the renderer to check the renderer
// proves nothing); it is verified end-to-end by
// TestPromQL_Property_NativeHistogram, where a mirror that disagreed
// with the seeded rows would surface immediately as oracle/cerberus
// drift. What this test pins is the shape of the drawn data itself.
func TestExpHistogramDatasetInvariants(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	rapid.Check(t, func(rt *rapid.T) {
		d := ExpHistogramDataset().Draw(rt, "dataset")

		if d.Metrics == nil || len(d.Metrics.Series) == 0 {
			rt.Fatal("dataset drew no series")
		}
		if d.Logs != nil {
			rt.Fatal("metrics dataset must leave the Logs mirror nil")
		}

		insertCount := strings.Count(d.DDL, "INSERT INTO "+ExpHistogramTableName)
		if insertCount != len(d.Metrics.Series) {
			rt.Fatalf("DDL has %d INSERTs for %d series", insertCount, len(d.Metrics.Series))
		}

		seen := map[string]struct{}{}
		for _, ser := range d.Metrics.Series {
			if !s.IsExpHistogramMetric(ser.MetricName) {
				rt.Fatalf("series metric %q would not route to the exp-histogram table", ser.MetricName)
			}
			key := ser.MetricName + labelKey(ser.Labels)
			if _, dup := seen[key]; dup {
				rt.Fatalf("duplicate series identity %q: the two would collapse into one", key)
			}
			seen[key] = struct{}{}

			if len(ser.Points) == 0 {
				rt.Fatalf("series %q drew no points", key)
			}
			var prevTs int64
			for i, p := range ser.Points {
				if p.Histogram == nil {
					rt.Fatalf("series %q point %d carries no histogram payload", key, i)
				}
				if i > 0 && p.TimestampMs <= prevTs {
					rt.Fatalf("series %q point %d timestamp %d does not advance past %d",
						key, i, p.TimestampMs, prevTs)
				}
				prevTs = p.TimestampMs

				h := p.Histogram
				// Count > 0 is the documented accept-set bound: every
				// native-histogram function divides by Count or walks a
				// cumulative array whose total is Count.
				if h.Count == 0 {
					rt.Fatalf("series %q point %d drew an empty histogram", key, i)
				}
				want := h.ZeroCount + sumBucketCounts(h.PositiveBucketCounts) + sumBucketCounts(h.NegativeBucketCounts)
				if h.Count != want {
					rt.Fatalf("series %q point %d: Count=%d but buckets total %d — an inconsistent "+
						"histogram has no defined answer to differ over", key, i, h.Count, want)
				}
				if math.IsNaN(h.Sum) || math.IsInf(h.Sum, 0) {
					rt.Fatalf("series %q point %d: Sum is not finite (%v)", key, i, h.Sum)
				}
			}
		}
	})
}

// TestBucketMidpointSumHandDerived pins the generated Sum against
// bucket midpoints worked out by hand rather than by re-running the
// implementation's own expression.
//
// Each expectation is derived from base = 2^(2^-scale) and the
// geometric midpoint base^(offset+i+0.5) of bucket offset+i, then
// simplified algebraically so the assertion does not restate the code:
// e.g. at scale 0 (base 2) the bucket at index 1 has midpoint
// 2^1.5 = 2·√2.
func TestBucketMidpointSumHandDerived(t *testing.T) {
	const tolerance = 1e-12

	cases := []struct {
		name      string
		scale     int32
		posOffset int32
		pos       []uint64
		negOffset int32
		neg       []uint64
		want      float64
	}{
		{
			// base = 2^(2^1) = 4. Bucket 0 spans (4^0, 4^1] = (1, 4],
			// geometric midpoint 4^0.5 = 2 exactly. One observation.
			name: "coarse scale gives an exact integer midpoint",
			// scale -1 -> base 4
			scale: -1, posOffset: 0, pos: []uint64{1},
			want: 2,
		},
		{
			// base = 2. Bucket 0 spans (1, 2], midpoint 2^0.5 = √2.
			name:  "base-2 bucket 0 midpoint is sqrt(2)",
			scale: 0, posOffset: 0, pos: []uint64{1},
			want: math.Sqrt2,
		},
		{
			// base = 2, offset 1: buckets 1 and 2, midpoints
			// 2^1.5 = 2·√2 and 2^2.5 = 4·√2.
			// 2·(2√2) + 3·(4√2) = 4√2 + 12√2 = 16√2.
			name:  "base-2 offset-1 two buckets",
			scale: 0, posOffset: 1, pos: []uint64{2, 3},
			want: 16 * math.Sqrt2,
		},
		{
			// Negative buckets subtract. base = 4, bucket 0 midpoint 2,
			// so 3 observations at -2 sum to -6.
			name:  "negative buckets subtract",
			scale: -1, negOffset: 0, neg: []uint64{3},
			want: -6,
		},
		{
			// base = 4. Positive bucket 0 midpoint 2 (×2 = 4);
			// negative bucket 0 midpoint 2 (×1 = -2). Net 2.
			name:  "positive and negative buckets cancel partially",
			scale: -1, posOffset: 0, pos: []uint64{2}, negOffset: 0, neg: []uint64{1},
			want: 2,
		},
		{
			// No buckets at all: the zero bucket sits at 0 and
			// contributes nothing, so the estimate is 0.
			name:  "zero-bucket-only histogram sums to zero",
			scale: 0,
			want:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bucketMidpointSum(tc.scale, tc.posOffset, tc.pos, tc.negOffset, tc.neg)
			if math.Abs(got-tc.want) > tolerance*math.Max(1, math.Abs(tc.want)) {
				t.Errorf("bucketMidpointSum = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRenderExpHistogramRowHandDerived pins the exact INSERT tuple the
// generator emits. The literal below is written out by hand from the
// column order in renderExpHistogramDDL, so a silent reordering of the
// value list against the column list — which would seed every field
// into the wrong column and make the whole differential compare two
// wrong answers — fails here.
func TestRenderExpHistogramRowHandDerived(t *testing.T) {
	p := property.Point{
		TimestampMs: time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC).UnixMilli(),
		Histogram: &property.NativeHistogram{
			Count:                6,
			Sum:                  1.5,
			Scale:                -1,
			ZeroCount:            0,
			PositiveOffset:       2,
			PositiveBucketCounts: []uint64{1, 2, 3},
			NegativeOffset:       -1,
			NegativeBucketCounts: nil,
		},
	}

	const want = `('request_latency_exp_hist', map('job', 'api'), ` +
		`toDateTime64('2026-05-13 12:00:00.000', 9), 6, 1.5, -1, 0, 2, [1, 2, 3], -1, [])`

	got := renderExpHistogramRow("request_latency_exp_hist", map[string]string{"job": "api"}, p)
	if got != want {
		t.Errorf("renderExpHistogramRow:\n got %s\nwant %s", got, want)
	}
}

// expHistogramAcceptSet is the seven native-histogram functions the
// query generator may draw. Declared here rather than inferred from a
// sweep so the accept-set assertions below are deterministic.
var expHistogramAcceptSet = []string{
	"histogram_count",
	"histogram_sum",
	"histogram_avg",
	"histogram_stddev",
	"histogram_stdvar",
	"histogram_quantile",
	"histogram_fraction",
}

// TestExpHistogramValueFnVocabulary pins the generator's function
// vocabulary against the accept-set, deterministically.
//
// A generator that silently lost a function — a typo in the pool, or a
// name PromQL does not define — would leave that function unexercised
// while the differential still reported green. Asserting reachability
// from a random sweep instead would make this test flaky: a specific
// value function is drawn with probability 1/15 per query, so it is
// missed across 100 draws roughly once in a thousand runs. The
// vocabulary is checked exactly, and the three-way shape draw that
// selects between the pools is left to the sweep below, where a miss
// has probability (2/3)^100.
func TestExpHistogramValueFnVocabulary(t *testing.T) {
	for _, fn := range expHistogramValueFns {
		if parser.Functions[fn] == nil {
			t.Errorf("value-function pool names %q, which PromQL does not define", fn)
		}
	}
	for _, fn := range []string{"histogram_quantile", "histogram_fraction"} {
		if parser.Functions[fn] == nil {
			t.Errorf("PromQL does not define %q", fn)
		}
	}

	got := append(append([]string(nil), expHistogramValueFns...), "histogram_quantile", "histogram_fraction")
	sort.Strings(got)
	want := append([]string(nil), expHistogramAcceptSet...)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("generator vocabulary = %v, want %v", got, want)
	}
}

// TestExpHistogramQueryShapes asserts every generated query is one of
// the seven native-histogram functions applied to a bare selector, and
// that all three call shapes are reachable.
func TestExpHistogramQueryShapes(t *testing.T) {
	accept := make(map[string]struct{}, len(expHistogramAcceptSet))
	for _, fn := range expHistogramAcceptSet {
		accept[fn] = struct{}{}
	}

	var sawValueFn, sawQuantile, sawFraction bool
	rapid.Check(t, func(rt *rapid.T) {
		d := ExpHistogramDataset().Draw(rt, "dataset")
		q := ExpHistogramQuery(d).Draw(rt, "query")
		if q.String == "" {
			rt.Fatal("generated an empty query")
		}
		fn, rest, ok := strings.Cut(q.String, "(")
		if !ok {
			rt.Fatalf("query %q is not a function call", q.String)
		}
		if _, known := accept[fn]; !known {
			rt.Fatalf("query %q uses function %q outside the accept-set", q.String, fn)
		}
		// The histogram argument is always a bare selector, which is
		// cerberus's own accept-set for these functions: no nested call
		// may appear after the scalar arguments.
		if strings.Contains(rest, "(") {
			rt.Fatalf("query %q wraps its histogram argument in a call; only a bare selector is supported", q.String)
		}
		switch fn {
		case "histogram_quantile":
			sawQuantile = true
		case "histogram_fraction":
			sawFraction = true
		default:
			sawValueFn = true
		}
	})

	if !sawValueFn || !sawQuantile || !sawFraction {
		t.Errorf("not every call shape was drawn: valueFn=%v quantile=%v fraction=%v",
			sawValueFn, sawQuantile, sawFraction)
	}
}

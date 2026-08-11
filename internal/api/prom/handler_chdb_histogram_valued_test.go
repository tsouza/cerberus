//go:build chdb

// chDB-backed END-TO-END coverage for the histogram-VALUED PromQL shapes
// (#1926 / #1967): a bare exponential-histogram selector, `sum()` over
// one, and `rate()` over one.
//
// Every other exp-histogram test in this repo wraps the selector in a
// scalar-returning function (histogram_quantile / histogram_count /
// histogram_sum / …), so none of them ever produced a `histogram` key on
// the wire. That left the whole histogram-valued path — the nine
// Histogram*Column outputs, their decode into chclient.HistogramValue,
// and the bucket walk that renders them — without a single test that ran
// it end to end. The gap was not theoretical: `rate()` over an
// exponential histogram could not decode at all, because the lowering
// emitted Float64 counts while the decode destination was uint64, and
// nothing executed the pair together (issue #1967).
//
// These tests run the real handler over a real chDB execution and assert
// the emitted Prometheus wire JSON, so a regression anywhere along that
// chain fails here.

package prom_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// histValuedDDL is the stock OTel-CH exponential-histogram table — the
// DDL the default schema targets, with no ZeroThreshold column (the
// upstream exporter does not persist the OTLP field).
const histValuedDDL = `CREATE TABLE otel_metrics_exponential_histogram (
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
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);`

// histValuedSeed is the two-scrape series test/spec/promql/
// exp_histogram_rate.txtar works through by hand against reference
// Prometheus's extrapolatedRate. Reusing the identical rows means the
// rate() expectations below are checked against that independent
// calculation rather than against cerberus's own output.
const histValuedSeed = histValuedDDL + `
INSERT INTO otel_metrics_exponential_histogram VALUES
    ('latency_exp_hist', map('service', 'api'), toDateTime64('2025-12-31 23:59:01', 9),  6,  3.0, 0, 1, 0, [ 2,  3], 0, []),
    ('latency_exp_hist', map('service', 'api'), toDateTime64('2026-01-01 00:00:01', 9), 30, 15.0, 0, 1, 0, [14, 15], 0, []);`

// histValuedEvalTime is the fixture's evaluation anchor: 00:00:01, so a
// [5m] range spans (23:55:01, 00:00:01] and covers both scrapes.
var histValuedEvalTime = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// newHistValuedServer mounts the prom handler over a chDB session seeded
// with histValuedSeed.
func newHistValuedServer(t *testing.T) *httptest.Server {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, histValuedSeed)
	h := prom.New(c, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// wireHistogram is the decoded `histogram` value of one vector result:
// the `[timestamp, {count, sum, buckets}]` pair upstream Prometheus
// emits. Buckets are `[boundaries, lower, upper, count]` tuples.
type wireHistogram struct {
	Count   string
	Sum     string
	Buckets [][4]any
}

// queryHistogramResult issues an instant query and returns the single
// vector result's decoded histogram plus its metric labels. It fails the
// test unless the response carries exactly one histogram-VALUED series —
// a float-valued answer means the shape silently collapsed to a scalar,
// which is the regression these tests exist to catch.
func queryHistogramResult(t *testing.T, srv *httptest.Server, query string) (map[string]string, wireHistogram) {
	t.Helper()

	reqURL := fmt.Sprintf("%s/api/v1/query?query=%s&time=%s",
		srv.URL, url.QueryEscape(query), histValuedEvalTime.Format(time.RFC3339))
	resp, err := http.Get(reqURL) //nolint:noctx // test-local request against httptest
	if err != nil {
		t.Fatalf("GET /api/v1/query %q: %v", query, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/v1/query %q: status %d\nbody: %s", query, resp.StatusCode, body)
	}

	var body struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric    map[string]string `json:"metric"`
				Value     []any             `json:"value"`
				Histogram []any             `json:"histogram"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response for %q: %v", query, err)
	}
	if body.Status != "success" {
		t.Fatalf("query %q returned status %q", query, body.Status)
	}
	if len(body.Data.Result) != 1 {
		t.Fatalf("query %q returned %d series, want 1", query, len(body.Data.Result))
	}
	res := body.Data.Result[0]
	if len(res.Histogram) == 0 {
		t.Fatalf("query %q returned a float-valued sample (%v), not a histogram — "+
			"the histogram-valued shape collapsed to a scalar", query, res.Value)
	}
	if len(res.Value) != 0 {
		t.Errorf("query %q returned BOTH a value and a histogram: %v", query, res.Value)
	}

	// histogram is [timestamp, {count, sum, buckets}].
	if len(res.Histogram) != 2 {
		t.Fatalf("query %q histogram pair has %d elements, want 2", query, len(res.Histogram))
	}
	raw, err := json.Marshal(res.Histogram[1])
	if err != nil {
		t.Fatalf("re-marshal histogram body: %v", err)
	}
	var hist wireHistogram
	if err := json.Unmarshal(raw, &hist); err != nil {
		t.Fatalf("decode histogram body for %q: %v", query, err)
	}
	return res.Metric, hist
}

func TestQuery_HistogramValued_ChDB(t *testing.T) {
	srv := newHistValuedServer(t)

	// The three positive-bucket ladders below all come from the same
	// seeded rows, so they are named once rather than repeated per case.
	latestBuckets := [][4]any{
		{float64(3), "0", "0", "1"},
		{float64(0), "1", "2", "14"},
		{float64(0), "2", "4", "15"},
	}

	cases := []struct {
		name  string
		query string
		// wantMetric is the FULL label set the shape publishes. A bare
		// selector keeps `__name__` and the series' labels; every
		// derived shape drops `__name__` (Prometheus's "drop __name__ on
		// derived samples" rule); an ungrouped `sum()` additionally
		// reduces across series and so publishes no labels at all.
		wantMetric map[string]string
		want       wireHistogram
	}{
		{
			// The latest scrape, verbatim: Count 30, Sum 15.0,
			// Scale 0 (base 2), PositiveOffset 0, buckets [14, 15].
			// Bucket i spans (2^i, 2^(i+1)]. ZeroCount 1 with no
			// persisted threshold puts the zero band at the point 0,
			// clamped to [0, 0] because the distribution is
			// positive-only.
			name:       "bare selector",
			query:      "latency_exp_hist",
			wantMetric: map[string]string{"__name__": "latency_exp_hist", "service": "api"},
			want:       wireHistogram{Count: "30", Sum: "15", Buckets: latestBuckets},
		},
		{
			// An ungrouped sum() reduces ACROSS series, so it publishes
			// no labels at all. One series in, so the bucket ladder is
			// unchanged.
			name:       "sum",
			query:      "sum(latency_exp_hist)",
			wantMetric: map[string]string{},
			want:       wireHistogram{Count: "30", Sum: "15", Buckets: latestBuckets},
		},
		{
			// sum by (...) keeps the grouping labels, which is what
			// exercises the HistogramProjection's GroupBy aliases
			// rather than its degenerate no-group form.
			name:       "sum by",
			query:      "sum by (service) (latency_exp_hist)",
			wantMetric: map[string]string{"service": "api"},
			want:       wireHistogram{Count: "30", Sum: "15", Buckets: latestBuckets},
		},
		{
			// The hand-worked expectation from
			// exp_histogram_rate.txtar: every field scaled by
			// 1.25/300 = 1/240. Count 24/240 = 0.1, Sum 12/240 =
			// 0.05, buckets [12/240, 12/240]. ZeroCount telescopes to
			// 0, so the zero bucket drops out entirely. rate() reduces
			// along TIME, not across series, so the labels survive.
			name:       "rate",
			query:      "rate(latency_exp_hist[5m])",
			wantMetric: map[string]string{"service": "api"},
			want: wireHistogram{
				Count: "0.1", Sum: "0.05",
				Buckets: [][4]any{
					{float64(0), "1", "2", "0.05"},
					{float64(0), "2", "4", "0.05"},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			metric, got := queryHistogramResult(t, srv, tc.query)

			if got.Count != tc.want.Count {
				t.Errorf("count = %q, want %q", got.Count, tc.want.Count)
			}
			if got.Sum != tc.want.Sum {
				t.Errorf("sum = %q, want %q", got.Sum, tc.want.Sum)
			}
			assertMetric(t, metric, tc.wantMetric)
			assertBuckets(t, got.Buckets, tc.want.Buckets)
		})
	}
}

// assertBuckets compares the decoded bucket tuples element by element.
func assertBuckets(t *testing.T, got, want [][4]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d buckets %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		for j := range want[i] {
			if fmt.Sprint(got[i][j]) != fmt.Sprint(want[i][j]) {
				t.Errorf("bucket %d field %d = %v, want %v (bucket %v)", i, j, got[i][j], want[i][j], got[i])
			}
		}
	}
}

// assertMetric compares the full published label set, so a shape that
// silently gains or loses a label fails rather than passing on the
// handful of keys a test happened to name.
func assertMetric(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("metric = %v, want %v", got, want)
		return
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("metric[%q] = %q, want %q (full: %v)", k, got[k], v, got)
		}
	}
}

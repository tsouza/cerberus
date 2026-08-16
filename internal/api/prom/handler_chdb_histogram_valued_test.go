//go:build chdb

// chDB-backed END-TO-END coverage for the histogram-VALUED PromQL shapes
// (#1926 / #1967 / #2224): a bare exponential-histogram selector, `sum()`
// over one, and every histogram-valued range function over one.
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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"

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
    ('latency_exp_hist', map('service', 'api'), toDateTime64('2026-01-01 00:00:01', 9), 30, 15.0, 0, 1, 0, [14, 15], 0, []);

INSERT INTO otel_metrics_exponential_histogram VALUES
    ('range_fn_exp_hist', map('case', 'reset'), toDateTime64('2025-12-31 23:58:01', 9),  10,  5.0, 0, 1, 0, [ 7,  2], 0, []),
    ('range_fn_exp_hist', map('case', 'reset'), toDateTime64('2025-12-31 23:59:01', 9), 100, 50.0, 0, 1, 0, [60, 39], 0, []),
    ('range_fn_exp_hist', map('case', 'reset'), toDateTime64('2026-01-01 00:00:01', 9),  20, 10.0, 0, 1, 0, [ 8, 11], 0, []);

INSERT INTO otel_metrics_exponential_histogram VALUES
    ('scale_shift_exp_hist', map('case', 'downscale'), toDateTime64('2025-12-31 23:59:01', 9),  6, 3.0, 1, 0, 0, [2, 4], 0, []),
    ('scale_shift_exp_hist', map('case', 'downscale'), toDateTime64('2026-01-01 00:00:01', 9), 10, 5.0, 0, 0, 0, [10],   0, []);`

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

func TestQuery_HistogramIncompatibleScalarBinopsDropSamples_ChDB(t *testing.T) {
	srv := newHistValuedServer(t)

	queries := []string{
		`latency_exp_hist + 1`,
		`2 / latency_exp_hist`,
		`latency_exp_hist > bool 1`,
	}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			reqURL := fmt.Sprintf("%s/api/v1/query?query=%s&time=%s",
				srv.URL, url.QueryEscape(query), histValuedEvalTime.Format(time.RFC3339))
			resp, err := http.Get(reqURL) //nolint:noctx // test-local request against httptest
			if err != nil {
				t.Fatalf("GET /api/v1/query %q: %v", query, err)
			}
			defer func() { _ = resp.Body.Close() }()

			var body struct {
				Status string `json:"status"`
				Data   struct {
					ResultType string            `json:"resultType"`
					Result     []json.RawMessage `json:"result"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode response for %q: %v", query, err)
			}
			if resp.StatusCode != http.StatusOK || body.Status != "success" {
				t.Fatalf("query %q returned HTTP %d status %q, want accepted empty result", query, resp.StatusCode, body.Status)
			}
			if body.Data.ResultType != "vector" || len(body.Data.Result) != 0 {
				t.Fatalf("query %q returned resultType=%q with %d samples, want empty vector",
					query, body.Data.ResultType, len(body.Data.Result))
			}
		})
	}
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
			name:       "label_replace bare selector",
			query:      `label_replace(latency_exp_hist, "service_copy", "$1-copy", "service", "(.*)")`,
			wantMetric: map[string]string{"__name__": "latency_exp_hist", "service": "api", "service_copy": "api-copy"},
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
			name:       "label_replace sum by",
			query:      `label_replace(sum by (service) (latency_exp_hist), "service_copy", "$1-copy", "service", "(.*)")`,
			wantMetric: map[string]string{"service": "api", "service_copy": "api-copy"},
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
		{
			name:       "label_replace rate",
			query:      `label_replace(rate(latency_exp_hist[5m]), "service_copy", "$1-copy", "service", "(.*)")`,
			wantMetric: map[string]string{"service": "api", "service_copy": "api-copy"},
			want: wireHistogram{
				Count: "0.1", Sum: "0.05",
				Buckets: [][4]any{
					{float64(0), "1", "2", "0.05"},
					{float64(0), "2", "4", "0.05"},
				},
			},
		},
		{
			// delta is the gauge sibling of increase: it subtracts the
			// endpoint histograms without reset correction, then applies the
			// ordinary boundary extrapolation. The raw Count/Sum/bucket delta
			// is 24/12/[12,12]; the gauge factor is 1.5 (there is no counter
			// duration-to-zero clamp), yielding 36/18/[18,18].
			name:       "delta",
			query:      "delta(latency_exp_hist[5m])",
			wantMetric: map[string]string{"service": "api"},
			want: wireHistogram{
				Count: "36", Sum: "18",
				Buckets: [][4]any{
					{float64(0), "1", "2", "18"},
					{float64(0), "2", "4", "18"},
				},
			},
		},
		{
			// irate reads only the final pair and divides its raw increase
			// by their actual 60-second separation. No reset occurs here, so
			// 24/12/[12,12] becomes 0.4/0.2/[0.2,0.2].
			name:       "irate",
			query:      "irate(latency_exp_hist[5m])",
			wantMetric: map[string]string{"service": "api"},
			want: wireHistogram{
				Count: "0.4", Sum: "0.2",
				Buckets: [][4]any{
					{float64(0), "1", "2", "0.2"},
					{float64(0), "2", "4", "0.2"},
				},
			},
		},
		{
			// idelta uses the same final pair as irate, but it is a gauge
			// difference: no reset correction and no per-second division.
			name:       "idelta",
			query:      "idelta(latency_exp_hist[5m])",
			wantMetric: map[string]string{"service": "api"},
			want: wireHistogram{
				Count: "24", Sum: "12",
				Buckets: [][4]any{
					{float64(0), "1", "2", "12"},
					{float64(0), "2", "4", "12"},
				},
			},
		},
		{
			// Three samples make endpoint selection observable. delta must
			// ignore the middle Count=100 reset-shaped sample and subtract
			// first from last (10/5/[1,9]), then apply the 1.25 gauge
			// extrapolation factor derived from all three timestamps.
			name:       "delta selects endpoints",
			query:      "delta(range_fn_exp_hist[5m])",
			wantMetric: map[string]string{"case": "reset"},
			want: wireHistogram{
				Count: "12.5", Sum: "6.25",
				Buckets: [][4]any{
					{float64(0), "1", "2", "1.25"},
					{float64(0), "2", "4", "11.25"},
				},
			},
		},
		{
			// irate must ignore the oldest sample, detect the whole-histogram
			// reset from Count=100 to Count=20, and retain the current
			// histogram before dividing every field by the 60-second gap.
			name:       "irate selects last two and corrects reset",
			query:      "irate(range_fn_exp_hist[5m])",
			wantMetric: map[string]string{"case": "reset"},
			want: wireHistogram{
				Count: "0.3333333333333333", Sum: "0.16666666666666666",
				Buckets: [][4]any{
					{float64(3), "0", "0", "0.016666666666666666"},
					{float64(0), "1", "2", "0.13333333333333333"},
					{float64(0), "2", "4", "0.18333333333333332"},
				},
			},
		},
		{
			// idelta reads the same final pair but deliberately ignores the
			// counter reset: the negative gauge difference must survive.
			name:       "idelta selects last two and ignores reset",
			query:      "idelta(range_fn_exp_hist[5m])",
			wantMetric: map[string]string{"case": "reset"},
			want: wireHistogram{
				Count: "-80", Sum: "-40",
				Buckets: [][4]any{
					{float64(0), "1", "2", "-52"},
					{float64(0), "2", "4", "-28"},
				},
			},
		},
		{
			// The final pair uses Scale 1 then Scale 0. idelta must downscale
			// the previous [2,4] ladder to [6] before subtracting it from
			// the current [10] ladder, rather than comparing bucket positions
			// from incompatible scales directly.
			name:       "idelta reconciles last-two scales",
			query:      "idelta(scale_shift_exp_hist[5m])",
			wantMetric: map[string]string{"case": "downscale"},
			want: wireHistogram{
				Count: "4", Sum: "2",
				Buckets: [][4]any{{float64(0), "1", "2", "4"}},
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

// TestQuery_HistogramValuedLabelReplaceCollision_ChDB pins the error half
// of label_replace's contract. Rewriting the only distinguishing label of
// two histogram series onto one value must abort instead of merging or
// publishing duplicate label sets.
func TestQuery_HistogramValuedLabelReplaceCollision_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	c.Seed(t, histValuedSeed+`
INSERT INTO otel_metrics_exponential_histogram VALUES
    ('latency_exp_hist', map('service', 'web'), toDateTime64('2025-12-31 23:59:01', 9),  4,  2.0, 0, 1, 0, [1, 2], 0, []),
    ('latency_exp_hist', map('service', 'web'), toDateTime64('2026-01-01 00:00:01', 9), 20, 10.0, 0, 1, 0, [9, 10], 0, []);`)
	h := prom.New(c, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const query = `label_replace(latency_exp_hist, "service", "same", "service", ".*")`
	status, body := getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%s",
		srv.URL, url.QueryEscape(query), histValuedEvalTime.Format(time.RFC3339)))
	assertDuplicateLabelsetRejected(t, body, status, query)
}

// TestQuery_FloatOnlyFunctionDropsNativeHistogram_ChDB pins the HTTP contract
// behind issue #2221. The same handler session carries one native histogram
// and one ordinary float series: abs() must accept both, drop the histogram
// sample, and continue transforming the representable float sample normally.
// OTel-CH stores the two sample kinds in different physical tables, so a
// single logical series containing both kinds is not representable; exercising
// both routes in one session is the closest storage-faithful mixed control.
func TestQuery_FloatOnlyFunctionDropsNativeHistogram_ChDB(t *testing.T) {
	stamp := histValuedEvalTime.Format("2006-01-02 15:04:05.000000000")
	seed := gaugeDDL + histValuedSeed + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge VALUES
    ('temperature', map('service', 'api'), toDateTime64('%s', 9), -4.0);`, stamp)
	srv, _ := newChDBServer(t, seed)

	cases := []struct {
		query     string
		wantCount int
		wantValue string
	}{
		{query: "abs(latency_exp_hist)", wantCount: 0},
		{query: "abs(temperature)", wantCount: 1, wantValue: "4"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			reqURL := fmt.Sprintf("%s/api/v1/query?query=%s&time=%s",
				srv.URL, url.QueryEscape(tc.query), histValuedEvalTime.Format(time.RFC3339))
			resp, err := http.Get(reqURL) //nolint:noctx // test-local request against httptest
			if err != nil {
				t.Fatalf("GET /api/v1/query: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			var body struct {
				Status string `json:"status"`
				Data   struct {
					ResultType string `json:"resultType"`
					Result     []struct {
						Value []json.RawMessage `json:"value"`
					} `json:"result"`
				} `json:"data"`
				ErrorType string `json:"errorType"`
				Error     string `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.StatusCode != http.StatusOK || body.Status != "success" {
				t.Fatalf("status=%d body.status=%q errorType=%q error=%q",
					resp.StatusCode, body.Status, body.ErrorType, body.Error)
			}
			if body.Data.ResultType != "vector" {
				t.Fatalf("resultType = %q, want vector", body.Data.ResultType)
			}
			if len(body.Data.Result) != tc.wantCount {
				t.Fatalf("result count = %d, want %d", len(body.Data.Result), tc.wantCount)
			}
			if tc.wantCount == 0 {
				return
			}
			if len(body.Data.Result[0].Value) != 2 {
				t.Fatalf("value pair = %s, want [timestamp, value]", body.Data.Result[0].Value)
			}
			var gotValue string
			if err := json.Unmarshal(body.Data.Result[0].Value[1], &gotValue); err != nil {
				t.Fatalf("decode sample value: %v", err)
			}
			if gotValue != tc.wantValue {
				t.Fatalf("sample value = %q, want %q", gotValue, tc.wantValue)
			}
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

// TestHistogramProjection_RuntimeColumnTypes_ChDB pins the half of the
// decode contract that neither the emitter's own tests nor the
// scan-contract test can see: what ClickHouse actually REPORTS for the
// nine output columns.
//
// The emitter's casts are pinned as SQL TEXT (chsql's shape test and the
// `-- sql --` cells of test/spec/promql/exp_histogram_*.txtar), and
// internal/chclient's scan-contract test pins that each named type scans
// into its HistogramValue field. Neither executes the expression. That
// matters because the chDB-backed handler tests CANNOT catch a reverted
// cast on their own: chdb-go's driver coerces an int64 or a text-rendered
// array into the destination, so a UInt64 HistogramCount would still
// decode here while failing on a real ClickHouse — the exact
// chDB-coerces / prod-is-strict split that produced #1967.
//
// Asserting toTypeName over the emitted SQL closes that: chDB is
// ClickHouse's own engine, so its type inference IS ClickHouse's.
func TestHistogramProjection_RuntimeColumnTypes_ChDB(t *testing.T) {
	c := chclienttest.NewChDB(t)
	// promql.Lower anchors an instant query to now64(), so this fixture
	// is seeded RELATIVE TO NOW rather than reusing histValuedSeed's
	// fixed 2026-01-01 rows — those fall outside the lookback and the
	// projection would report types over an empty result. Two scrapes
	// 60s apart keep rate()'s two-sample floor satisfied.
	c.Seed(t, histValuedDDL+`
INSERT INTO otel_metrics_exponential_histogram VALUES
    ('typecheck_exp_hist', map('service', 'api'), now64(9) - toIntervalSecond(60),  6,  3.0, 0, 1, 0, [ 2,  3], 0, []),
    ('typecheck_exp_hist', map('service', 'api'), now64(9),                        30, 15.0, 0, 1, 0, [14, 15], 0, []);`)

	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	// The pinned type of each of the nine outputs, in projection order.
	wantTypes := []string{
		"Float64", // HistogramCount
		"Float64", // HistogramSum
		"Int32",   // HistogramScale
		"Float64", // HistogramZeroThreshold
		"Float64", // HistogramZeroCount
		"Int32",   // HistogramPositiveOffset
		"Array(Float64)",
		"Int32", // HistogramNegativeOffset
		"Array(Float64)",
	}

	// Both shapes matter: a bare selector forwards the physical OTel-CH
	// columns (UInt64 / Array(UInt64)) and rate() derives Float64 ones,
	// so before the pin these two disagreed. They must now report the
	// SAME nine types.
	for _, tc := range []struct{ name, query string }{
		{"bare selector", "typecheck_exp_hist"},
		{"rate", "rate(typecheck_exp_hist[5m])"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.query, err)
			}
			plan, err := promql.Lower(t.Context(), expr, schema.DefaultOTelMetrics())
			if err != nil {
				t.Fatalf("lower %q: %v", tc.query, err)
			}
			sql, args, err := chsql.Emit(t.Context(), plan)
			if err != nil {
				t.Fatalf("emit %q: %v", tc.query, err)
			}

			// Wrap the emitted SQL without touching its parameter
			// order, so the recorded args still bind positionally.
			var sb strings.Builder
			sb.WriteString("SELECT arrayStringConcat([")
			for i, col := range histogramOutputColumns {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString("toTypeName(`" + col + "`)")
			}
			sb.WriteString("], '|') FROM (")
			sb.WriteString(sql)
			sb.WriteString(") LIMIT 1")

			got, err := c.QueryStrings(t.Context(), sb.String(), args...)
			if err != nil {
				t.Fatalf("query column types: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d type rows, want 1 (is the seeded row in the query window?)", len(got))
			}
			gotTypes := strings.Split(got[0], "|")
			if len(gotTypes) != len(wantTypes) {
				t.Fatalf("got %d types %v, want %d", len(gotTypes), gotTypes, len(wantTypes))
			}
			for i := range wantTypes {
				if gotTypes[i] != wantTypes[i] {
					t.Errorf("%s reports %s, want %s", histogramOutputColumns[i], gotTypes[i], wantTypes[i])
				}
			}
		})
	}
}

// histogramOutputColumns is the projection order chplan.HistogramProjection
// publishes its nine outputs in.
var histogramOutputColumns = []string{
	"HistogramCount",
	"HistogramSum",
	"HistogramScale",
	"HistogramZeroThreshold",
	"HistogramZeroCount",
	"HistogramPositiveOffset",
	"HistogramPositiveBucketCounts",
	"HistogramNegativeOffset",
	"HistogramNegativeBucketCounts",
}

// TestQueryRange_HistogramValued_ChDB covers the MATRIX half of the
// histogram-valued wire shape — the `histograms` key rather than
// `histogram`.
//
// It is not redundant with the instant tests above. internal/api/prom's
// lang.go stamps chclient.ResponseShapeMatrix only when Step > 0, so the
// columnar decode path this change taught the histogram shape to is
// reachable EXCLUSIVELY from /api/v1/query_range. An instant query never
// exercises it.
func TestQueryRange_HistogramValued_ChDB(t *testing.T) {
	srv := newHistValuedServer(t)

	cases := []struct {
		name       string
		query      string
		wantMetric map[string]string
		want       wireHistogram
	}{
		{
			name:       "label_replace rate",
			query:      `label_replace(rate(latency_exp_hist[5m]), "service_copy", "$1-copy", "service", "(.*)")`,
			wantMetric: map[string]string{"service": "api", "service_copy": "api-copy"},
			want: wireHistogram{
				Count: "0.1", Sum: "0.05", Buckets: [][4]any{
					{float64(0), "1", "2", "0.05"},
					{float64(0), "2", "4", "0.05"},
				},
			},
		},
		{name: "rate", query: "rate(latency_exp_hist[5m])", wantMetric: map[string]string{"service": "api"}, want: wireHistogram{
			Count: "0.1", Sum: "0.05", Buckets: [][4]any{
				{float64(0), "1", "2", "0.05"},
				{float64(0), "2", "4", "0.05"},
			},
		}},
		{name: "delta", query: "delta(latency_exp_hist[5m])", wantMetric: map[string]string{"service": "api"}, want: wireHistogram{
			Count: "36", Sum: "18", Buckets: [][4]any{
				{float64(0), "1", "2", "18"},
				{float64(0), "2", "4", "18"},
			},
		}},
		{name: "irate", query: "irate(latency_exp_hist[5m])", wantMetric: map[string]string{"service": "api"}, want: wireHistogram{
			Count: "0.4", Sum: "0.2", Buckets: [][4]any{
				{float64(0), "1", "2", "0.2"},
				{float64(0), "2", "4", "0.2"},
			},
		}},
		{name: "idelta", query: "idelta(latency_exp_hist[5m])", wantMetric: map[string]string{"service": "api"}, want: wireHistogram{
			Count: "24", Sum: "12", Buckets: [][4]any{
				{float64(0), "1", "2", "12"},
				{float64(0), "2", "4", "12"},
			},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A single anchor at the fixture's evaluation time makes the
			// matrix point directly comparable to the instant-vector case.
			stamp := histValuedEvalTime.Format(time.RFC3339)
			reqURL := fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%s&end=%s&step=60",
				srv.URL, url.QueryEscape(tc.query), stamp, stamp)

			resp, err := http.Get(reqURL) //nolint:noctx // test-local request against httptest
			if err != nil {
				t.Fatalf("GET /api/v1/query_range: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("GET /api/v1/query_range: status %d\nbody: %s", resp.StatusCode, body)
			}

			var body struct {
				Data struct {
					ResultType string `json:"resultType"`
					Result     []struct {
						Metric     map[string]string `json:"metric"`
						Values     []any             `json:"values"`
						Histograms []any             `json:"histograms"`
					} `json:"result"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode range response: %v", err)
			}
			if body.Data.ResultType != "matrix" {
				t.Fatalf("resultType = %q, want matrix", body.Data.ResultType)
			}
			if len(body.Data.Result) != 1 {
				t.Fatalf("got %d series, want 1", len(body.Data.Result))
			}
			series := body.Data.Result[0]
			if len(series.Histograms) == 0 {
				t.Fatalf("range query returned float values (%v), not histograms — "+
					"the matrix path collapsed the histogram-valued shape to a scalar", series.Values)
			}
			if len(series.Values) != 0 {
				t.Errorf("range query returned BOTH values and histograms: %v", series.Values)
			}
			assertMetric(t, series.Metric, tc.wantMetric)

			// Each entry is [timestamp, {count, sum, buckets}].
			point, ok := series.Histograms[0].([]any)
			if !ok || len(point) != 2 {
				t.Fatalf("histogram point is %v, want a [ts, body] pair", series.Histograms[0])
			}
			raw, err := json.Marshal(point[1])
			if err != nil {
				t.Fatalf("re-marshal histogram body: %v", err)
			}
			var hist wireHistogram
			if err := json.Unmarshal(raw, &hist); err != nil {
				t.Fatalf("decode histogram body: %v", err)
			}
			if hist.Count != tc.want.Count || hist.Sum != tc.want.Sum {
				t.Errorf("count/sum = %q/%q, want %q/%q", hist.Count, hist.Sum, tc.want.Count, tc.want.Sum)
			}
			assertBuckets(t, hist.Buckets, tc.want.Buckets)
		})
	}
}

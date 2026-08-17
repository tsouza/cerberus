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
    ('latency_exp_hist', map('service', 'api'), toDateTime64('2026-01-01 00:00:01', 9), 30, 15.0, 0, 1, 0, [14, 15], 0, []);

INSERT INTO otel_metrics_exponential_histogram VALUES
    ('range_fn_exp_hist', map('case', 'reset'), toDateTime64('2025-12-31 23:58:01', 9),  10,  5.0, 0, 1, 0, [ 7,  2], 0, []),
    ('range_fn_exp_hist', map('case', 'reset'), toDateTime64('2025-12-31 23:59:01', 9), 100, 50.0, 0, 1, 0, [60, 39], 0, []),
    ('range_fn_exp_hist', map('case', 'reset'), toDateTime64('2026-01-01 00:00:01', 9),  20, 10.0, 0, 1, 0, [ 8, 11], 0, []);

INSERT INTO otel_metrics_exponential_histogram VALUES
    ('scale_shift_exp_hist', map('case', 'downscale'), toDateTime64('2025-12-31 23:59:01', 9),  6, 3.0, 1, 0, 0, [2, 4], 0, []),
    ('scale_shift_exp_hist', map('case', 'downscale'), toDateTime64('2026-01-01 00:00:01', 9), 10, 5.0, 0, 0, 0, [10],   0, []);

INSERT INTO otel_metrics_exponential_histogram VALUES
    ('single_sample_exp_hist', map('case', 'one'), toDateTime64('2026-01-01 00:00:01', 9), 10, 5.0, 0, 0, 0, [4, 6], 0, []);`

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
	return queryHistogramResultAt(t, srv, query, histValuedEvalTime)
}

func queryHistogramResultAt(t *testing.T, srv *httptest.Server, query string, at time.Time) (map[string]string, wireHistogram) {
	t.Helper()

	reqURL := fmt.Sprintf("%s/api/v1/query?query=%s&time=%s",
		srv.URL, url.QueryEscape(query), at.Format(time.RFC3339))
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

func queryRangeHistogramResult(t *testing.T, srv *httptest.Server, query string, start, end time.Time, step time.Duration) (map[string]string, []wireHistogram) {
	t.Helper()
	reqURL := fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%s&end=%s&step=%d",
		srv.URL, url.QueryEscape(query), start.Format(time.RFC3339), end.Format(time.RFC3339), int64(step.Seconds()))
	resp, err := http.Get(reqURL) //nolint:noctx // test-local request against httptest
	if err != nil {
		t.Fatalf("GET /api/v1/query_range %q: %v", query, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/v1/query_range %q: status %d\nbody: %s", query, resp.StatusCode, body)
	}

	var body struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric     map[string]string `json:"metric"`
				Values     []any             `json:"values"`
				Histograms []any             `json:"histograms"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode range response for %q: %v", query, err)
	}
	if body.Status != "success" || body.Data.ResultType != "matrix" || len(body.Data.Result) != 1 {
		t.Fatalf("query_range %q returned status=%q resultType=%q series=%d", query, body.Status, body.Data.ResultType, len(body.Data.Result))
	}
	series := body.Data.Result[0]
	if len(series.Values) != 0 {
		t.Fatalf("query_range %q returned float values %v alongside histogram payload", query, series.Values)
	}
	out := make([]wireHistogram, 0, len(series.Histograms))
	for _, rawPoint := range series.Histograms {
		point, ok := rawPoint.([]any)
		if !ok || len(point) != 2 {
			t.Fatalf("query_range %q histogram point = %v, want [timestamp, body]", query, rawPoint)
		}
		raw, err := json.Marshal(point[1])
		if err != nil {
			t.Fatalf("marshal range histogram body: %v", err)
		}
		var hist wireHistogram
		if err := json.Unmarshal(raw, &hist); err != nil {
			t.Fatalf("decode range histogram body: %v", err)
		}
		out = append(out, hist)
	}
	return series.Metric, out
}

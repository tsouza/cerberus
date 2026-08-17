//go:build chdb

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

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

// TestQuery_HistogramIncompatibleHistogramBinopsDropSamples_ChDB pins the
// HTTP contract behind issue #2277: a binary op between two native-
// histogram selectors that isn't `+`/`-` answers HTTP 200 with an empty
// result vector, matching reference Prometheus's info-annotation drop
// (NewIncompatibleTypesInBinOpInfo), rather than the hard error cerberus
// used to answer via expHistogramSelectorRouting.
func TestQuery_HistogramIncompatibleHistogramBinopsDropSamples_ChDB(t *testing.T) {
	srv := newHistValuedServer(t)

	queries := []string{
		`latency_exp_hist * latency_exp_hist`,
		`latency_exp_hist / latency_exp_hist`,
		`latency_exp_hist > bool latency_exp_hist`,
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

// Every delta / instant-rate kernel needs two native-histogram samples. A
// one-sample window is a successful empty vector, not a zero-valued histogram
// and not an internal error. This executes the per-series sample floor through
// the emitted SQL and wire response instead of merely asserting that the plan
// contains uniqExact(TimeUnix).
func TestQuery_HistogramRangeFunctionsOneSampleDrop_ChDB(t *testing.T) {
	srv := newHistValuedServer(t)

	for _, fn := range []string{"delta", "irate", "idelta"} {
		t.Run(fn, func(t *testing.T) {
			query := fn + "(single_sample_exp_hist[5m])"
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
				t.Fatalf("query %q returned HTTP %d status %q, want successful empty vector",
					query, resp.StatusCode, body.Status)
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
	summedBuckets := [][4]any{
		{float64(3), "0", "0", "2"},
		{float64(0), "1", "2", "16"},
		{float64(0), "2", "4", "18"},
	}
	averagedBuckets := [][4]any{
		{float64(3), "0", "0", "1"},
		{float64(0), "1", "2", "8"},
		{float64(0), "2", "4", "9"},
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
			// Over-time reducers add the two in-window histogram
			// distributions component by component. They reduce along
			// time, so the series labels survive and __name__ is dropped.
			name:       "sum over time",
			query:      "sum_over_time(latency_exp_hist[5m])",
			wantMetric: map[string]string{"service": "api"},
			want:       wireHistogram{Count: "36", Sum: "18", Buckets: summedBuckets},
		},
		{
			name:       "avg over time",
			query:      "avg_over_time(latency_exp_hist[5m])",
			wantMetric: map[string]string{"service": "api"},
			want:       wireHistogram{Count: "18", Sum: "9", Buckets: averagedBuckets},
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

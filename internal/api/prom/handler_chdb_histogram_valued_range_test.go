//go:build chdb

package prom_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"
)

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

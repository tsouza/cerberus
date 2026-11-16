package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// classicHistogramMemoryLimitWant is the exact wire message for a
// ClickHouse memory-limit abort (code 241) of a classic-histogram quantile
// under the 1 GiB per-query cap. Hardcoded, not built from the handler's
// helper, so a drift in the production message fails this pin.
const classicHistogramMemoryLimitWant = "classic-histogram quantile exceeds the ClickHouse memory limit (per-query cap 1073741824 bytes)"

// classicHistogramQuantileQuery is a classic-histogram quantile, on the
// instant path and on the range path alike.
const classicHistogramQuantileQuery = `histogram_quantile(0.5, sum by (le) (rate(req_bucket[5m])))`

// assertMemoryLimitMessage decodes a Prom error envelope and pins HTTP 422,
// errorType "execution" and the exact message.
func assertMemoryLimitMessage(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var body queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity || body.Status != "error" || body.ErrorType != "execution" || body.Error != want {
		t.Fatalf("status=%d %q errorType=%q message=%q; want 422 error execution %q",
			resp.StatusCode, body.Status, body.ErrorType, body.Error, want)
	}
}

// TestQuery_ClassicHistogramMemoryLimit422 pins the instant path: a code-241
// abort of a classic-histogram quantile answers its own 422 message,
// and the same abort of any other query keeps the generic memory-limit one.
func TestQuery_ClassicHistogramMemoryLimit422(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query, want string
	}{
		{classicHistogramQuantileQuery, classicHistogramMemoryLimitWant},
		{`sum by (le) (rate(req_bucket[5m]))`, memoryLimitWant},
		{"up", memoryLimitWant},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{err: chMemLimitError(1 << 30)})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + "/api/v1/query?time=1767225600&query=" + url.QueryEscape(tc.query))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			assertMemoryLimitMessage(t, resp, tc.want)
		})
	}
}

// TestQueryRange_ClassicHistogramMemoryLimit422 pins the query_range drain:
// ClickHouse aborts the classic-histogram quantile's cursor mid-stream with
// code 241 and the handler answers its own 422 message, while the
// same mid-stream abort of any other query keeps the generic one.
func TestQueryRange_ClassicHistogramMemoryLimit422(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		query, want string
	}{
		{classicHistogramQuantileQuery, classicHistogramMemoryLimitWant},
		{"up", memoryLimitWant},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			q := &memLimitQuerier{
				stubQuerier: stubQuerier{samples: []chclient.Sample{
					{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1},
				}},
				limit: 1 << 30,
			}
			srv := newServer(q)
			t.Cleanup(srv.Close)
			resp, err := http.Get(fmt.Sprintf("%s/api/v1/query_range?start=%d&end=%d&step=15&query=%s",
				srv.URL, ts.Add(-time.Hour).Unix(), ts.Unix(), url.QueryEscape(tc.query)))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			assertMemoryLimitMessage(t, resp, tc.want)
		})
	}
}

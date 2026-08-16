//go:build chdb

package prom_test

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestQuery_NestedHistogramConsumers_ChDB(t *testing.T) {
	srv := newHistValuedServer(t)
	for _, tc := range []struct {
		name       string
		query      string
		wantMetric map[string]string
	}{
		{name: "sum over rate", query: "sum(rate(latency_exp_hist[5m]))", wantMetric: map[string]string{}},
		{name: "avg by over rate", query: "avg by (service) (rate(latency_exp_hist[5m]))", wantMetric: map[string]string{"service": "api"}},
		{name: "label_join rate", query: `label_join(rate(latency_exp_hist[5m]), "service_copy", "-", "service")`, wantMetric: map[string]string{"service": "api", "service_copy": "api"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metric, got := queryHistogramResult(t, srv, tc.query)
			assertMetric(t, metric, tc.wantMetric)
			if got.Count != "0.1" || got.Sum != "0.05" {
				t.Fatalf("query %q histogram count/sum = %q/%q, want 0.1/0.05", tc.query, got.Count, got.Sum)
			}
			assertBuckets(t, got.Buckets, [][4]any{
				{float64(0), "1", "2", "0.05"},
				{float64(0), "2", "4", "0.05"},
			})
		})
	}
}

func TestQuery_NestedHistogramPresenceAggregations_ChDB(t *testing.T) {
	seed := histValuedSeed + `
INSERT INTO otel_metrics_exponential_histogram VALUES
    ('latency_exp_hist', map('service', 'web'), toDateTime64('2025-12-31 23:59:01', 9),  4,  2.0, 0, 0, 0, [1, 3], 0, []),
    ('latency_exp_hist', map('service', 'web'), toDateTime64('2026-01-01 00:00:01', 9), 20, 10.0, 0, 0, 0, [9, 11], 0, []);`
	srv, _ := newChDBServer(t, seed)

	for _, tc := range []struct {
		query      string
		wantValues map[string]string
	}{
		{query: `count(rate(latency_exp_hist[5m]))`, wantValues: map[string]string{"": "2"}},
		{query: `count by (service) (rate(latency_exp_hist[5m]))`, wantValues: map[string]string{"api": "1", "web": "1"}},
		{query: `group(rate(latency_exp_hist[5m]))`, wantValues: map[string]string{"": "1"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got := decodeVectorQuery(t, srv.URL, tc.query, histValuedEvalTime.Unix())
			if len(got) != len(tc.wantValues) {
				t.Fatalf("query %q returned %d samples, want %d: %#v", tc.query, len(got), len(tc.wantValues), got)
			}
			for _, sample := range got {
				service := sample.Metric["service"]
				want, ok := tc.wantValues[service]
				if !ok || sample.Value[1] != want {
					t.Errorf("query %q labels=%v value=%v, want service=%q value=%q", tc.query, sample.Metric, sample.Value[1], service, want)
				}
			}
		})
	}
}

func TestQuery_NativeHistogramCountValues_ChDB(t *testing.T) {
	srv := newHistValuedServer(t)
	for _, tc := range []struct {
		query, wantLabel string
	}{
		{query: `count_values("hist", latency_exp_hist)`, wantLabel: `{count:30, sum:15, [-0,0]:1, (0.5,1]:14, (1,2]:15}`},
		{query: `count_values("hist", rate(latency_exp_hist[5m]))`, wantLabel: `{count:0.1, sum:0.05, (0.5,1]:0.05, (1,2]:0.05}`},
	} {
		t.Run(tc.query, func(t *testing.T) {
			samples := decodeVectorQuery(t, srv.URL, tc.query, histValuedEvalTime.Unix())
			if len(samples) != 1 {
				t.Fatalf("query %q returned %d samples, want 1: %#v", tc.query, len(samples), samples)
			}
			if got := samples[0].Metric["hist"]; got != tc.wantLabel {
				t.Errorf("query %q histogram label = %q, want %q", tc.query, got, tc.wantLabel)
			}
			if got := samples[0].Value[1]; got != "1" {
				t.Errorf("query %q count = %v, want 1", tc.query, got)
			}
		})
	}
}

func TestQuery_HistogramValuedSubqueryConsumers_ChDB(t *testing.T) {
	seed := histValuedDDL + `
INSERT INTO otel_metrics_exponential_histogram VALUES
    ('subquery_exp_hist', map('service', 'api'), toDateTime64('2025-12-31 23
58:00', 9), 10,  5.0, 0, 0, 0, [10], 0, []),
    ('subquery_exp_hist', map('service', 'api'), toDateTime64('2025-12-31 23
59:00', 9), 20, 10.0, 0, 0, 0, [20], 0, []),
    ('subquery_exp_hist', map('service', 'api'), toDateTime64('2026-01-01 00
00:00', 9), 30, 15.0, 0, 0, 0, [30], 0, []),
    ('subquery_exp_hist', map('service', 'api'), toDateTime64('2026-01-01 00
01:00', 9), 40, 20.0, 0, 0, 0, [40], 0, []);`
	srv, _ := newChDBServer(t, seed)
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	metric, instant := queryHistogramResultAt(t, srv, `rate(subquery_exp_hist[2m:1m])`, anchor)
	assertMetric(t, metric, map[string]string{"service": "api"})
	assertSubqueryRateHistogram(t, instant)
	_, offset := queryHistogramResultAt(t, srv, `rate(subquery_exp_hist[2m:1m] offset 1m)`, anchor.Add(time.Minute))
	assertSubqueryRateHistogram(t, offset)
	if fmt.Sprint(offset) != fmt.Sprint(instant) {
		t.Errorf("offset subquery histogram = %#v, want %#v", offset, instant)
	}

	reqURL := fmt.Sprintf("%s/api/v1/query?query=%s&time=%s", srv.URL,
		url.QueryEscape(`rate(subquery_exp_hist[1m:1m])`), anchor.Format(time.RFC3339))
	resp, err := http.Get(reqURL) //nolint:noctx // test-local request against httptest
	if err != nil {
		t.Fatalf("GET one-sample subquery: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var emptyBody struct {
		Status string `json:"status"`
		Data   struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&emptyBody); err != nil {
		t.Fatalf("decode one-sample response: %v", err)
	}
	if resp.StatusCode != http.StatusOK || emptyBody.Status != "success" || len(emptyBody.Data.Result) != 0 {
		t.Fatalf("one-sample subquery returned HTTP %d status=%q samples=%d, want successful empty vector",
			resp.StatusCode, emptyBody.Status, len(emptyBody.Data.Result))
	}

	_, matrix := queryRangeHistogramResult(t, srv, `rate(subquery_exp_hist[2m:1m])`, anchor, anchor.Add(time.Minute), time.Minute)
	if len(matrix) != 2 {
		t.Fatalf("un-pinned subquery range returned %d points, want 2", len(matrix))
	}
	for _, hist := range matrix {
		assertSubqueryRateHistogram(t, hist)
	}
	_, pinned := queryRangeHistogramResult(t, srv, `rate(subquery_exp_hist[2m:1m] @ 1767225600)`, anchor, anchor.Add(time.Minute), time.Minute)
	if len(pinned) != 2 {
		t.Fatalf("pinned subquery range returned %d points, want 2", len(pinned))
	}
	for _, hist := range pinned {
		assertSubqueryRateHistogram(t, hist)
	}
}

func assertSubqueryRateHistogram(t *testing.T, hist wireHistogram) {
	t.Helper()
	const epsilon = 1e-12
	for label, got := range map[string]struct {
		got  string
		want float64
	}{
		"count":        {got: hist.Count, want: 1.0 / 6.0},
		"sum":          {got: hist.Sum, want: 1.0 / 12.0},
		"bucket count": {got: histogramBucketCount(t, hist), want: 1.0 / 6.0},
	} {
		value, err := strconv.ParseFloat(got.got, 64)
		if err != nil {
			t.Fatalf("parse %s %q: %v", label, got.got, err)
		}
		if math.Abs(value-got.want) > epsilon {
			t.Errorf("%s = %.17g, want %.17g", label, value, got.want)
		}
	}
}

func histogramBucketCount(t *testing.T, hist wireHistogram) string {
	t.Helper()
	if len(hist.Buckets) != 1 {
		t.Fatalf("histogram buckets = %#v, want exactly one positive bucket", hist.Buckets)
	}
	count, ok := hist.Buckets[0][3].(string)
	if !ok {
		t.Fatalf("histogram bucket count = %T(%v), want string", hist.Buckets[0][3], hist.Buckets[0][3])
	}
	return count
}

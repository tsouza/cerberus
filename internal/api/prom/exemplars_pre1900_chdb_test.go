//go:build chdb

// chDB coverage for /api/v1/query_exemplars windows whose bounds fall
// before 1900. ClickHouse parses a time literal earlier than 1900-01-01 as
// 1900-01-01, so a bound in that range must compare exactly some other
// way. The seed carries one exemplar-bearing sample in 1800 and one in
// 2000, and every case sits on a nanosecond edge of the 1800 sample.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
)

// exemplarPre1900Seed seeds `http_requests_total` with job="y1800" at
// sample, and job="y2000" at modern, each carrying one exemplar at the
// sample's own instant. Both instants are inserted from their Unix
// nanoseconds: a toDateTime64 literal would itself clamp 1800 to 1900.
func exemplarPre1900Seed(sample, modern time.Time) string {
	row := func(job string, ns int64) string {
		return fmt.Sprintf(`('http_requests_total', map('job', '%s'), 'checkout',
        fromUnixTimestamp64Nano(toInt64(%d)), 1.0,
        [map('request_id', 'r-%s')], [fromUnixTimestamp64Nano(toInt64(%d))], [0.5],
        ['span-%s'], ['trace-%s'])`, job, ns, job, ns, job, job)
	}
	return exemplarsDDL() + `
INSERT INTO otel_metrics_sum (
    MetricName, Attributes, ServiceName, TimeUnix, Value,
    Exemplars.FilteredAttributes, Exemplars.TimeUnix, Exemplars.Value,
    Exemplars.SpanId, Exemplars.TraceId
) VALUES
    ` + row("y1800", sample.UnixNano()) + `,
    ` + row("y2000", modern.UnixNano()) + `;`
}

// TestQueryExemplars_ChDB_Pre1900Window asserts the exemplar window is
// exact at nanosecond edges before 1900, and that a start before the
// earliest storable instant excludes nothing.
func TestQueryExemplars_ChDB_Pre1900Window(t *testing.T) {
	sample := time.Date(1800, time.January, 1, 0, 0, 0, 0, time.UTC)
	modern := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	srv, _ := newChDBExemplarsServer(t, exemplarPre1900Seed(sample, modern))
	rfc := func(t time.Time) string { return t.Format(time.RFC3339Nano) }
	ns := time.Nanosecond
	cases := []struct {
		name, start, end string
		want             []string
	}{
		{"start_at_1800_sample", rfc(sample), rfc(modern), []string{"y1800", "y2000"}},
		{"start_1ns_after_1800_sample", rfc(sample.Add(ns)), rfc(modern), []string{"y2000"}},
		{"end_at_1800_sample", "1700-01-01T00:00:00Z", rfc(sample), []string{"y1800"}},
		{"end_1ns_before_1800_sample", "1700-01-01T00:00:00Z", rfc(sample.Add(-ns)), nil},
		{"start_before_storage", "1600-01-01T00:00:00Z", rfc(modern), []string{"y1800", "y2000"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := url.Values{"query": {"http_requests_total"}, "start": {c.start}, "end": {c.end}}
			resp, err := http.Get(srv.URL + "/api/v1/query_exemplars?" + v.Encode())
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			var out struct {
				Status string                `json:"status"`
				Data   []prom.ExemplarSeries `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got := []string{}
			for _, s := range out.Data {
				got = append(got, s.SeriesLabels["job"])
			}
			sort.Strings(got)
			if !setEqual(got, c.want) {
				t.Errorf("exemplar series jobs = %v, want %v", got, c.want)
			}
		})
	}
}

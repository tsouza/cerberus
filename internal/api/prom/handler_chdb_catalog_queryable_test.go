//go:build chdb

// Catalog ↔ query-surface invariant, end-to-end against chDB:
// EVERY name returned by `/api/v1/label/__name__/values` must be
// queryable — an instant `{__name__="<name>"}` returns >= 1 series.
//
// This is the ratchet for the round-3 Drilldown-Metrics sweep finding
// (live stack: 83 advertised names, 10 queryable, 73 empty preview
// panels). The two manifestations it pins:
//
//  1. Dotted-name asymmetry — OTel kubeletstats / k8scluster /
//     semconv emitters store DOTTED MetricNames
//     (`k8s.node.cpu.usage`); the catalog normalises them to the Prom
//     grammar (`k8s_node_cpu_usage`); the selector lowering must
//     resolve the underscored alias back to the dotted rows (the
//     MetricName candidate fan-out in internal/promql's
//     metricNamePredicate).
//
//  2. Bare classic-histogram names — the histogram table stores one
//     row per sample under the BARE base name, but the query surface
//     serves only the `_bucket` / `_count` / `_sum` companions; the
//     catalog must advertise exactly those and drop the bare name
//     (reference Prometheus never lists a classic histogram's bare
//     family name in `__name__` values).
//
// The seeded corpus covers all five OTel-CH metric tables. The
// exponential-histogram + summary tables are seeded but deliberately
// expected ABSENT from the catalog: the bare-selector query surface
// reads neither table (exp-histograms are reachable only through
// `histogram_quantile` + the ExpHistogramSuffix routing; the summary
// table has no lowering), so advertising their names would recreate
// the empty-preview-panel bug. If a future change adds either table to
// the catalog, the exact-set assertion fails and forces the
// queryability side to land in the same PR. No tolerance lists.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"
)

const catalogGaugeDDL = `CREATE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

const catalogSumDDL = `CREATE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

const catalogHistogramDDL = `CREATE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

const catalogExpHistogramDDL = `CREATE TABLE otel_metrics_exponential_histogram (
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
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

const catalogSummaryDDL = `CREATE TABLE otel_metrics_summary (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

func TestCatalog_AdvertisedNamesAreQueryable_ChDB(t *testing.T) {
	// Step 1 enumerates __name__ windowless; seed within the default-lookback
	// retention horizon (boundMetadataWindow) so the bounded discovery scan
	// still advertises every name. Step 2's instant queries anchor at
	// seedTime, whose 5m staleness window covers these rows.
	seedTime := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	ts := seedTime.Format("2006-01-02 15:04:05.000")

	seed := catalogGaugeDDL + catalogSumDDL + catalogHistogramDDL +
		catalogExpHistogramDDL + catalogSummaryDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge VALUES
    ('k8s.node.cpu.usage', map('k8s.node.name', 'n1'), toDateTime64('%[1]s', 9), 0.42),
    ('k8s.node.memory.major_page_faults', map('k8s.node.name', 'n1'), toDateTime64('%[1]s', 9), 3.0),
    ('up', map('job', 'api'), toDateTime64('%[1]s', 9), 1.0);
INSERT INTO otel_metrics_sum VALUES
    ('k8s.pod.network.io', map('k8s.pod.name', 'p1'), toDateTime64('%[1]s', 9), 1024.0),
    ('cerberus_queries_total', map('cerberus.ql', 'promql'), toDateTime64('%[1]s', 9), 42.0),
    ('http_server_request_duration_count', map('job', 'api'), toDateTime64('%[1]s', 9), 7.0);
INSERT INTO otel_metrics_histogram VALUES
    ('cerberus_queries_duration_seconds', map('cerberus.ql', 'promql'), toDateTime64('%[1]s', 9), 3, 1.5, [1, 1, 1], [0.1, 1]),
    ('http.server.request.duration', map('http.request.method', 'GET'), toDateTime64('%[1]s', 9), 2, 0.5, [2, 0], [0.25]);
INSERT INTO otel_metrics_exponential_histogram VALUES
    ('showcase_latency_exp_hist', map('job', 'api'), toDateTime64('%[1]s', 9), 4, 2.0, 2, 1, 0, [1, 2], 0, []);
INSERT INTO otel_metrics_summary VALUES
    ('rpc.duration', map('job', 'api'), toDateTime64('%[1]s', 9), 5, 2.5);`,
		ts)

	srv, _ := newChDBServer(t, seed)

	// --- 1. The catalog advertises exactly the queryable name set. ---
	resp, err := http.Get(srv.URL + "/api/v1/label/__name__/values")
	if err != nil {
		t.Fatalf("GET label values: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("label values status=%d body=%s", resp.StatusCode, body)
	}
	var env struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode label values: %v body=%s", err, body)
	}
	if env.Status != "success" {
		t.Fatalf("label values status=%q body=%s", env.Status, body)
	}

	want := []string{
		"cerberus_queries_duration_seconds_bucket",
		"cerberus_queries_duration_seconds_count",
		"cerberus_queries_duration_seconds_sum",
		"cerberus_queries_total",
		"http_server_request_duration_bucket",
		"http_server_request_duration_count", // sum-table row + histogram expansion dedupe to one entry
		"http_server_request_duration_sum",
		"k8s_node_cpu_usage",
		"k8s_node_memory_major_page_faults",
		"k8s_pod_network_io",
		"up",
	}
	if !reflect.DeepEqual(env.Data, want) {
		t.Fatalf("advertised __name__ values mismatch:\n got: %v\nwant: %v", env.Data, want)
	}

	// --- 2. Every advertised name returns >= 1 series on /query. ---
	for _, name := range env.Data {
		name := name
		t.Run(name, func(t *testing.T) {
			q := fmt.Sprintf(`{__name__=%q}`, name)
			u := fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
				srv.URL, url.QueryEscape(q), seedTime.Unix())
			resp, err := http.Get(u)
			if err != nil {
				t.Fatalf("GET query: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("query status=%d body=%s", resp.StatusCode, body)
			}
			var parsed queryResponse
			if err := json.Unmarshal([]byte(body), &parsed); err != nil {
				t.Fatalf("decode query: %v body=%s", err, body)
			}
			if parsed.Status != "success" {
				t.Fatalf("query status=%q err=%s", parsed.Status, parsed.Error)
			}
			if parsed.Data.ResultType != "vector" {
				t.Fatalf("resultType=%q, want vector", parsed.Data.ResultType)
			}
			rawResult, _ := json.Marshal(parsed.Data.Result)
			var vec []json.RawMessage
			if err := json.Unmarshal(rawResult, &vec); err != nil {
				t.Fatalf("decode vector: %v body=%s", err, body)
			}
			if len(vec) == 0 {
				t.Fatalf("advertised name %q is not queryable: instant query %s returned 0 series — "+
					"the catalog must never advertise a name the query surface cannot serve "+
					"(empty Drilldown-Metrics preview panel)", name, q)
			}
		})
	}
}

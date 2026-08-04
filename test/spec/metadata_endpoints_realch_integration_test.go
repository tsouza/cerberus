//go:build integration

// metadata_endpoints_realch_integration_test.go — real-ClickHouse
// differential for the Loki + Prometheus metadata/label-values endpoints
// (issue #1634, split out of #1510).
//
// # WHY THIS TEST EXISTS
//
// `/loki/api/v1/series`, `/loki/api/v1/labels`, `/loki/api/v1/label/<name>/values`,
// `/loki/api/v1/index/stats`, `/loki/api/v1/index/volume`, and PromQL's
// `/api/v1/labels`, `/api/v1/label/<name>/values`, `/api/v1/metadata` all
// call a `chclient.Client` decoder method directly from the handler
// (`QueryLabelSets`, `QueryIndexStats`, `QueryStrings`, `QueryIndexVolume`,
// `QueryMetricMeta`) — the SQL these handlers build is exactly the SQL that
// reaches ClickHouse in production, with no `engine.QueryPlan` /
// `ProjectSamples` stage in between. That makes them the tractable half of
// #1510 (see the sibling issue #1635 for the Tempo-search half, which DOES
// have a hidden pipeline stage to reproduce) — but until this file existed,
// `test/spec/{promql,logql,traceql}` carried no fixture corpus for them at
// all: every TXTAR fixture is query-body-shaped (a PromQL/LogQL/TraceQL
// string that lowers into a chplan), and these endpoints are matcher +
// time-range driven instead, so they don't fit that format.
//
// This is exactly the blind spot strict-scan.yml exists to catch (chDB
// coerces result-column types a real clickhouse-go/v2 Scan strict-scans and
// rejects — see project memory "chDB vs prod-CH scan strictness" and
// strictscan_integration_test.go's doc comment above) — left open on six
// decoder methods that had never been exercised against a real server at
// all.
//
// # WHAT THIS TEST DOES
//
// Spins ONE real `clickhouse/clickhouse-server` via testcontainers (the
// same pattern strict-scan / router-corpus / solver-memory-apportion /
// route-B / chclient's real-CH lanes already use), creates the OTel-CH
// default logs table plus the three metric tables the metadata handlers
// fan out across (gauge / sum / classic-histogram), seeds one small
// deterministic, past-anchored fixture, then drives each endpoint through a
// REAL `loki.Handler` / `prom.Handler` mounted on an `httptest.Server` —
// the actual production HTTP routing + decoder path, not a direct call
// into an unexported builder function. Each subtest asserts the decoded response
// shape AND content, so a real-CH type-coercion failure (a strict-scan
// error) fails loudly instead of silently returning an empty/wrong body.
//
// Gated behind the `integration` build tag (Docker required); joins the
// established real-CH lane family (test/spec/strictscan_integration_test.go,
// internal/routerrules/realch_integration_test.go,
// internal/solver/executor_realch_integration_test.go,
// internal/api/prom/handler_route_b_realch_integration_test.go,
// internal/chclient/conn_teardown_integration_test.go) wired as an extra
// step in .github/workflows/strict-scan.yml — run locally with:
//
//	go test -tags=integration -count=1 -run TestMetadataEndpoints_RealClickHouse ./test/spec/...
package spec_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// metadataEndpointsCHImage matches the other real-CH integration lanes
// (strict-scan, router-rules, solver-memory-apportion, route-B, chclient)
// so every real-CH lane exercises the same server version.
const metadataEndpointsCHImage = "clickhouse/clickhouse-server:25.8-alpine"

// metadataEndpointsAnchor is the past-anchored base timestamp every seeded
// row and every request window derives from. It is computed relative to
// wall-clock time at test start (mirroring
// handler_route_b_realch_integration_test.go's windowStart), not a fixed
// calendar constant: Prometheus's `/api/v1/metadata` deliberately accepts
// no start/end parameters at all (see internal/api/prom/metadata.go's
// handleMetadata) and always bounds its scan to
// [now-defaultMetadataLookback, now] — a hardcoded past calendar date would
// eventually scroll outside that 14-day lookback and the prom/metadata
// subtest below would start failing on data that is otherwise perfectly
// valid. Anchoring an hour behind the actual test-run "now" keeps every
// subtest's seeded row inside both the explicit [start,end] windows the
// other endpoints are given AND the metadata endpoint's own implicit
// lookback window, regardless of when the suite runs.
var metadataEndpointsAnchor = time.Now().UTC().Truncate(time.Minute).Add(-time.Hour)

// metadataEndpointsJob is the stream/series label value seeded on every
// row, queried back by every /labels, /label/<name>/values, /series,
// /index/stats and /index/volume case below.
const metadataEndpointsJob = "api"

// metadataLogsDDL is the minimal otel_logs projection the Loki metadata
// endpoints read: ResourceAttributes (the matcher target + stream-identity
// label set), Body (the byte-volume source for /index/stats + /index/volume)
// and Timestamp (the window predicate column). Mirrors
// internal/api/loki/map_key_order_chdb_test.go's keyOrderSeedTable.
const metadataLogsDDL = `CREATE TABLE otel_logs (
    Timestamp DateTime64(9),
    Body String,
    ResourceAttributes Map(String, String)
) ENGINE = MergeTree() ORDER BY Timestamp;`

// metadataGaugeDDL / metadataSumDDL / metadataHistogramDDL are the three
// metric tables internal/api/prom.Handler.metricTables() fans the
// /labels, /label/<name>/values and /metadata endpoints across. Columns
// are trimmed to what those handlers actually project (see
// internal/api/prom/metadata.go), following the same "minimal projection,
// not full production DDL" convention as
// internal/api/prom/handler_route_b_realch_integration_test.go's
// routeBSumDDL.
const metadataGaugeDDL = `CREATE TABLE otel_metrics_gauge (
    ResourceAttributes Map(String, String),
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

const metadataSumDDL = `CREATE TABLE otel_metrics_sum (
    ResourceAttributes Map(String, String),
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64,
    IsMonotonic Boolean
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

const metadataHistogramDDL = `CREATE TABLE otel_metrics_histogram (
    ResourceAttributes Map(String, String),
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

// metadataEndpointsTSFmt is the literal format toDateTime64(<lit>, 9)
// expects, matching handler_route_b_realch_integration_test.go's seed
// helper.
const metadataEndpointsTSFmt = "2006-01-02 15:04:05.000"

// envelope decodes the {status, data, error} shape every Loki + Prom
// metadata endpoint (except /loki/api/v1/index/stats, which writes its
// IndexStats struct bare) wraps its body in.
type envelope struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
	Error  string          `json:"error"`
}

// getEnvelope issues a GET against base+path and decodes the {status,
// data} envelope, failing the test on a non-200 status, a transport
// error, or a non-"success" status (surfacing the decoded error string —
// this is the assertion that would catch a strict-scan 502).
func getEnvelope(t *testing.T, base, path string, query url.Values) envelope {
	t.Helper()
	full := base + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	resp, err := http.Get(full)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", path, resp.StatusCode, body)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("GET %s: unmarshal envelope: %v\nbody=%s", path, err, body)
	}
	if env.Status != "success" {
		t.Fatalf("GET %s: status=%q error=%q body=%s", path, env.Status, env.Error, body)
	}
	return env
}

// containsStr reports whether s appears in vals.
func containsStr(vals []string, s string) bool {
	for _, v := range vals {
		if v == s {
			return true
		}
	}
	return false
}

func TestMetadataEndpoints_RealClickHouse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")

	container, err := tcclickhouse.Run(
		ctx,
		metadataEndpointsCHImage,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	client, err := chclient.New(chclient.Config{
		Addr:     host + ":" + port.Port(),
		Database: "otel",
		Username: "cerberus",
		Password: "cerberus",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	for _, ddl := range []string{metadataLogsDDL, metadataGaugeDDL, metadataSumDDL, metadataHistogramDDL} {
		if err := client.Exec(ctx, ddl); err != nil {
			t.Fatalf("create table: %v\nddl=%s", err, ddl)
		}
	}

	// --- seed otel_logs: three rows in one job="api" stream ---
	var logsSB strings.Builder
	logsSB.WriteString(`INSERT INTO otel_logs (Timestamp, Body, ResourceAttributes) VALUES `)
	for i, body := range []string{"line one", "line two", "line three"} {
		ts := metadataEndpointsAnchor.Add(time.Duration(10*(i+1)) * time.Minute).Format(metadataEndpointsTSFmt)
		if i > 0 {
			logsSB.WriteString(",\n")
		}
		fmt.Fprintf(&logsSB, `(toDateTime64('%s', 9), '%s', map('job', '%s'))`,
			ts, body, metadataEndpointsJob)
	}
	if err := client.Exec(ctx, logsSB.String()); err != nil {
		t.Fatalf("seed otel_logs: %v", err)
	}

	// --- seed metric tables: one gauge, two sum (mono + non-mono), one histogram ---
	sampleTS := metadataEndpointsAnchor.Add(15 * time.Minute).Format(metadataEndpointsTSFmt)

	gaugeInsert := fmt.Sprintf(
		`INSERT INTO otel_metrics_gauge (ResourceAttributes, MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value)
		 VALUES (map(), 'test_gauge_metric', 'a test gauge', '1', map('job', '%s'), toDateTime64('%s', 9), 42.0)`,
		metadataEndpointsJob, sampleTS,
	)
	if err := client.Exec(ctx, gaugeInsert); err != nil {
		t.Fatalf("seed otel_metrics_gauge: %v", err)
	}

	sumInsert := fmt.Sprintf(
		`INSERT INTO otel_metrics_sum (ResourceAttributes, MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value, IsMonotonic)
		 VALUES
		 (map(), 'test_sum_counter', 'a test counter', '1', map('job', '%s'), toDateTime64('%s', 9), 7.0, true),
		 (map(), 'test_sum_updown', 'a test updown counter', '1', map('job', '%s'), toDateTime64('%s', 9), 3.0, false)`,
		metadataEndpointsJob, sampleTS, metadataEndpointsJob, sampleTS,
	)
	if err := client.Exec(ctx, sumInsert); err != nil {
		t.Fatalf("seed otel_metrics_sum: %v", err)
	}

	histogramInsert := fmt.Sprintf(
		`INSERT INTO otel_metrics_histogram
		 (ResourceAttributes, MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds)
		 VALUES (map(), 'test_histogram_metric', 'a test histogram', 'ms', map('job', '%s'), toDateTime64('%s', 9), 10, 100.5, [1,2,3], [1.0,2.0])`,
		metadataEndpointsJob, sampleTS,
	)
	if err := client.Exec(ctx, histogramInsert); err != nil {
		t.Fatalf("seed otel_metrics_histogram: %v", err)
	}

	// --- wire the two REAL handlers on one httptest.Server, same
	//     production Mount() path both wire.go / cmd/cerberus use. ---
	lokiHandler := loki.New(client, schema.DefaultOTelLogs(), nil)
	promHandler := prom.New(client, schema.DefaultOTelMetrics(), nil)

	mux := http.NewServeMux()
	lokiHandler.Mount(mux)
	promHandler.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	windowStart := metadataEndpointsAnchor
	windowEnd := metadataEndpointsAnchor.Add(time.Hour)
	startEndQuery := func(extra url.Values) url.Values {
		q := url.Values{
			"start": {fmt.Sprintf("%d", windowStart.Unix())},
			"end":   {fmt.Sprintf("%d", windowEnd.Unix())},
		}
		for k, v := range extra {
			q[k] = v
		}
		return q
	}

	// === Loki /series: chclient.QueryLabelSets ===
	t.Run("loki/series", func(t *testing.T) {
		env := getEnvelope(t, srv.URL, "/loki/api/v1/series", startEndQuery(nil))
		var streams []map[string]string
		if err := json.Unmarshal(env.Data, &streams); err != nil {
			t.Fatalf("unmarshal data: %v\ndata=%s", err, env.Data)
		}
		if len(streams) != 1 {
			t.Fatalf("got %d streams, want 1: %v", len(streams), streams)
		}
		if got := streams[0]["job"]; got != metadataEndpointsJob {
			t.Fatalf("stream label job=%q, want %q: %v", got, metadataEndpointsJob, streams[0])
		}
	})

	// === Loki /labels: chclient.QueryStrings ===
	t.Run("loki/labels", func(t *testing.T) {
		env := getEnvelope(t, srv.URL, "/loki/api/v1/labels", startEndQuery(nil))
		var names []string
		if err := json.Unmarshal(env.Data, &names); err != nil {
			t.Fatalf("unmarshal data: %v\ndata=%s", err, env.Data)
		}
		if !containsStr(names, "job") {
			t.Fatalf("labels = %v, want it to contain \"job\"", names)
		}
	})

	// === Loki /label/job/values: chclient.QueryStrings ===
	t.Run("loki/label_values", func(t *testing.T) {
		env := getEnvelope(t, srv.URL, "/loki/api/v1/label/job/values", startEndQuery(nil))
		var values []string
		if err := json.Unmarshal(env.Data, &values); err != nil {
			t.Fatalf("unmarshal data: %v\ndata=%s", err, env.Data)
		}
		if !containsStr(values, metadataEndpointsJob) {
			t.Fatalf("label values = %v, want it to contain %q", values, metadataEndpointsJob)
		}
	})

	// === Loki /index/stats: chclient.QueryIndexStats ===
	t.Run("loki/index_stats", func(t *testing.T) {
		q := startEndQuery(url.Values{"query": {`{job="api"}`}})
		resp, err := http.Get(srv.URL + "/loki/api/v1/index/stats?" + q.Encode())
		if err != nil {
			t.Fatalf("GET index/stats: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("index/stats status=%d body=%s", resp.StatusCode, body)
		}
		var stats loki.IndexStats
		if err := json.Unmarshal(body, &stats); err != nil {
			t.Fatalf("unmarshal IndexStats: %v\nbody=%s", err, body)
		}
		if stats.Streams != 1 {
			t.Fatalf("Streams=%d, want 1: %+v", stats.Streams, stats)
		}
		if stats.Entries != 3 {
			t.Fatalf("Entries=%d, want 3: %+v", stats.Entries, stats)
		}
		if stats.Bytes == 0 {
			t.Fatalf("Bytes=0, want > 0: %+v", stats)
		}
	})

	// === Loki /index/volume: chclient.QueryIndexVolume ===
	t.Run("loki/index_volume", func(t *testing.T) {
		env := getEnvelope(t, srv.URL, "/loki/api/v1/index/volume", startEndQuery(url.Values{"query": {`{job="api"}`}}))
		var qd struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		}
		if err := json.Unmarshal(env.Data, &qd); err != nil {
			t.Fatalf("unmarshal data: %v\ndata=%s", err, env.Data)
		}
		if qd.ResultType != "vector" {
			t.Fatalf("resultType=%q, want vector", qd.ResultType)
		}
		if len(qd.Result) != 1 {
			t.Fatalf("got %d volume rows, want 1: %v", len(qd.Result), qd.Result)
		}
		if got := qd.Result[0].Metric["job"]; got != metadataEndpointsJob {
			t.Fatalf("volume row job=%q, want %q: %v", got, metadataEndpointsJob, qd.Result[0])
		}
		bytesStr, _ := qd.Result[0].Value[1].(string)
		if bytesStr == "" || bytesStr == "0" {
			t.Fatalf("volume bytes=%q, want a positive byte count", bytesStr)
		}
	})

	// === Prometheus /api/v1/labels: chclient.QueryStrings (fan-out over
	//     gauge/sum/histogram) ===
	t.Run("prom/labels", func(t *testing.T) {
		env := getEnvelope(t, srv.URL, "/api/v1/labels", startEndQuery(nil))
		var names []string
		if err := json.Unmarshal(env.Data, &names); err != nil {
			t.Fatalf("unmarshal data: %v\ndata=%s", err, env.Data)
		}
		sort.Strings(names)
		if !containsStr(names, "__name__") {
			t.Fatalf("labels = %v, want it to contain \"__name__\"", names)
		}
		if !containsStr(names, "job") {
			t.Fatalf("labels = %v, want it to contain \"job\"", names)
		}
	})

	// === Prometheus /api/v1/label/job/values: chclient.QueryStrings ===
	t.Run("prom/label_values", func(t *testing.T) {
		env := getEnvelope(t, srv.URL, "/api/v1/label/job/values", startEndQuery(nil))
		var values []string
		if err := json.Unmarshal(env.Data, &values); err != nil {
			t.Fatalf("unmarshal data: %v\ndata=%s", err, env.Data)
		}
		if !containsStr(values, metadataEndpointsJob) {
			t.Fatalf("label values = %v, want it to contain %q", values, metadataEndpointsJob)
		}
	})

	// === Prometheus /api/v1/metadata: chclient.QueryMetricMeta, one arm
	//     per (table, reported-type) — proves the gauge / monotonic-sum /
	//     non-monotonic-sum / histogram arms all strict-scan cleanly and
	//     the monotonic split reports counter vs gauge correctly. ===
	t.Run("prom/metadata", func(t *testing.T) {
		env := getEnvelope(t, srv.URL, "/api/v1/metadata", nil)
		var grouped map[string][]struct {
			Type string `json:"type"`
			Help string `json:"help"`
			Unit string `json:"unit"`
		}
		if err := json.Unmarshal(env.Data, &grouped); err != nil {
			t.Fatalf("unmarshal data: %v\ndata=%s", err, env.Data)
		}
		wantType := map[string]string{
			"test_gauge_metric":     "gauge",
			"test_sum_counter":      "counter",
			"test_sum_updown":       "gauge",
			"test_histogram_metric": "histogram",
		}
		for metric, wantKind := range wantType {
			entries, ok := grouped[metric]
			if !ok || len(entries) == 0 {
				t.Fatalf("metadata missing metric %q; got keys %v", metric, mapKeys(grouped))
			}
			if got := entries[0].Type; got != wantKind {
				t.Fatalf("metric %q type=%q, want %q", metric, got, wantKind)
			}
		}
	})
}

func mapKeys[V any](m map[string][]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

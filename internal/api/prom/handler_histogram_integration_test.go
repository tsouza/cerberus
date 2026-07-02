//go:build integration

// Histogram end-to-end against the REAL OTel-CH exporter schema — the layer no
// other test exercises.
//
// Every other layer validates cerberus against a fixture cerberus itself
// created: the seed DDL and the reader both use the same table-name constant
// and the same plain `Map(String, String)` column type, so they always agree.
// This test provisions the schema the way the upstream clickhouseexporter
// actually writes it — label maps are `Map(LowCardinality(String), String)`
// and the exponential-histogram table is `otel_metrics_exponential_histogram`
// — and drives the Prometheus HTTP surface with `columnar_result_decode` ON.
//
// It pins three behaviours at once:
//
//   1. A native exp-histogram quantile resolves against
//      `otel_metrics_exponential_histogram` (the table the exporter creates).
//   2. Columnar decode of the raw `Map(LowCardinality(String), String)` label
//      column, which ch-go's Auto inference cannot construct, falls back to the
//      row decoder and returns 200 rather than a 502.
//   3. Classic-histogram discovery + reconstruction — the `_bucket` / `_count`
//      / `_sum` companions are advertised and queryable off the single
//      bare-name histogram row.
//
// Gated by the `integration` build tag (needs Docker) and NOT wired into any CI
// lane — `internal/api/prom` has no integration step — so this is a
// locally-runnable reproduction: `go test -tags=integration ./internal/api/prom/`.
// The columnar `Map(LowCardinality(String), String)` fall-back it exercises IS
// guarded in CI by internal/chclient's integration lane (chdb.yml runs
// `just chclient-integration`, which executes
// TestColumnarLowCardinalityMapFallback_E2E); this test additionally walks the
// full Prom HTTP path end to end.

package prom_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// realExporterHistogramDDL mirrors the upstream clickhouseexporter templates:
// LowCardinality-keyed attribute maps, the full histogram column set, and the
// exponential-histogram table under its true default name — with NO
// ZeroThreshold column (the exporter's exp-histogram DDL does not persist it,
// which is why schema.DefaultOTelMetrics leaves ZeroThresholdColumn empty).
const realExporterHistogramDDL = `
CREATE TABLE otel_metrics_gauge (
    ResourceAttributes Map(LowCardinality(String), String),
    ServiceName LowCardinality(String),
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(LowCardinality(String), String),
    StartTimeUnix DateTime64(9),
    TimeUnix DateTime64(9),
    Value Float64,
    Flags UInt32,
    AggregationTemporality Int32,
    IsMonotonic Boolean
) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, TimeUnix);

CREATE TABLE otel_metrics_sum (
    ResourceAttributes Map(LowCardinality(String), String),
    ServiceName LowCardinality(String),
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(LowCardinality(String), String),
    StartTimeUnix DateTime64(9),
    TimeUnix DateTime64(9),
    Value Float64,
    Flags UInt32,
    AggregationTemporality Int32,
    IsMonotonic Boolean
) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, TimeUnix);

CREATE TABLE otel_metrics_histogram (
    ResourceAttributes Map(LowCardinality(String), String),
    ServiceName LowCardinality(String),
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(LowCardinality(String), String),
    StartTimeUnix DateTime64(9),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64),
    Flags UInt32,
    Min Float64,
    Max Float64,
    AggregationTemporality Int32
) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, TimeUnix);

CREATE TABLE otel_metrics_exponential_histogram (
    ResourceAttributes Map(LowCardinality(String), String),
    ServiceName LowCardinality(String),
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(LowCardinality(String), String),
    StartTimeUnix DateTime64(9),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    Scale Int32,
    ZeroCount UInt64,
    PositiveOffset Int32,
    PositiveBucketCounts Array(UInt64),
    NegativeOffset Int32,
    NegativeBucketCounts Array(UInt64),
    Flags UInt32,
    Min Float64,
    Max Float64,
    AggregationTemporality Int32
) ENGINE = MergeTree() ORDER BY (ServiceName, MetricName, TimeUnix);`

func TestHistogram_RealExporterSchema_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:25.8-alpine",
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

	cfg := chclient.Config{
		Addr:     host + ":" + port.Port(),
		Database: "otel",
		Username: "cerberus",
		Password: "cerberus",
	}
	client, err := chclient.New(cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	// Enable the columnar matrix decode — the opt-in path whose ch-go Auto
	// inference cannot construct Map(LowCardinality(String), String) and, before
	// the fall-back fix, 502'd on the exporter's label columns.
	client.UseColumnarMatrixDecode(true, cfg)

	for _, stmt := range splitDDL(realExporterHistogramDDL) {
		if err := client.Exec(ctx, stmt); err != nil {
			t.Fatalf("create table: %v\nstmt: %s", err, stmt)
		}
	}

	// Seed within the instant-query staleness window so an unqualified `time`
	// anchor still sees the rows. Truncate to the second: the eval anchor is
	// evalTS (whole seconds) and the bare `_bucket` LWR uses a strict
	// `TimeUnix <= eval_ts`, so a sub-second seed timestamp would sort just
	// after the anchor and be filtered out.
	seedTime := time.Now().UTC().Truncate(time.Second).Add(-90 * time.Second)
	ts := seedTime.Format("2006-01-02 15:04:05.000")

	// Classic histogram, dotted OTel name, one series. ExplicitBounds [1,2,4]
	// with BucketCounts [1,2,3,4] gives cumulative le-counts 1/3/6/10, so
	// histogram_quantile(0.5) interpolates inside (2,4] to 2 + 2*(5-3)/(6-3) =
	// 10/3, and histogram_quantile(0.9) lands in the +Inf bucket -> 4.
	if err := client.Exec(ctx, fmt.Sprintf(`
INSERT INTO otel_metrics_histogram
    (ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit,
     Attributes, StartTimeUnix, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds,
     AggregationTemporality)
VALUES
    (map('service.name','api'), 'api', 'test.hist.metric', 'test histogram', 's',
     map('method','GET'), toDateTime64('%[1]s', 9), toDateTime64('%[1]s', 9),
     10, 18.0, [1, 2, 3, 4], [1, 2, 4], 2)`, ts)); err != nil {
		t.Fatalf("seed histogram: %v", err)
	}

	// Exponential histogram. The `_exp_hist` suffix (schema.ExpHistogramSuffix)
	// routes histogram_quantile onto the native lowering, which reads the
	// exp-histogram table BY NAME — so this query only succeeds if that name is
	// the exporter's real `otel_metrics_exponential_histogram`.
	if err := client.Exec(ctx, fmt.Sprintf(`
INSERT INTO otel_metrics_exponential_histogram
    (ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit,
     Attributes, StartTimeUnix, TimeUnix, Count, Sum, Scale, ZeroCount,
     PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts,
     AggregationTemporality)
VALUES
    (map('service.name','api'), 'api', 'latency_exp_hist', 'test exp histogram', 's',
     map('method','GET'), toDateTime64('%[1]s', 9), toDateTime64('%[1]s', 9),
     10, 20.0, 0, 0, 0, [2, 3, 5], 0, [], 2)`, ts)); err != nil {
		t.Fatalf("seed exp histogram: %v", err)
	}

	h := prom.New(client, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	evalTS := seedTime.Unix()

	// --- 1. Discovery: classic-histogram companions advertised, exp + bare
	// names withheld (exactly reference Prometheus behaviour). ---
	names := getNameValues(t, srv.URL)
	for _, want := range []string{
		"test_hist_metric_bucket", "test_hist_metric_count", "test_hist_metric_sum",
	} {
		if !contains(names, want) {
			t.Errorf("__name__ values missing %q: got %v", want, names)
		}
	}
	for _, absent := range []string{"test_hist_metric", "latency_exp_hist"} {
		if contains(names, absent) {
			t.Errorf("__name__ values should not advertise %q: got %v", absent, names)
		}
	}

	// --- 2. Classic histogram_quantile, columnar ON — the instant shape that
	// projects the raw Map(LowCardinality(String), String) label column and
	// 502'd before the columnar fall-back. Must be 200 with the exact value. ---
	assertScalarQuantile(t, srv.URL, evalTS,
		"histogram_quantile(0.5, test_hist_metric_bucket)", 10.0/3.0)
	assertScalarQuantile(t, srv.URL, evalTS,
		"histogram_quantile(0.9, test_hist_metric_bucket)", 4.0)

	// --- 3. Bare `_bucket` selector reconstructs the per-le series. ---
	buckets := runInstant(t, srv.URL, evalTS, "test_hist_metric_bucket")
	if len(buckets) == 0 {
		t.Fatalf("test_hist_metric_bucket returned no series")
	}

	// --- 4. native exp-histogram quantile resolves against
	// otel_metrics_exponential_histogram. Positive buckets [2,3,5] at scale 0
	// (base 2) put the median in the (4,8] bucket boundary -> 4. ---
	assertScalarQuantile(t, srv.URL, evalTS,
		"histogram_quantile(0.5, latency_exp_hist)", 4.0)
}

// splitDDL splits a multi-statement DDL string on `;` into individual
// non-empty CREATE statements (the driver executes one statement per Exec).
func splitDDL(ddl string) []string {
	var out []string
	for _, part := range strings.Split(ddl, ";") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func assertScalarQuantile(t *testing.T, base string, evalTS int64, query string, want float64) {
	t.Helper()
	res := runInstant(t, base, evalTS, query)
	if len(res) != 1 {
		t.Fatalf("%s: expected 1 series, got %d: %+v", query, len(res), res)
	}
	got := res[0].value
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s: got %v, want %v", query, got, want)
	}
}

type instantSeries struct {
	metric map[string]string
	value  float64
}

func runInstant(t *testing.T, base string, evalTS int64, query string) []instantSeries {
	t.Helper()
	u := fmt.Sprintf("%s/api/v1/query?query=%s&time=%d", base, url.QueryEscape(query), evalTS)
	resp, err := http.Get(u) //nolint:noctx // test helper
	if err != nil {
		t.Fatalf("GET %s: %v", query, err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", query, resp.StatusCode, body)
	}
	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("%s: decode: %v body=%s", query, err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("%s: status=%q err=%s", query, parsed.Status, parsed.Error)
	}
	raw, _ := json.Marshal(parsed.Data.Result)
	var vec []struct {
		Metric map[string]string `json:"metric"`
		Value  [2]any            `json:"value"`
	}
	if err := json.Unmarshal(raw, &vec); err != nil {
		t.Fatalf("%s: decode vector: %v body=%s", query, err, body)
	}
	out := make([]instantSeries, 0, len(vec))
	for _, s := range vec {
		valStr, _ := s.Value[1].(string)
		var f float64
		if _, err := fmt.Sscanf(valStr, "%g", &f); err != nil {
			t.Fatalf("%s: parse value %q: %v", query, valStr, err)
		}
		out = append(out, instantSeries{metric: s.Metric, value: f})
	}
	return out
}

func getNameValues(t *testing.T, base string) []string {
	t.Helper()
	resp, err := http.Get(base + "/api/v1/label/__name__/values") //nolint:noctx // test helper
	if err != nil {
		t.Fatalf("GET __name__ values: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("__name__ values status=%d body=%s", resp.StatusCode, body)
	}
	var env struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode __name__ values: %v body=%s", err, body)
	}
	return env.Data
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

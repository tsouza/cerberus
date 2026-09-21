//go:build integration

package nightly

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopttest"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
	"github.com/tsouza/cerberus/internal/solver"
)

const (
	histogramReportCHImage     = "clickhouse/clickhouse-server:26.6.1.1193-alpine"
	histogramReportMetric      = "core_http_request_duration_seconds"
	histogramReportEnvironment = "prod-aws-us-east-1-default"
	// The fixed shared range query reads the captured table once and stays
	// below the production 1 GiB limit with useful headroom. These ceilings
	// reject the old per-quantile fan-out/state construction without pinning
	// noisy wall-clock timing.
	histogramReportMaxReadRows       = 1400000
	histogramReportMaxPeakMemoryByte = 800 << 20
)

// TestHistogramReportLFSRealCH uses captured samples, preserving every original
// series, timestamp, bucket, reset and temporality. Only the scrubbed metric and
// environment names are aliased so the incident's exact PromQL can be replayed.
// This is a classic-histogram capture, not captured native-histogram telemetry.
func TestHistogramReportLFSRealCH(t *testing.T) {
	const testTimeout = 20 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	container, client := startNightlyCH(ctx, t, histogramReportCHImage)
	conn := client.Conn()
	if err := ddl.Apply(ctx, conn, []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatal(err)
	}
	rows, err := loadHistogramSample(ctx, container, client, sampleParquetPath(t, "svc_http_request_duration_seconds.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	const capturedHistogramRows = 1317183
	if rows != capturedHistogramRows {
		t.Fatalf("LFS capture rows=%d, want %d", rows, capturedHistogramRows)
	}
	var profile string
	if err := conn.QueryRow(ctx, `SELECT toJSONString(tuple(version(), count(), uniqExact(tuple(Attributes, ResourceAttributes)), min(TimeUnix), max(TimeUnix), groupUniqArray(AggregationTemporality), min(length(BucketCounts)), max(length(BucketCounts)))) FROM otel_metrics_histogram WHERE MetricName = ?`, histogramMetric).Scan(&profile); err != nil {
		t.Fatal(err)
	}
	t.Logf("LFS source profile (version, rows, series, first, last, temporalities, bucket widths): %s", profile)
	if err := conn.Exec(ctx, `INSERT INTO otel_metrics_histogram SELECT * REPLACE (? AS MetricName, mapUpdate(ResourceAttributes, map('deployment.environment.name', ?)) AS ResourceAttributes) FROM (SELECT * FROM otel_metrics_histogram WHERE MetricName = ?)`, histogramReportMetric, histogramReportEnvironment, histogramMetric); err != nil {
		t.Fatal(err)
	}
	var replayRows uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM otel_metrics_histogram WHERE MetricName = ?`, histogramReportMetric).Scan(&replayRows); err != nil {
		t.Fatal(err)
	}
	if replayRows != rows {
		t.Fatalf("aliased replay rows=%d, want %d", replayRows, rows)
	}
	// The capture's morning window is continuous; include the full rate lookback
	// before the first anchor and stay clear of the gap between sampling windows.
	start := time.Date(2026, 8, 18, 9, 5, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	const step = 30 * time.Second
	const inner = `sum by (le, k8s_deployment_name)(rate(core_http_request_duration_seconds_bucket{deployment_environment_name="prod-aws-us-east-1-default",k8s_deployment_name=~".*"}[5m]))`
	oracles := loadHistogramReportOracle(ctx, t, client, inner, start, end, step)
	for _, mode := range []string{"auto", "auto,quantile_prom_histogram"} {
		t.Run(mode, func(t *testing.T) {
			set := chopttest.ResolveEnabledSet(ctx, t, client, mode)
			handler := prom.New(client, schema.DefaultOTelMetrics(), nil)
			handler.Lowerers = choptwire.RangeLowerers(set)
			rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
			rules.ResultCache = false
			handler.Engine.SetSettings(rules)
			solverConfig := solver.DefaultConfig()
			solverConfig.Mode = solver.ModeAuto
			handler.Engine.Solver = solver.New(solverConfig, engine.ChsqlEmitter{}, solver.ExecDeps{Client: client, Breaker: client})
			mux := http.NewServeMux()
			handler.Mount(mux)
			for _, target := range []struct{ name, expression string }{
				{"buckets", inner},
				{"p50", "histogram_quantile(0.5," + inner + ")"},
				{"p95", "histogram_quantile(0.95," + inner + ")"},
				{"p99", "histogram_quantile(0.99," + inner + ")"},
				{"shared", "histogram_quantiles((" + inner + "),\"quantile\",0.5,0.95,0.99)"},
			} {
				for _, instant := range []bool{true, false} {
					t.Run(fmt.Sprintf("%s/instant=%t", target.name, instant), func(t *testing.T) {
						queryID := fmt.Sprintf("histogram-report-lfs-%s-%s-%t", mode, target.name, instant)
						digest := sha256.Sum256([]byte(queryID))
						var traceID trace.TraceID
						var spanID trace.SpanID
						copy(traceID[:], digest[:len(traceID)])
						copy(spanID[:], digest[len(traceID):len(traceID)+len(spanID)])
						requestContext := trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID}))
						params := url.Values{"query": {target.expression}, "start": {formatPromTime(start)}, "end": {formatPromTime(end)}, "time": {formatPromTime(end)}, "step": {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)}}
						path := "/api/v1/query_range"
						if instant {
							path = "/api/v1/query"
						}
						req := httptest.NewRequest(http.MethodGet, path+"?"+params.Encode(), nil).WithContext(chclient.WithQueryID(requestContext, queryID))
						rec := httptest.NewRecorder()
						before := time.Now()
						mux.ServeHTTP(rec, req)
						t.Logf("HTTP=%d wall=%s bytes=%d route=%s", rec.Code, time.Since(before), rec.Body.Len(), rec.Header().Get("X-Cerberus-Route-Decision"))
						if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
							t.Fatal(err)
						}
						var metrics string
						if err := conn.QueryRow(ctx, `SELECT toJSONString(groupArray(tuple(type, query_id, query_duration_ms, read_rows, read_bytes, memory_usage, length(query), exception))) FROM system.query_log WHERE (startsWith(query_id, ?) OR startsWith(query_id, ?)) AND type != 'QueryStart'`, queryID, traceID.String()+"-").Scan(&metrics); err != nil {
							t.Fatal(err)
						}
						t.Logf("ClickHouse executions (type, id, ms, rows, bytes, peak bytes, SQL bytes, exception): %s", metrics)
						assertHistogramReportExecution(ctx, t, conn, queryID, traceID.String()+"-", mode, target.name)
						if rec.Code != http.StatusOK {
							t.Fatalf("%s", rec.Body.String())
						}
						assertHistogramReportLFSResponse(t, rec.Body.Bytes(), instant)
						assertHistogramReportOracle(t, rec.Body.Bytes(), oracles[target.name], instant, end)
					})
				}
			}
		})
	}
}

func assertHistogramReportExecution(ctx context.Context, t *testing.T, conn driver.Conn, queryID, tracePrefix, mode, target string) {
	t.Helper()
	var executions, readRows, peakMemory, pluralCalls uint64
	err := conn.QueryRow(ctx, `SELECT count(), max(read_rows), max(memory_usage), countIf(position(query, 'quantilesPrometheusHistogram(') > 0) FROM system.query_log WHERE (startsWith(query_id, ?) OR startsWith(query_id, ?)) AND type = 'QueryFinish'`, queryID, tracePrefix).
		Scan(&executions, &readRows, &peakMemory, &pluralCalls)
	if err != nil {
		t.Fatal(err)
	}
	if executions != 1 {
		t.Fatalf("%s executed %d ClickHouse queries, want 1", target, executions)
	}
	if readRows > histogramReportMaxReadRows {
		t.Fatalf("%s read %d rows, ceiling %d", target, readRows, histogramReportMaxReadRows)
	}
	if peakMemory > histogramReportMaxPeakMemoryByte {
		t.Fatalf("%s peak memory %d, ceiling %d", target, peakMemory, histogramReportMaxPeakMemoryByte)
	}
	wantPluralCalls := uint64(0)
	if target == "shared" && strings.Contains(mode, "quantile_prom_histogram") {
		wantPluralCalls = 1
	}
	if pluralCalls != wantPluralCalls {
		t.Fatalf("plural ClickHouse aggregate executions=%d, want %d for mode %q", pluralCalls, wantPluralCalls, mode)
	}
}

func assertHistogramReportLFSResponse(t *testing.T, body []byte, instant bool) {
	t.Helper()
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string   `json:"metric"`
				Value  []json.RawMessage   `json:"value"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	wantType := "matrix"
	if instant {
		wantType = "vector"
	}
	if response.Status != "success" || response.Data.ResultType != wantType || len(response.Data.Result) == 0 {
		t.Fatalf("expected nonempty %s: %s", wantType, body)
	}
	var finiteSamples int
	for _, result := range response.Data.Result {
		if strings.TrimSpace(result.Metric["k8s_deployment_name"]) == "" {
			t.Fatalf("missing deployment label: %v", result.Metric)
		}
		samples := result.Values
		if instant {
			samples = [][]json.RawMessage{result.Value}
		}
		for _, sample := range samples {
			if len(sample) != 2 {
				t.Fatalf("malformed sample: %s", sample)
			}
			var encoded string
			if err := json.Unmarshal(sample[1], &encoded); err != nil {
				t.Fatal(err)
			}
			value, err := strconv.ParseFloat(encoded, 64)
			if err != nil {
				t.Fatal(err)
			}
			if !math.IsNaN(value) && !math.IsInf(value, 0) {
				finiteSamples++
			}
		}
	}
	if finiteSamples == 0 {
		t.Fatal("no finite samples returned from nonempty production capture")
	}
	t.Logf("returned series=%d finite samples=%d", len(response.Data.Result), finiteSamples)
}

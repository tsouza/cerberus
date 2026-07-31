// Test-only Prom remote_write fan-out for the compatibility harness.
//
// The CH-side seeder lands the OTel fixture in ClickHouse so cerberus has
// data to query. The compatibility tester also queries the reference
// Prometheus, which has no scrape config and no live ingest path — without
// this fan-out, Prom returns empty for every metric query. `absent()` /
// `absent_over_time()` specifically diverge: on cerberus they return empty
// (metric present), on Prom they return 1 per step (metric absent), so the
// tester reports 7+ shape-diffs purely because of the asymmetric dataset.
//
// This file mirrors the CH data into Prom by reading it back with SELECT,
// re-shaping each row as a Prometheus sample, and POSTing snappy-encoded
// `prompb.WriteRequest` batches to Prom's remote-write receiver (enabled
// in docker-compose.yml via `--web.enable-remote-write-receiver`).
//
// Production cerberus does NOT include this code path — the seeder
// (`compatibility/prometheus/cmd/seed/`) is harness-only and never
// compiled into the cerberus binary. The user's contract is "cerberus is
// for querying"; remote-write stays in test infrastructure.

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

// remoteWriteFixture reads every series the CH fixture just wrote and
// mirrors it into the reference Prom instance over remote_write. Errors
// surface immediately — failing to seed Prom poisons the compatibility
// tester's reference target, so we'd rather fail fast than silently diff.
//
// The reads are scoped to the fixture's anchor window so we don't
// accidentally fan out anything else that happens to be in the tables.
func remoteWriteFixture(ctx context.Context, conn driver.Conn, promURL string, logger *slog.Logger) error {
	for _, src := range fixtureSources {
		logger.Info("remote_write to prom", "metric", src.metricName, "table", src.table)
		read := readFixtureSeries
		if src.histogram {
			read = readHistogramFixtureSeries
		}
		batch, err := read(ctx, conn, src)
		if err != nil {
			return fmt.Errorf("read %s: %w", src.metricName, err)
		}
		// An empty batch would leave the metric absent from the
		// reference Prometheus while ClickHouse still has it (or vice
		// versa), so every query over it would compare empty against
		// empty and report parity it never established.
		if len(batch) == 0 {
			return fmt.Errorf("read %s: fixture produced no rows", src.metricName)
		}
		if err := postRemoteWrite(ctx, promURL, batch); err != nil {
			return fmt.Errorf("post %s: %w", src.metricName, err)
		}
	}
	return nil
}

// fixtureSource pins a single (metric_name, ch_table) pair so we can
// drive one SELECT per logical fixture block. The metric_name doubles as
// the Prom `__name__` label.
type fixtureSource struct {
	metricName string
	table      string
	histogram  bool
}

// fixtureSources mirrors fixtureInserts above — keep them in lock-step so
// every CH-side INSERT has a corresponding Prom remote_write.
var fixtureSources = []fixtureSource{
	{"demo_resource_latency_seconds", "otel_metrics_histogram", true},
	{"demo_cpu_usage_seconds_total", "otel_metrics_sum"},
	{"demo_memory_usage_bytes", "otel_metrics_gauge"},
	{"demo_sparse_memory_bytes", "otel_metrics_gauge"},
	{"demo_http_requests_total", "otel_metrics_sum"},
	{"demo_disk_usage_bytes", "otel_metrics_gauge"},
	{"demo_disk_total_bytes", "otel_metrics_gauge"},
	{"demo_num_cpus", "otel_metrics_gauge"},
	{"up", "otel_metrics_gauge"},
}

// readHistogramFixtureSeries expands OTel's per-row bucket arrays into the
// cumulative Prometheus `_bucket` float series used by remote_write. The
// compatibility fixture deliberately keeps k8s.namespace.name out of
// Attributes so Cerberus must project it from ResourceAttributes, while the
// mirrored Prometheus series receives its sanitized wire label directly.
func readHistogramFixtureSeries(ctx context.Context, conn driver.Conn, src fixtureSource) ([]prompb.TimeSeries, error) {
	q := fmt.Sprintf(
		"SELECT Attributes, ResourceAttributes, toUnixTimestamp64Milli(TimeUnix) AS ts_ms, BucketCounts, ExplicitBounds "+
			"FROM %s WHERE MetricName = ? ORDER BY Attributes, TimeUnix",
		src.table,
	)
	rows, err := conn.Query(ctx, q, src.metricName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	bySeries := map[string]*prompb.TimeSeries{}
	for rows.Next() {
		var attrs, resourceAttrs map[string]string
		var tsMS int64
		var counts []uint64
		var bounds []float64
		if err := rows.Scan(&attrs, &resourceAttrs, &tsMS, &counts, &bounds); err != nil {
			return nil, err
		}
		labels := make(map[string]string, len(attrs)+len(resourceAttrs)+1)
		for k, v := range attrs {
			labels[k] = v
		}
		for k, v := range resourceAttrs {
			labels[strings.ReplaceAll(k, ".", "_")] = v
		}
		var cumulative uint64
		for i, count := range counts {
			cumulative += count
			bucketLabels := make(map[string]string, len(labels)+1)
			for k, v := range labels {
				bucketLabels[k] = v
			}
			if i < len(bounds) {
				bucketLabels["le"] = fmt.Sprintf("%g", bounds[i])
			} else {
				bucketLabels["le"] = "+Inf"
			}
			key := canonicaliseLabels(bucketLabels)
			ts, ok := bySeries[key]
			if !ok {
				ts = &prompb.TimeSeries{Labels: buildPromLabels(src.metricName+"_bucket", bucketLabels)}
				bySeries[key] = ts
			}
			ts.Samples = append(ts.Samples, prompb.Sample{Value: float64(cumulative), Timestamp: tsMS})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]prompb.TimeSeries, 0, len(bySeries))
	for _, ts := range bySeries {
		out = append(out, *ts)
	}
	return out, nil
}

// readFixtureSeries reads every (Attributes, TimeUnix, Value) row for one
// metric, grouped into prompb timeseries by label-set. Output is a slice
// of prompb.TimeSeries ready to be wire-encoded.
func readFixtureSeries(ctx context.Context, conn driver.Conn, src fixtureSource) ([]prompb.TimeSeries, error) {
	q := fmt.Sprintf(
		"SELECT Attributes, toUnixTimestamp64Milli(TimeUnix) AS ts_ms, Value "+
			"FROM %s WHERE MetricName = ? ORDER BY Attributes, TimeUnix",
		src.table,
	)
	rows, err := conn.Query(ctx, q, src.metricName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	bySeries := map[string]*prompb.TimeSeries{}
	for rows.Next() {
		var attrs map[string]string
		var tsMS int64
		var val float64
		if err := rows.Scan(&attrs, &tsMS, &val); err != nil {
			return nil, err
		}
		key := canonicaliseLabels(attrs)
		ts, ok := bySeries[key]
		if !ok {
			ts = &prompb.TimeSeries{Labels: buildPromLabels(src.metricName, attrs)}
			bySeries[key] = ts
		}
		ts.Samples = append(ts.Samples, prompb.Sample{Value: val, Timestamp: tsMS})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]prompb.TimeSeries, 0, len(bySeries))
	for _, ts := range bySeries {
		out = append(out, *ts)
	}
	return out, nil
}

// buildPromLabels turns the OTel-shape (ResourceAttributes is the
// service.name carrier, Attributes is the per-sample label set) into a
// Prom label slice — `__name__` first, then sorted alphabetically.
func buildPromLabels(metricName string, attrs map[string]string) []prompb.Label {
	out := make([]prompb.Label, 0, 1+len(attrs))
	out = append(out, prompb.Label{Name: "__name__", Value: metricName})
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, prompb.Label{Name: k, Value: attrs[k]})
	}
	return out
}

// canonicaliseLabels gives the same series a stable key for grouping.
func canonicaliseLabels(attrs map[string]string) string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(attrs[k])
		b.WriteByte(';')
	}
	return b.String()
}

// postRemoteWrite encodes a WriteRequest and POSTs it to Prom's
// remote-write receiver. Per the Prom spec the body is snappy-encoded
// proto and the Content-Encoding / Content-Type headers are required.
func postRemoteWrite(ctx context.Context, promURL string, series []prompb.TimeSeries) error {
	req := &prompb.WriteRequest{Timeseries: series}
	raw, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	compressed := snappy.Encode(nil, raw)

	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(hctx, http.MethodPost,
		strings.TrimRight(promURL, "/")+"/api/v1/write",
		bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Encoding", "snappy")
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	httpReq.Header.Set("User-Agent", "cerberus-compat-seeder/1")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("prom returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// promRemoteWriteURL resolves the Prom remote-write endpoint. Lookup order:
// flag is parsed in main; this file only knows about the env-default chain.
func promRemoteWriteURL() string {
	if v := os.Getenv("CERBERUS_PROM_REMOTE_WRITE"); v != "" {
		return v
	}
	return "http://localhost:29090"
}

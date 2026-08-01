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

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
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
		batch, err := readFixtureSeries(ctx, conn, src)
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
	for _, src := range histogramFixtureSources {
		logger.Info("remote_write histogram to prom", "base", src.base, "table", src.table)
		batch, err := readHistogramFixtureSeries(ctx, conn, src)
		if err != nil {
			return fmt.Errorf("read %s: %w", src.base, err)
		}
		// Same reasoning as the Value-shaped loop above: an empty batch
		// means the reference Prometheus never sees the family, so every
		// query over it compares empty against empty.
		if len(batch) == 0 {
			return fmt.Errorf("read %s: fixture produced no rows", src.base)
		}
		if err := postRemoteWrite(ctx, promURL, batch); err != nil {
			return fmt.Errorf("post %s: %w", src.base, err)
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
}

// fixtureSources and histogramFixtureSources together mirror
// fixtureInserts in main.go — keep them in lock-step so every CH-side
// INSERT has a corresponding Prom remote_write. The lock-step is enforced,
// not merely documented: test/regression/compat_promql_seed_corpus_test.go
// fails when the two lists diverge.
var fixtureSources = []fixtureSource{
	{"demo_cpu_usage_seconds_total", "otel_metrics_sum"},
	{"demo_memory_usage_bytes", "otel_metrics_gauge"},
	{"demo_sparse_memory_bytes", "otel_metrics_gauge"},
	{"demo_http_requests_total", "otel_metrics_sum"},
	{"demo_disk_usage_bytes", "otel_metrics_gauge"},
	{"demo_disk_total_bytes", "otel_metrics_gauge"},
	{"demo_num_cpus", "otel_metrics_gauge"},
	{"demo_batch_last_success_timestamp_seconds", "otel_metrics_gauge"},
	{"demo_intermittent_metric", "otel_metrics_gauge"},
	{"up", "otel_metrics_gauge"},
}

// histogramFixtureSource mirrors a classic-histogram fixture. `base` is
// the BARE MetricName the row is stored under; the Prom wire surface is
// the synthetic names promql.HistogramSyntheticNames derives from it. The
// bare name is deliberately NOT mirrored: cerberus's lowering never serves
// it, so remote-writing it would manufacture a diff the fixture invented.
type histogramFixtureSource struct {
	base  string
	table string
}

var histogramFixtureSources = []histogramFixtureSource{
	{"demo_api_request_duration_seconds", "otel_metrics_histogram"},
	{"demo_resource_latency_seconds", "otel_metrics_histogram"},
}

// histogramFixtureSelect computes the `le` labels, the cumulative counts
// and the Prom-sanitized resource labels IN CLICKHOUSE, using the same
// expressions cerberus's own lowering emits — the classic bucket fan-out
// (internal/promql/histogram_bucket.go) for the first two and the selector
// attribute projection (internal/promql/lower.go) for the third:
//
//	le    = if(i > length(ExplicitBounds), '+Inf', toString(ExplicitBounds[i]))
//	cum   = arraySum(arraySlice(BucketCounts, 1, i))
//	rlkey = replaceRegexpAll(k, '[^a-zA-Z0-9_]', '_')
//
// Re-deriving them in Go would put a second float-to-label formatter and a
// second label sanitiser in the harness, and a `"0.5"` vs `"0.50"` (or a
// `k8s_namespace_name` vs `k8s.namespace.name`) disagreement between the
// two would hard-diff every bucket series for a reason the fixture
// invented. Computing them once, server-side, makes the two sides equal by
// construction.
//
// A fixture whose identifying label lives in ResourceAttributes rather than
// Attributes is the point of the resource arm: cerberus must project it out
// of the resource layer, while the mirrored Prometheus series carries it as
// an ordinary wire label.
const histogramFixtureSelect = `SELECT
        Attributes,
        mapFromArrays(
            arrayMap(k -> replaceRegexpAll(k, '[^a-zA-Z0-9_]', '_'), mapKeys(ResourceAttributes)),
            mapValues(ResourceAttributes)) AS resource_labels,
        toUnixTimestamp64Milli(TimeUnix) AS ts_ms,
        arrayMap(i -> if(i > length(ExplicitBounds), '+Inf', toString(ExplicitBounds[i])),
                 arrayEnumerate(BucketCounts)) AS le_labels,
        arrayMap(i -> toFloat64(arraySum(arraySlice(BucketCounts, 1, i))),
                 arrayEnumerate(BucketCounts)) AS cum_counts,
        toFloat64(Count) AS count_value,
        Sum AS sum_value
    FROM %s
    WHERE MetricName = ?
    ORDER BY Attributes, TimeUnix`

// leLabel is the Prom-convention bucket-boundary label. It is the one
// label the mirror synthesises rather than reading out of Attributes,
// because in the OTel-CH layout the boundary lives in an array column.
const leLabel = "le"

// promSeriesSet accumulates samples into prompb time series keyed by
// (metric name, label set), preserving first-seen order so the wire batch
// is deterministic. The name is part of the key because the histogram
// mirror emits three families off ONE CH row — `_bucket`, `_count`,
// `_sum` — and the count/sum arms share an attribute set: keying on labels
// alone would fold them into a single series.
type promSeriesSet struct {
	bySeries map[string]*prompb.TimeSeries
	order    []string
}

func newPromSeriesSet() *promSeriesSet {
	return &promSeriesSet{bySeries: map[string]*prompb.TimeSeries{}}
}

func (p *promSeriesSet) add(metricName string, labels map[string]string, tsMS int64, value float64) {
	key := metricName + "\x00" + canonicaliseLabels(labels)
	ts, ok := p.bySeries[key]
	if !ok {
		ts = &prompb.TimeSeries{Labels: buildPromLabels(metricName, labels)}
		p.bySeries[key] = ts
		p.order = append(p.order, key)
	}
	ts.Samples = append(ts.Samples, prompb.Sample{Value: value, Timestamp: tsMS})
}

func (p *promSeriesSet) series() []prompb.TimeSeries {
	out := make([]prompb.TimeSeries, 0, len(p.order))
	for _, key := range p.order {
		out = append(out, *p.bySeries[key])
	}
	return out
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

	acc := newPromSeriesSet()
	for rows.Next() {
		var attrs map[string]string
		var tsMS int64
		var val float64
		if err := rows.Scan(&attrs, &tsMS, &val); err != nil {
			return nil, err
		}
		acc.add(src.metricName, attrs, tsMS, val)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return acc.series(), nil
}

// histogramWireNames resolves the three Prom-wire family names one
// classic-histogram row surfaces as. The names come from the production
// enumeration (promql.HistogramSyntheticNames) and are classified with the
// production router (schema.Metrics.HistogramCompanionColumn) rather than
// from a hardcoded suffix list, so a lowering change that adds a fourth
// companion fails loudly here instead of silently under-mirroring.
func histogramWireNames(base string, m schema.Metrics) (bucketName, countName, sumName string, err error) {
	for _, name := range promql.HistogramSyntheticNames(base, m) {
		bare, col, isCompanion := m.HistogramCompanionColumn(name)
		switch {
		case !isCompanion:
			if bucketName != "" {
				return "", "", "", fmt.Errorf(
					"histogram %q yields two array-fanned names (%q and %q); "+
						"teach the mirror which column each one projects", base, bucketName, name,
				)
			}
			bucketName = name
		case bare != base:
			return "", "", "", fmt.Errorf(
				"histogram companion %q resolves to base %q, not %q", name, bare, base,
			)
		case col == m.CountColumn:
			countName = name
		case col == m.SumColumn:
			sumName = name
		default:
			return "", "", "", fmt.Errorf(
				"histogram companion %q maps to unknown column %q", name, col,
			)
		}
	}
	if bucketName == "" || countName == "" || sumName == "" {
		return "", "", "", fmt.Errorf(
			"histogram %q enumerated an incomplete wire surface (bucket=%q count=%q sum=%q)",
			base, bucketName, countName, sumName,
		)
	}
	return bucketName, countName, sumName, nil
}

// readHistogramFixtureSeries mirrors one classic-histogram family. Each CH
// row becomes one sample on each of the `_count` and `_sum` series plus one
// sample on every `_bucket` series the row's boundary array names.
func readHistogramFixtureSeries(
	ctx context.Context, conn driver.Conn, src histogramFixtureSource,
) ([]prompb.TimeSeries, error) {
	m := schema.DefaultOTelMetrics()
	bucketName, countName, sumName, err := histogramWireNames(src.base, m)
	if err != nil {
		return nil, err
	}

	rows, err := conn.Query(ctx, fmt.Sprintf(histogramFixtureSelect, src.table), src.base)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	acc := newPromSeriesSet()
	for rows.Next() {
		var attrs, resourceLabels map[string]string
		var tsMS int64
		var leLabels []string
		var cumCounts []float64
		var countValue, sumValue float64
		if err := rows.Scan(
			&attrs, &resourceLabels, &tsMS, &leLabels, &cumCounts, &countValue, &sumValue,
		); err != nil {
			return nil, err
		}
		// The resource layer is folded into the wire label set for every
		// arm of the family, so `_bucket`, `_count` and `_sum` carry the
		// same series identity cerberus projects for them.
		for k, v := range resourceLabels {
			attrs[k] = v
		}
		if len(leLabels) != len(cumCounts) {
			return nil, fmt.Errorf(
				"histogram %q row at %d ms: %d bucket labels vs %d cumulative counts",
				src.base, tsMS, len(leLabels), len(cumCounts),
			)
		}
		for i, le := range leLabels {
			bucketLabels := make(map[string]string, len(attrs)+1)
			for k, v := range attrs {
				bucketLabels[k] = v
			}
			bucketLabels[leLabel] = le
			acc.add(bucketName, bucketLabels, tsMS, cumCounts[i])
		}
		acc.add(countName, attrs, tsMS, countValue)
		acc.add(sumName, attrs, tsMS, sumValue)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return acc.series(), nil
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

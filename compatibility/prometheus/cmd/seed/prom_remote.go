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
	"math"
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
	for _, src := range expHistogramFixtureSources {
		logger.Info("remote_write native histogram to prom", "metric", src.name, "table", src.table)
		batch, err := readExpHistogramFixtureSeries(ctx, conn, src)
		if err != nil {
			return fmt.Errorf("read %s: %w", src.name, err)
		}
		// Same reasoning as the two loops above, and it is the reason this
		// mirror exists at all: without it the reference skips every native
		// histogram sample, and the corpus's histogram_* cases compare the
		// empty vector against the empty vector while reporting parity.
		if len(batch) == 0 {
			return fmt.Errorf("read %s: fixture produced no rows", src.name)
		}
		if err := postRemoteWrite(ctx, promURL, batch); err != nil {
			return fmt.Errorf("post %s: %w", src.name, err)
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
	// accumulateToCumulative mirrors a DELTA-temporality CH fixture (each
	// stored sample is the increase since the PREVIOUS sample only) as its
	// EQUIVALENT CUMULATIVE running-sum series on the reference Prometheus
	// side — reference Prometheus's remote-write wire format and its own
	// rate() / increase() have no delta-temporality concept at all, so a
	// literal passthrough of the raw delta values would hand Prometheus a
	// wrong, non-monotonic "counter" and produce a meaningless diff. Per
	// series (keyed by label set), readFixtureSeries adds each row's raw
	// value to a running total and mirrors THAT instead of the raw value —
	// see issue #1628. False (the default) is the literal passthrough
	// every pre-#1628 family uses, byte-identical to before this field
	// existed.
	accumulateToCumulative bool
}

// fixtureSources, histogramFixtureSources and expHistogramFixtureSources
// together mirror fixtureInserts in main.go — keep them in lock-step so every
// CH-side INSERT has a corresponding Prom remote_write. The lock-step is
// enforced, not merely documented:
// test/regression/compat_promql_seed_corpus_test.go fails when the lists
// diverge.
var fixtureSources = []fixtureSource{
	{metricName: "demo_cpu_usage_seconds_total", table: "otel_metrics_sum"},
	{metricName: "demo_memory_usage_bytes", table: "otel_metrics_gauge"},
	{metricName: "demo_sparse_memory_bytes", table: "otel_metrics_gauge"},
	{metricName: "demo_http_requests_total", table: "otel_metrics_sum"},
	{metricName: "demo_disk_usage_bytes", table: "otel_metrics_gauge"},
	{metricName: "demo_disk_total_bytes", table: "otel_metrics_gauge"},
	{metricName: "demo_num_cpus", table: "otel_metrics_gauge"},
	{metricName: "demo_batch_last_success_timestamp_seconds", table: "otel_metrics_gauge"},
	{metricName: "demo_intermittent_metric", table: "otel_metrics_gauge"},
	{metricName: "up", table: "otel_metrics_gauge"},
	{metricName: "demo_gauge_with_nan_run", table: "otel_metrics_gauge"},
	// demo_delta_requests_total: a DELTA-temporality counter (issue #1628).
	// The CH fixture stores raw per-step increments with
	// AggregationTemporality = 1; the reference Prometheus side has no
	// delta-temporality concept, so it needs the equivalent CUMULATIVE
	// running sum instead — see fixtureSource.accumulateToCumulative.
	{metricName: "demo_delta_requests_total", table: "otel_metrics_sum", accumulateToCumulative: true},
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

// expHistogramFixtureSource mirrors an exponential (native) histogram
// fixture. Unlike the classic layout there is no synthetic wire surface to
// derive: a native histogram rides on ONE series under its own name, with the
// whole distribution carried in the sample, so `name` is both the stored
// MetricName and the Prom `__name__`.
type expHistogramFixtureSource struct {
	name  string
	table string
}

var expHistogramFixtureSources = []expHistogramFixtureSource{
	{"demo_latency_exp_hist", "otel_metrics_exponential_histogram"},
	{"demo_shifting_latency_exp_hist", "otel_metrics_exponential_histogram"},
}

// promBucketIndexShift is the one-bucket offset between the two sparse
// encodings. OTel bucket index i covers (base^i, base^(i+1)] while Prometheus
// bucket index i covers (base^(i-1), base^i], so the same boundary pair is
// named one higher on the Prometheus side. This mirrors `initialOffset` in the
// upstream OTLP translator (prometheusremotewrite.convertBucketsLayout) —
// getting it wrong shifts every quantile by one bucket, a factor of two at
// scale 0.
const promBucketIndexShift = 1

// promZeroThreshold is the zero-bucket half-width the upstream OTLP translator
// stamps when OTLP carries none (its `defaultZeroThreshold`). Mirroring that
// constant keeps the reference seeing exactly what a real OTLP→Prometheus
// pipeline would hand it.
const promZeroThreshold = 1e-128

// expHistogramFixtureSelect reads the OTLP-shaped exponential-histogram
// columns. Everything the Prom wire form needs is a pure re-encoding of these
// (see otelBucketsToPromSpans), so unlike the classic mirror there is nothing
// to compute server-side.
const expHistogramFixtureSelect = `SELECT
        Attributes,
        mapFromArrays(
            arrayMap(k -> replaceRegexpAll(k, '[^a-zA-Z0-9_]', '_'), mapKeys(ResourceAttributes)),
            mapValues(ResourceAttributes)) AS resource_labels,
        toUnixTimestamp64Milli(TimeUnix) AS ts_ms,
        Count, Sum, Scale, ZeroCount,
        PositiveOffset, PositiveBucketCounts,
        NegativeOffset, NegativeBucketCounts
    FROM %s
    WHERE MetricName = ?
    ORDER BY Attributes, TimeUnix`

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
//
// The SELECT sanitises every resource key; WHICH of them become wire labels
// is decided in Go by the production predicate — see
// [mirroredResourceLabels].
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

func (p *promSeriesSet) seriesFor(metricName string, labels map[string]string) *prompb.TimeSeries {
	key := metricName + "\x00" + canonicaliseLabels(labels)
	ts, ok := p.bySeries[key]
	if !ok {
		ts = &prompb.TimeSeries{Labels: buildPromLabels(metricName, labels)}
		p.bySeries[key] = ts
		p.order = append(p.order, key)
	}
	return ts
}

func (p *promSeriesSet) add(metricName string, labels map[string]string, tsMS int64, value float64) {
	ts := p.seriesFor(metricName, labels)
	ts.Samples = append(ts.Samples, prompb.Sample{Value: value, Timestamp: tsMS})
}

// addHistogram appends a native-histogram sample. It shares seriesFor with the
// float arm because the two are mutually exclusive per series, not per batch:
// Prometheus rejects a series that carries both a float and a histogram sample
// at the same timestamp, so the fixture families that use this one never use
// add.
func (p *promSeriesSet) addHistogram(metricName string, labels map[string]string, h prompb.Histogram) {
	ts := p.seriesFor(metricName, labels)
	ts.Histograms = append(ts.Histograms, h)
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
	// running holds the per-series accumulated total for
	// src.accumulateToCumulative, keyed the same way promSeriesSet keys a
	// series (canonicalised label set) — the ORDER BY Attributes, TimeUnix
	// above guarantees one series' rows arrive together and in time order,
	// so a running total keyed by that string never mixes two series.
	var running map[string]float64
	if src.accumulateToCumulative {
		running = map[string]float64{}
	}
	for rows.Next() {
		var attrs map[string]string
		var tsMS int64
		var val float64
		if err := rows.Scan(&attrs, &tsMS, &val); err != nil {
			return nil, err
		}
		if running != nil {
			key := canonicaliseLabels(attrs)
			running[key] += val
			val = running[key]
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

// mirroredResourceLabels folds the Prom-sanitized resource keys of one row
// into its wire label set dst, promoting exactly the keys cerberus's
// resource arm promotes.
//
// The filter is the production predicate
// (promql.DedicatedResourceLabelExcluded), not a hardcoded key list: a
// resource key already backed by a dedicated top-level column — today only
// service.name → ServiceName — is never surfaced through the resource arm,
// because the dedicated column owns it. The fixture deliberately leaves
// ServiceName unpopulated (see insertFixture in main.go), so cerberus emits
// no `service_name` label at all for these series. Folding
// `service.name=demo` in here would manufacture a label on the REFERENCE
// side that cerberus never emits, hard-diffing every histogram series for a
// reason the mirror invented.
func mirroredResourceLabels(m schema.Metrics, resourceLabels, dst map[string]string) {
	for k, v := range resourceLabels {
		if promql.DedicatedResourceLabelExcluded(m, k) {
			continue
		}
		dst[k] = v
	}
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
		mirroredResourceLabels(m, resourceLabels, attrs)
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

// otelBucketsToPromSpans re-encodes one OTLP dense bucket array as the
// Prometheus span + delta form, mirroring the upstream OTLP translator
// (prometheusremotewrite.convertBucketsLayout). A dense OTLP array is
// contiguous by construction, so it becomes exactly one span; the counts are
// delta-encoded against the running previous count, which is what
// `positive_deltas` / `negative_deltas` mean on the wire.
//
// Both wire fields are narrower than the column they carry — a span length is
// uint32 and a delta is int64 against a uint64 count — so each conversion is
// range-checked. A fixture that overflowed either would silently mirror a
// different distribution than ClickHouse holds, which is exactly the
// empty-vs-empty class of hollow comparison this seeder exists to prevent.
func otelBucketsToPromSpans(offset int32, counts []uint64) ([]prompb.BucketSpan, []int64, error) {
	if len(counts) == 0 {
		return nil, nil, nil
	}
	n := uint64(len(counts))
	if n > math.MaxUint32 {
		return nil, nil, fmt.Errorf("bucket array of %d entries exceeds a span length", n)
	}
	spans := []prompb.BucketSpan{{
		Offset: offset + promBucketIndexShift,
		Length: uint32(n),
	}}
	deltas := make([]int64, len(counts))
	var prev int64
	for i, c := range counts {
		if c > math.MaxInt64 {
			return nil, nil, fmt.Errorf("bucket count %d exceeds a delta-encoded bucket", c)
		}
		cur := int64(c)
		deltas[i] = cur - prev
		prev = cur
	}
	return spans, deltas, nil
}

// readExpHistogramFixtureSeries mirrors one exponential-histogram family. Each
// CH row becomes one native-histogram sample on the series its attributes
// identify — no fan-out, because the Prometheus wire form carries the whole
// distribution in the sample rather than across `_bucket` companions.
func readExpHistogramFixtureSeries(
	ctx context.Context, conn driver.Conn, src expHistogramFixtureSource,
) ([]prompb.TimeSeries, error) {
	m := schema.DefaultOTelMetrics()
	rows, err := conn.Query(ctx, fmt.Sprintf(expHistogramFixtureSelect, src.table), src.name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	acc := newPromSeriesSet()
	for rows.Next() {
		var attrs, resourceLabels map[string]string
		var tsMS int64
		var count, zeroCount uint64
		var sum float64
		var scale, positiveOffset, negativeOffset int32
		var positiveCounts, negativeCounts []uint64
		if err := rows.Scan(
			&attrs, &resourceLabels, &tsMS, &count, &sum, &scale, &zeroCount,
			&positiveOffset, &positiveCounts, &negativeOffset, &negativeCounts,
		); err != nil {
			return nil, err
		}
		// Same resource-layer fold as the classic mirror, for the same
		// reason: cerberus projects resource labels into the wire label set,
		// so the reference series must carry them too.
		mirroredResourceLabels(m, resourceLabels, attrs)
		positiveSpans, positiveDeltas, err := otelBucketsToPromSpans(positiveOffset, positiveCounts)
		if err != nil {
			return nil, fmt.Errorf("%s positive buckets: %w", src.name, err)
		}
		negativeSpans, negativeDeltas, err := otelBucketsToPromSpans(negativeOffset, negativeCounts)
		if err != nil {
			return nil, fmt.Errorf("%s negative buckets: %w", src.name, err)
		}
		acc.addHistogram(src.name, attrs, prompb.Histogram{
			// The integer arms of the two oneofs: OTLP counts are integral,
			// and the float arms would make Prometheus treat the sample as
			// an already-aggregated float histogram.
			Count:          &prompb.Histogram_CountInt{CountInt: count},
			ZeroCount:      &prompb.Histogram_ZeroCountInt{ZeroCountInt: zeroCount},
			Sum:            sum,
			Schema:         scale,
			ZeroThreshold:  promZeroThreshold,
			PositiveSpans:  positiveSpans,
			PositiveDeltas: positiveDeltas,
			NegativeSpans:  negativeSpans,
			NegativeDeltas: negativeDeltas,
			ResetHint:      prompb.Histogram_UNKNOWN,
			Timestamp:      tsMS,
		})
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

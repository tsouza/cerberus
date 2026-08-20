// Package smoke holds the real-ClickHouse perf-smoke sentinel fixtures: the
// seed builders (this file), the sentinel query definitions
// (sentinels.go), and the integration harness that drives them
// (realch_perfsmoke_integration_test.go, //go:build integration).
//
// # Why this package exists
//
// PR #2358 disabled ClickHouse's query analyzer for native-histogram plan
// shapes to fix a real timeout (#2355), validated only by a manual 8-run
// timing comparison on CI-fixture-scale data. That silently removed a CSE
// fold the SQL emitter relied on; 6h8m later, prod hit MEMORY_LIMIT_EXCEEDED
// (#2364) because two array-scan quantities were no longer folded into
// single scans. The failure mode (memory, not time) was invisible at CI
// scale and only appears at real cardinality on a real ClickHouse server —
// nothing else in CI runs real CH with real memory measurement. This
// package seeds exactly the shapes that broke, at the scale that broke them.
package smoke

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// --- Sentinel 1 seed: native-histogram-at-scale --------------------------

// nativeHistogramScrapeInterval is the sample cadence the seed lays down per
// pod series. Calibrated, not guessed: the merge/window-fold machinery
// applyNativeHistogramAnalyzerFix targets scales STEEPLY (worse than
// quadratic) in per-series sample count within the query range, not
// linearly — sweeping against a real testcontainers ClickHouse 25.8-alpine
// at NativeHistogramSeriesCount=500 over the 1h/15s grid found peak memory
// climbing from 205 MiB at a 300s cadence (14 samples/series) to 1.56 GiB at
// 270s (17 samples/series) to 6+ GiB at 210s (20 samples/series) — a cliff,
// not a slope. 300s is the calibrated cadence: comfortably below the cliff
// (real margin below the 270s failure point) while still dense enough that
// every anchor's 5m rate() window sees real, non-degenerate sample data.
const nativeHistogramScrapeInterval = 300 * time.Second

// nativeHistogramRateRange mirrors Sentinel 1's `[5m]` range selector: the
// seed must extend at least this far before the query window's start so
// every anchor's rate() window has a preceding sample to diff against.
const nativeHistogramRateRange = 5 * time.Minute

// nativeHistogramNominalAvgNanos is the nominal per-observation latency (in
// the histogram's implied nanosecond unit) baked into Sum, so Sum/Count is a
// plausible positive average rather than an arbitrary placeholder.
const nativeHistogramNominalAvgNanos = 12_500_000 // 12.5ms

// nativeHistogramScale is the exponential-histogram base-2 Scale stamped on
// every seeded bucket set. 3 is a realistic OTel SDK default-range value
// (higher resolution than Scale=0), not load-bearing for the test itself.
const nativeHistogramScale = 3

// nativeHistogramBucketShape returns a realistic (non-trivial), symmetric
// bell-shaped positive-bucket layout — deliberately NOT the near-empty
// 3-bucket arrays test/spec's chDB fixtures use (e.g.
// histogram_quantile_native_agg_groupby.txtar's [1,2,3]) — so the seed
// exercises the merge/window-fold machinery applyNativeHistogramAnalyzerFix
// targets (query_settings_rules.go's promHistogramKahanSum /
// arrayFold/arrayMap trees) over a genuine bucket count, not a degenerate
// one that under-represents the shape the analyzer cost was actually
// measured on.
func nativeHistogramBucketShape() []uint64 {
	return []uint64{
		1, 2, 4, 8, 16, 32, 64, 128, 256, 512,
		384, 256, 128, 64, 32, 16, 8, 4, 2, 1,
		1, 1, 1, 1,
	}
}

// SeedNativeHistogramAtScale seeds Sentinel 1's fixture directly into the
// real otel_metrics_exponential_histogram table (created by ddl.Apply against
// ddl.Metrics — this function only INSERTs). It lays down seriesCount
// distinct `pod` series of metric, each carrying
// nativeHistogramBucketShape's realistic bucket layout, sampled every
// nativeHistogramScrapeInterval from (start - nativeHistogramRateRange)
// through end. Bucket counts (and Count/Sum) grow monotonically per series
// across time (cumulative-counter semantics, matching
// AggregationTemporality=2/CUMULATIVE), so rate() sees a genuine positive
// delta at every query-grid anchor rather than a flat zero-rate series.
//
// Returns the row count seeded — the primary sentinel's leaf scan-row count.
func SeedNativeHistogramAtScale(ctx context.Context, conn driver.Conn, metric string, seriesCount int, start, end time.Time) (rows int64, err error) {
	if seriesCount <= 0 {
		return 0, fmt.Errorf("seed native histogram: seriesCount must be positive, got %d", seriesCount)
	}
	seedStart := start.Add(-nativeHistogramRateRange)
	samplesPerSeries := int(end.Sub(seedStart)/nativeHistogramScrapeInterval) + 1
	if samplesPerSeries <= 0 {
		return 0, fmt.Errorf("seed native histogram: non-positive samplesPerSeries (start=%v end=%v)", start, end)
	}
	totalSamples := samplesPerSeries * seriesCount

	shape := nativeHistogramBucketShape()
	baseCount := sumUint64(shape)
	shapeLit := chUint64ArrayLiteral(shape)

	insert := fmt.Sprintf(
		`
INSERT INTO otel_metrics_exponential_histogram
    (MetricName, Attributes, ServiceName, TimeUnix, Scale, ZeroCount,
     PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts,
     Count, Sum, AggregationTemporality)
SELECT
    '%s' AS MetricName,
    map('pod', concat('pod-', toString(number %% %d))) AS Attributes,
    'cerberus-smoke' AS ServiceName,
    toDateTime64(%d, 9) + toIntervalSecond(intDiv(number, %d) * %d) AS TimeUnix,
    %d AS Scale,
    0 AS ZeroCount,
    0 AS PositiveOffset,
    arrayMap(x -> x * (1 + intDiv(number, %d)), %s) AS PositiveBucketCounts,
    0 AS NegativeOffset,
    emptyArrayUInt64() AS NegativeBucketCounts,
    toUInt64(%d * (1 + intDiv(number, %d))) AS Count,
    toFloat64(%d * (1 + intDiv(number, %d)) * %d) AS Sum,
    2 AS AggregationTemporality
FROM numbers(%d)`,
		metric, seriesCount,
		seedStart.Unix(), seriesCount, int(nativeHistogramScrapeInterval/time.Second),
		nativeHistogramScale,
		seriesCount, shapeLit,
		baseCount, seriesCount,
		baseCount, seriesCount, nativeHistogramNominalAvgNanos,
		totalSamples,
	)
	if err := conn.Exec(ctx, insert); err != nil {
		return 0, fmt.Errorf("seed native histogram: %w\n%s", err, insert)
	}
	return int64(totalSamples), nil
}

// --- Sentinel 2 seed: high-cardinality counter ----------------------------

// wideCounterScrapeInterval / wideCounterRateRange mirror the native-
// histogram seed's cadence choice. Calibration showed rate()'s per-anchor
// arrayJoin fan-out (not just GROUP BY cardinality) drives peak memory hard:
// at a dense 60s cadence even 50,000 series overflowed the 1 GiB cap outright
// (code 241), well before cardinality alone would predict. The same 300s
// cadence Sentinel 1 settled on keeps the per-series row count — and so the
// fan-out — small enough that spillSeriesCount's calibrated cardinality (see
// realch_perfsmoke_integration_test.go) is what determines whether the query
// crosses spillThreshold, not incidental row-level fan-out.
const wideCounterScrapeInterval = 300 * time.Second

const wideCounterRateRange = 5 * time.Minute

// wideCounterValueStepPerSample is the per-scrape counter increment baked
// into Value, so rate() sees a constant, genuinely positive per-series rate
// rather than a flat series.
const wideCounterValueStepPerSample = 10

// SeedHighCardinalityCounter seeds Sentinel 2's fixture directly into the
// real otel_metrics_sum table (created by ddl.Apply against ddl.Metrics). It
// lays down seriesCount distinct `session_id` series of metric, each a
// monotonically increasing counter sampled every wideCounterScrapeInterval
// from (start - wideCounterRateRange) through end.
//
// seriesCount is NOT a fixed constant — the caller passes the calibrated
// cardinality (realch_perfsmoke_integration_test.go's spillSeriesCount) at
// which the `sum by (session_id) (rate(...))` GROUP BY's in-memory
// hash-table state reliably crosses spillThreshold(cap) (spill.go), so
// applySpillSettings' spill-to-disk path is what actually keeps the query
// under the memory cap rather than the cardinality being incidentally too
// low to matter.
//
// Returns the row count seeded.
func SeedHighCardinalityCounter(ctx context.Context, conn driver.Conn, metric string, seriesCount int, start, end time.Time) (rows int64, err error) {
	if seriesCount <= 0 {
		return 0, fmt.Errorf("seed high-cardinality counter: seriesCount must be positive, got %d", seriesCount)
	}
	seedStart := start.Add(-wideCounterRateRange)
	samplesPerSeries := int(end.Sub(seedStart)/wideCounterScrapeInterval) + 1
	if samplesPerSeries <= 0 {
		return 0, fmt.Errorf("seed high-cardinality counter: non-positive samplesPerSeries (start=%v end=%v)", start, end)
	}
	totalSamples := samplesPerSeries * seriesCount

	insert := fmt.Sprintf(
		`
INSERT INTO otel_metrics_sum
    (MetricName, Attributes, ServiceName, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT
    '%s' AS MetricName,
    map('session_id', concat('sess-', toString(number %% %d))) AS Attributes,
    'cerberus-smoke' AS ServiceName,
    toDateTime64(%d, 9) + toIntervalSecond(intDiv(number, %d) * %d) AS TimeUnix,
    toFloat64(intDiv(number, %d) * %d) AS Value,
    2 AS AggregationTemporality,
    true AS IsMonotonic
FROM numbers(%d)`,
		metric, seriesCount,
		seedStart.Unix(), seriesCount, int(wideCounterScrapeInterval/time.Second),
		seriesCount, wideCounterValueStepPerSample,
		totalSamples,
	)
	if err := conn.Exec(ctx, insert); err != nil {
		return 0, fmt.Errorf("seed high-cardinality counter: %w\n%s", err, insert)
	}
	return int64(totalSamples), nil
}

// --- Sentinel 3 seed: wide-attribute traces -------------------------------

// wideTraceAttrKeysPerMap is the number of distinct attribute keys seeded
// into EACH of ResourceAttributes / SpanAttributes per span — "wide" as in
// the applyCompareMemoryBound doc comment's "wide ResourceAttributes /
// SpanAttributes Map columns", not the handful of keys
// metrics_compare.txtar's tiny fixture carries.
const wideTraceAttrKeysPerMap = 24

// wideTraceAttrValueModulus bounds the distinct attribute VALUES a key can
// take (value = (trace_index [+ key_index [+ span_index]]) %
// wideTraceAttrValueModulus), so compare()'s arrayZip/arrayJoin over
// (attr, val) pairs sees genuine value diversity instead of collapsing to a
// handful of distinct values GROUP BY (is_selection, attr, val) would fold
// away for free.
const wideTraceAttrValueModulus = 997

// wideTraceErrorFraction is the denominator of the fraction of root spans
// seeded with StatusCode='Error' (one in wideTraceErrorFraction), so
// compare({status=error})'s selection predicate has a genuine, non-trivial
// (non-0%, non-100%) subpopulation to contrast against the rest.
const wideTraceErrorFraction = 5

// SeedWideAttributeTraces seeds Sentinel 3's fixture directly into the real
// otel_traces table (created by ddl.Apply against ddl.Traces). It lays down
// traceCount two-span trees (a Server root + a Client child) spread evenly
// across [start, end), each span carrying wideTraceAttrKeysPerMap distinct
// keys in BOTH ResourceAttributes and SpanAttributes with values drawn from
// a wideTraceAttrValueModulus-sized space, so compare()'s per-attribute
// arrayJoin/GROUP BY has genuine width and cardinality to aggregate over.
// One root span in wideTraceErrorFraction carries StatusCode='Error', giving
// compare({status=error}) a real, non-degenerate selection split.
//
// traceCount is NOT a fixed constant — the caller passes the calibrated
// cardinality (realch_perfsmoke_integration_test.go's wideTraceCount) at
// which the compare() GROUP BY's attribute-distribution state is large
// enough that applyCompareMemoryBound's spill + thread-cap settings are what
// keep the query under the memory cap, not incidental smallness.
//
// Returns the row count seeded (2 * traceCount spans).
func SeedWideAttributeTraces(ctx context.Context, conn driver.Conn, traceCount int, start, end time.Time) (rows int64, err error) {
	if traceCount <= 0 {
		return 0, fmt.Errorf("seed wide-attribute traces: traceCount must be positive, got %d", traceCount)
	}
	span := end.Sub(start)
	if span <= 0 {
		return 0, fmt.Errorf("seed wide-attribute traces: end must be after start (start=%v end=%v)", start, end)
	}
	stepNanos := int64(span) / int64(traceCount)
	if stepNanos <= 0 {
		stepNanos = 1
	}

	insert := fmt.Sprintf(
		`
INSERT INTO otel_traces
    (Timestamp, TraceId, SpanId, ParentSpanId, SpanName, SpanKind, ServiceName,
     ResourceAttributes, SpanAttributes, Duration, StatusCode, StatusMessage,
     ScopeName, ScopeVersion, TraceState)
SELECT
    base_ts + toIntervalNanosecond(sp) AS Timestamp,
    traceid AS TraceId,
    concat(traceid, '_', toString(sp)) AS SpanId,
    if(sp = 0, '', concat(traceid, '_0')) AS ParentSpanId,
    if(sp = 0, 'GET /checkout', 'call-downstream') AS SpanName,
    if(sp = 0, 'Server', 'Client') AS SpanKind,
    'cerberus-smoke' AS ServiceName,
    mapFromArrays(
        arrayMap(i -> concat('res.k', toString(i)), range(%d)),
        arrayMap(i -> concat('v', toString((tr + i) %% %d)), range(%d))
    ) AS ResourceAttributes,
    mapFromArrays(
        arrayMap(i -> concat('span.k', toString(i)), range(%d)),
        arrayMap(i -> concat('v', toString((tr + i + sp) %% %d)), range(%d))
    ) AS SpanAttributes,
    1000000 AS Duration,
    if(sp = 0 AND (tr %% %d) = 0, 'Error', 'Unset') AS StatusCode,
    '' AS StatusMessage, '' AS ScopeName, '' AS ScopeVersion, '' AS TraceState
FROM (
    SELECT
        number AS tr,
        concat('t', leftPad(toString(number), 10, '0')) AS traceid,
        toDateTime64(%d, 9) + toIntervalNanosecond(number * %d) AS base_ts
    FROM numbers(%d)
) AS t
ARRAY JOIN [0, 1] AS sp`,
		wideTraceAttrKeysPerMap, wideTraceAttrValueModulus, wideTraceAttrKeysPerMap,
		wideTraceAttrKeysPerMap, wideTraceAttrValueModulus, wideTraceAttrKeysPerMap,
		wideTraceErrorFraction,
		start.Unix(), stepNanos, traceCount,
	)
	if err := conn.Exec(ctx, insert); err != nil {
		return 0, fmt.Errorf("seed wide-attribute traces: %w\n%s", err, insert)
	}
	return int64(traceCount) * 2, nil
}

// --- small local helpers (kept out of internal/chsql: these build
//     test-fixture SQL, never production plan SQL — see the "bare SQL is
//     acceptable here" convention scale_wall_pin_chdb_test.go's yardstickSQL
//     and metrics_compare.txtar document) --------------------------------

func sumUint64(vs []uint64) uint64 {
	var total uint64
	for _, v := range vs {
		total += v
	}
	return total
}

func chUint64ArrayLiteral(vs []uint64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vs {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", v)
	}
	b.WriteByte(']')
	return b.String()
}

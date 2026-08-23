// loader.go loads the real-production-shaped parquet samples
// (testdata/samples/*.parquet, trimmed from test/perf/smoke/testdata/samples/'s
// full 14-day set — see testdata/samples/README.md) into a real ClickHouse
// server for the nightly measurement harness (#2370).
//
// ClickHouse's own Parquet reader represents each sample's Attributes /
// ResourceAttributes columns as a named Tuple (the shape the extraction
// pipeline scrubbed and flattened them into), but the real
// otel_metrics_{histogram,sum,gauge} DDL (the upstream OTel Collector
// ClickHouse exporter's own templates, see internal/schema/ddl) declares
// both as Map(LowCardinality(String), String) — so loading is not a bare
// `INSERT ... SELECT * FROM file(...)`; each named tuple field is read back
// via dot access (backtick-quoted for the dotted ResourceAttributes keys)
// and re-packed with mapFromArrays. This file is test-only data-loading
// code, not the chsql emitter, so invariant 10 (typed Frags only) does not
// apply here — the same carve-out scale_wall_pin_chdb_test.go's own seeding
// already uses.
package nightly

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/testcontainers/testcontainers-go"

	"github.com/tsouza/cerberus/internal/chclient"
)

// sampleAttributeKeys names the Attributes struct fields the extraction
// pipeline flattened for a given metric, in the order they must be listed
// against their mapFromArrays value list. Kept per-metric rather than
// shared: svc_http_requests_total's Attributes carries one extra field
// (status_class) svc_http_request_duration_seconds does not.
var sampleAttributeKeys = map[string][]string{
	histogramMetric: {"app_id", "http_route", "k8s_namespace_name", "k8s_pod_name", "method", "service"},
	sumMetric:       {"app_id", "http_route", "k8s_namespace_name", "k8s_pod_name", "method", "service", "status_class"},
	gaugeMetric:     {"k8s_namespace_name", "k8s_pod_name", "namespace", "pod", "reason", "uid"},
}

// sampleResourceAttributeKeys names the ResourceAttributes struct fields
// every sample metric carries identically (the extraction pipeline flattens
// the same resource-level key set regardless of metric type).
var sampleResourceAttributeKeys = []string{
	"container.image.name", "container.image.tag", "deployment.environment", "deployment.environment.name",
	"host.name", "k8s.cluster.name", "k8s.cluster.uid", "k8s.container.name", "k8s.deployment.name",
	"k8s.namespace.name", "k8s.node.name", "k8s.pod.name", "k8s.pod.start_time", "k8s.pod.uid",
	"k8s.replicaset.name", "k8s.replicaset.uid", "server.address", "server.port", "service.instance.id",
	"service.name", "service.namespace", "service.version", "url.scheme",
}

// clickhouseUserFilesDir is where every ClickHouse server image restricts
// the `file()` table function to reading from (DATABASE_ACCESS_DENIED
// outside it) — the container image's own baked-in default, not a cerberus
// configuration choice.
const clickhouseUserFilesDir = "/var/lib/clickhouse/user_files"

// mapArraysExpr renders `mapFromArrays([...keys...], [col.key1, col.key2, ...])`
// for a struct column's named fields, backtick-quoting each key for the dot
// access (required for ResourceAttributes' dotted field names, harmless for
// Attributes' plain ones).
func mapArraysExpr(column string, keys []string) string {
	keyList, valueList := "", ""
	for i, k := range keys {
		if i > 0 {
			keyList += ", "
			valueList += ", "
		}
		keyList += fmt.Sprintf("'%s'", k)
		valueList += fmt.Sprintf("%s.`%s`", column, k)
	}
	return fmt.Sprintf("mapFromArrays([%s], [%s])", keyList, valueList)
}

// loadHistogramSample copies hostParquetPath into the container's
// user_files directory and inserts its rows into otel_metrics_histogram.
func loadHistogramSample(ctx context.Context, container testcontainers.Container, client *chclient.Client, hostParquetPath string) (uint64, error) {
	return loadSample(ctx, container, client, hostParquetPath, "otel_metrics_histogram",
		"ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, ResourceAttributes, "+
			"StartTimeUnix, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds, Min, Max, AggregationTemporality",
		"ServiceName, MetricName, MetricDescription, MetricUnit, "+mapArraysExpr("Attributes", sampleAttributeKeys[histogramMetric])+", "+
			mapArraysExpr("ResourceAttributes", sampleResourceAttributeKeys)+", "+
			"toDateTime64(StartTimeUnix, 9), toDateTime64(TimeUnix, 9), Count, Sum, BucketCounts, ExplicitBounds, Min, Max, AggregationTemporality")
}

// loadSumSample loads the counter (Sum, IsMonotonic=true) sample into
// otel_metrics_sum.
func loadSumSample(ctx context.Context, container testcontainers.Container, client *chclient.Client, hostParquetPath string) (uint64, error) {
	return loadSample(ctx, container, client, hostParquetPath, "otel_metrics_sum",
		"ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, ResourceAttributes, "+
			"StartTimeUnix, TimeUnix, Value, Flags, AggregationTemporality, IsMonotonic",
		"ServiceName, MetricName, MetricDescription, MetricUnit, "+mapArraysExpr("Attributes", sampleAttributeKeys[sumMetric])+", "+
			mapArraysExpr("ResourceAttributes", sampleResourceAttributeKeys)+", "+
			"toDateTime64(StartTimeUnix, 9), toDateTime64(TimeUnix, 9), toFloat64(Value), toUInt32(Flags), toInt32(AggregationTemporality), IsMonotonic")
}

// loadGaugeSample loads the gauge sample into otel_metrics_gauge.
func loadGaugeSample(ctx context.Context, container testcontainers.Container, client *chclient.Client, hostParquetPath string) (uint64, error) {
	return loadSample(ctx, container, client, hostParquetPath, "otel_metrics_gauge",
		"ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, ResourceAttributes, StartTimeUnix, TimeUnix, Value, Flags",
		"ServiceName, MetricName, MetricDescription, MetricUnit, "+mapArraysExpr("Attributes", sampleAttributeKeys[gaugeMetric])+", "+
			mapArraysExpr("ResourceAttributes", sampleResourceAttributeKeys)+", "+
			"toDateTime64(StartTimeUnix, 9), toDateTime64(TimeUnix, 9), toFloat64(Value), toUInt32(Flags)")
}

// --- Native (exponential) histogram derivation ----------------------------

// nativeHistogramMetric names the derived exponential-histogram metric this
// loader manufactures from the REAL classic-histogram sample already loaded
// into otel_metrics_histogram (loadHistogramSample) — issue #2370's nightly
// sample has no captured native-histogram production data at all (the
// production system this was scrubbed from only emits classic-bucket
// histograms), so this is a DERIVED sentinel rather than a captured or a
// from-scratch-synthetic one — see loadNativeHistogramFromClassicSample's
// own doc comment. The "_native_exp_hist" suffix is load-bearing: the
// trailing "_exp_hist" is what schema.Metrics.RouteHistogramToNative (the
// default ExpHistogramSuffix, "_exp_hist" — see internal/schema/otel.go)
// uses to route a histogram_quantile(...) call to
// otel_metrics_exponential_histogram instead of the classic table; "_native"
// keeps the name visually distinct from a hypothetical real
// "..._exp_hist" metric so nothing could mistake it for captured data.
const nativeHistogramMetric = histogramMetric + "_native_exp_hist"

// nativeHistogramDerivedScale is the exponential-histogram base-2 Scale the
// derivation re-buckets the real classic sample into. 3 mirrors
// test/perf/smoke/seed.go's own nativeHistogramScale (a realistic OTel SDK
// default-range value, higher resolution than Scale=0) — reused rather than
// independently chosen, since the point of this sentinel is to stress the
// same native-quantile machinery that scale already exercises at CI-fixture
// scale, now at real production row/series scale.
const nativeHistogramDerivedScale = 3

// nativeHistogramBucketsPerOctave is 2^nativeHistogramDerivedScale, the
// number of native exponential buckets spanning one doubling of value.
// internal/chsql/histogram_quantile_native.go's reader defines
// base = pow(2, pow(2, -Scale)), so a value's native bucket index is
// ceil(log_base(value)) = ceil(log2(value) * 2^Scale) — this constant is
// that literal multiplier, applied by the derivation SQL below in the
// SAME direction the reader inverts.
const nativeHistogramBucketsPerOctave = 1 << nativeHistogramDerivedScale

// nativeHistogramOverflowBoundMultiplier is the factor the derivation
// multiplies a classic histogram row's highest finite ExplicitBounds entry
// by to get a representative value for the classic ladder's trailing
// `+Inf` overflow bucket (BucketCounts always carries one more element than
// ExplicitBounds — the trailing slot has no upper edge to derive a
// midpoint from). There is no principled "correct" placement for an
// unbounded bucket; one octave above the last finite bound is a
// defensible, order-of-magnitude-honest placement that keeps the derived
// layout non-degenerate without inventing a synthetic maximum observation.
const nativeHistogramOverflowBoundMultiplier = 2

// loadNativeHistogramFromClassicSample derives nativeHistogramMetric's
// exponential-histogram rows from the REAL classic-histogram sample already
// loaded into otel_metrics_histogram by loadHistogramSample: same real
// per-series cardinality, same real per-series sample cadence, and the same
// real per-row Count/Sum, just re-expressed as an OTel exponential-histogram
// bucket layout instead of a classic (bounds/bucket) one.
//
// # The conversion
//
// otel_metrics_histogram.BucketCounts is the raw, NON-cumulative per-bucket
// count OTel stores per row (see classicBucketRowCumulativeExpr's own
// comment in internal/promql/histogram_quantile.go for why cerberus itself
// computes a cumulative ladder from it rather than reading one off the
// wire): row BucketCounts[i] holds the count of observations in
// (ExplicitBounds[i-1], ExplicitBounds[i]] (or, for the trailing overflow
// slot, in (ExplicitBounds[last], +Inf)). The derivation runs entirely PER
// SOURCE ROW — no cross-row aggregation, since a classic sample row already
// IS one series at one TimeUnix, exactly the granularity the derived
// exp-histogram row needs too — and for each row:
//
//  1. Computes each classic bucket's representative value: the geometric
//     mean of its (lower, upper) edges (the natural placement on the
//     log-scale axis the exponential layout itself uses), or upper/2 for
//     the first (lower=0) bucket, or ExplicitBounds[last] *
//     nativeHistogramOverflowBoundMultiplier for the trailing overflow
//     bucket.
//  2. Maps that value to a native bucket index via the inverse of
//     internal/chsql/histogram_quantile_native.go's own reader formula
//     (see nativeHistogramBucketsPerOctave).
//  3. Builds the (offset, counts) array pair the OTel exponential wire
//     format requires (PositiveOffset / PositiveBucketCounts —
//     internal/schema/otel.go) over the FULL classic-ladder's mapped
//     native-index range — not just the positions a given row actually
//     populated. Every row in a fixed metric shares the identical
//     ExplicitBounds layout (verified against the real sample: a single
//     distinct ExplicitBounds array across all 1.3M rows), so this range is
//     the SAME min/max for every row — every derived row therefore gets the
//     identical PositiveOffset and array length, exactly the stable,
//     unchanging-resolution layout a real OTel SDK exporter would emit for
//     one metric definition. This is a deliberate, load-bearing choice, not
//     a simplification: an earlier version of this derivation shrank each
//     row's array down to just its own populated span, which gave
//     thousands of real, sparsely-populated rows (many carry Count=1, one
//     single classic bucket) WILDLY different per-row offsets — and
//     merging many such mutually-misaligned layouts is exactly what
//     internal/chsql/histogram_quantile_native.go's cross-snapshot merge
//     has to re-align, which measured as a genuine ClickHouse
//     MEMORY_LIMIT_EXCEEDED abort even at a SINGLE query anchor and NO
//     rate() window at this sample's real 3,741-series cardinality. Fixing
//     the offset keeps every merge step's "common index space" trivially
//     already-aligned, which is what real OTel exporter data looks like in
//     the first place.
//
// Placing each classic bucket's full count at one representative point,
// rather than uniformly fanning it across the bucket's range, is the
// simpler of the two approximations this package's own doc comment allows.
// Checked directly against the real sample data (a scratch ClickHouse
// container loaded with the real parquet, queried by hand — see the PR that
// introduced this function for the transcript): the real classic sample's
// 12-bucket-per-row layout (0.005s..10s+overflow) derives to a fixed
// ~101-element native array per row, and every observation is preserved
// exactly (sum(PositiveBucketCounts) across the whole derived table equals
// sum(Count) across the whole classic table bit for bit) — a real,
// non-degenerate multi-bucket native histogram once aggregated, not a
// single-bucket, trivially-cheap one. See classic_native_histogram_derived's
// own comment in sentinels.go for the query-level verification.
//
// Returns the row count actually inserted (read back via a plain count(),
// mirroring loadSample's own convention — this driver's Exec does not
// report affected rows).
func loadNativeHistogramFromClassicSample(ctx context.Context, client *chclient.Client) (uint64, error) {
	conn := client.Conn()
	if err := conn.Exec(ctx, nativeHistogramDerivationSQL()); err != nil {
		return 0, fmt.Errorf("derive native histogram from classic sample: %w", err)
	}

	row := conn.QueryRow(ctx, fmt.Sprintf("SELECT count() FROM otel_metrics_exponential_histogram WHERE MetricName = '%s'", nativeHistogramMetric))
	var n uint64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count derived native histogram rows: %w", err)
	}
	return n, nil
}

// nativeHistogramDerivationSQL renders loadNativeHistogramFromClassicSample's
// INSERT ... SELECT (split out from that function so its exact text is
// independently testable without a live ClickHouse connection — see
// loader_test.go). Step-by-step, the innermost subquery's aliases (which
// ClickHouse resolves left-to-right within one SELECT list, each free to
// reference an earlier one) are:
//
//	lowerBounds / upperBounds — the (lower, upper] edge pair for every
//	  classic bucket, including the synthesised overflow edge (see
//	  nativeHistogramOverflowBoundMultiplier).
//	reps       — each bucket's representative value (geometric mean of its
//	  edges, or upper/2 for the zero-anchored first bucket).
//	nativeIdx  — reps mapped to native exponential bucket indices. Since
//	  ExplicitBounds (and so lowerBounds/upperBounds/reps/nativeIdx) is
//	  identical for every row of a fixed metric, minIdx/maxIdx below are
//	  the SAME constant for every row — see loadNativeHistogramFromClassicSample's
//	  own comment for why that fixed layout is load-bearing, not incidental.
//	minIdx/maxIdx — nativeIdx's own range, and PositiveOffset is minIdx.
//	PositiveBucketCounts — the dense array over [minIdx, maxIdx], each
//	  position summing every classic bucket whose nativeIdx landed on it
//	  (handles the rare case where two classic buckets map to the same
//	  native index; zero where none did).
//
// The outer SELECT renames the derived row into nativeHistogramMetric and
// zeroes the fields a duration metric's real classic sample never
// populates (ZeroCount, NegativeOffset/NegativeBucketCounts — no observed
// duration is ever <= 0).
func nativeHistogramDerivationSQL() string {
	return fmt.Sprintf(`
INSERT INTO otel_metrics_exponential_histogram
    (ServiceName, MetricName, MetricDescription, MetricUnit, Attributes, ResourceAttributes,
     StartTimeUnix, TimeUnix, Count, Sum, Scale, ZeroCount,
     PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts,
     AggregationTemporality)
SELECT
    ServiceName, '%s' AS MetricName, MetricDescription, MetricUnit,
    Attributes, ResourceAttributes, StartTimeUnix, TimeUnix, Count, Sum,
    %d AS Scale, toUInt64(0) AS ZeroCount,
    PositiveOffset, PositiveBucketCounts, toInt32(0) AS NegativeOffset, emptyArrayUInt64() AS NegativeBucketCounts,
    AggregationTemporality
FROM
(
    SELECT
        ServiceName, MetricDescription, MetricUnit, Attributes, ResourceAttributes,
        StartTimeUnix, TimeUnix, Count, Sum, AggregationTemporality,
        arrayPushFront(ExplicitBounds, toFloat64(0)) AS lowerBounds,
        arrayPushBack(ExplicitBounds, ExplicitBounds[length(ExplicitBounds)] * %d) AS upperBounds,
        arrayMap((lo, hi) -> if(lo > 0, sqrt(lo * hi), hi / 2), lowerBounds, upperBounds) AS reps,
        arrayMap(r -> toInt32(ceil(log2(r) * %d)), reps) AS nativeIdx,
        arrayMin(nativeIdx) AS minIdx,
        arrayMax(nativeIdx) AS maxIdx,
        minIdx AS PositiveOffset,
        arrayMap(i -> arraySum(arrayFilter((c, idx) -> idx = (minIdx + i), BucketCounts, nativeIdx)), range(toUInt32(maxIdx - minIdx + 1))) AS PositiveBucketCounts
    FROM otel_metrics_histogram
    WHERE MetricName = '%s'
)`,
		nativeHistogramMetric, nativeHistogramDerivedScale,
		nativeHistogramOverflowBoundMultiplier, nativeHistogramBucketsPerOctave,
		histogramMetric)
}

// loadSample copies hostParquetPath into the container, then runs
// `INSERT INTO table (columns) SELECT selectList FROM file(name, Parquet)`,
// returning the row count actually inserted (read back via a plain
// count(), since ClickHouse's Exec doesn't report affected rows).
func loadSample(ctx context.Context, container testcontainers.Container, client *chclient.Client, hostParquetPath, table, columns, selectList string) (uint64, error) {
	name := filepath.Base(hostParquetPath)
	containerPath := clickhouseUserFilesDir + "/" + name
	if err := container.CopyFileToContainer(ctx, hostParquetPath, containerPath, 0o644); err != nil {
		return 0, fmt.Errorf("copy %s into container: %w", hostParquetPath, err)
	}

	conn := client.Conn()
	insertSQL := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM file('%s', Parquet)", table, columns, selectList, name)
	if err := conn.Exec(ctx, insertSQL); err != nil {
		return 0, fmt.Errorf("insert into %s from %s: %w", table, name, err)
	}

	row := conn.QueryRow(ctx, fmt.Sprintf("SELECT count() FROM %s", table))
	var n uint64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return n, nil
}

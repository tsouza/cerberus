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

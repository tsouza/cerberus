//go:build chdb

package chsql_test

import (
	"database/sql"
	"testing"
)

// This file exists only on the release/1.15.x backport line. On main
// hqNativeSeedDDL and seed are defined in
// histogram_quantile_classic_native_chdb_test.go — but the commit that adds
// that file's own feature (the ClickHouse-native classic-histogram ladder,
// #2401, a perf: commit) was not selected for this backport batch, while a
// later fix: commit that WAS backported
// (histogram_quantile_fanout_nonfinite_bound_chdb_test.go, #2502) reuses
// both as shared test fixtures. Copied verbatim rather than backporting the
// whole feature, since both are pure test infrastructure with no behavioral
// dependency on the native-ladder code #2401 actually adds.

// hqNativeSeedDDL is the OTel-CH classic-histogram table shape the default
// schema reads: the bucket pair plus the ResourceAttributes / ServiceName
// columns the label projection always references and the
// AggregationTemporality column the rate fold branches on.
const hqNativeSeedDDL = `
CREATE OR REPLACE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64),
    AggregationTemporality Int32 DEFAULT 2
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
`

// seed executes a semicolon-separated DDL/DML script against db.
func seed(t *testing.T, db *sql.DB, script string) {
	t.Helper()
	for _, stmt := range splitSeedStatements(script) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}
}

//go:build chdb

// Shared seed DDL for this package's chDB round-trip tests.

package chsql_test

import "fmt"

// metricsSeedDDL renders the OTel-CH metric-table shape every chDB round-trip
// test in this package seeds. It is ONE definition rather than a copy per test
// file because the read path decides which columns a selector projects, and a
// column added there has to reach every seed at once: `ServiceName` was added
// to the selector's label projection and eight identical inline CREATEs each
// had to learn about it, which is exactly the drift a shared renderer removes.
//
// `ResourceAttributes` and `ServiceName` carry DEFAULTs so the tests' explicit
// INSERT column lists stay short — the read path only needs them to RESOLVE,
// not to be populated.
func metricsSeedDDL(table string) string {
	return fmt.Sprintf(`
CREATE OR REPLACE TABLE %s (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
`, table)
}

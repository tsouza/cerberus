package chsqltest

import (
	"fmt"
	"strings"

	"github.com/tsouza/cerberus/internal/schema"
)

// metricsSeedTimestampKeyFn is the ClickHouse function the OTel-CH metrics
// ORDER BY wraps its timestamp key element in. It is the one part of the
// sorting key internal/schema cannot hand back as a bare column name —
// Metrics.SortingKeyPrefix stops at the first function-wrapped element by
// contract, because a wrapped element can never be a GROUP BY prefix — so it
// is named here, next to the only renderer that needs it, rather than being
// re-derived from DDL text. See internal/schema/sortingkey.go for the tuple
// this completes.
const metricsSeedTimestampKeyFn = "toUnixTimestamp64Nano"

// MetricsSeedDDL renders the OTel-CH metric-table shape internal/chsql's chDB
// round-trip tests seed. It is ONE definition rather than a copy per test file
// because the read path decides which columns a selector projects, and a
// column added there has to reach every seed at once: `ServiceName` was added
// to the selector's label projection and eight identical inline CREATEs each
// had to learn about it, which is exactly the drift a shared renderer removes.
// A ninth seed that never adopted it — a hand-rolled three-column
// `otel_metrics_sum` — is what #2074 was.
//
// `ResourceAttributes` and `ServiceName` carry DEFAULTs so the tests' explicit
// INSERT column lists stay short — the read path only needs them to RESOLVE,
// not to be populated.
//
// The ORDER BY is DERIVED from internal/schema rather than typed here. It was
// typed here, as `(MetricName, Attributes, TimeUnix)`, while every real
// cerberus metrics table is ordered by
// `(MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix))` —
// so every chDB round-trip in internal/chsql exercised PREWHERE promotion and
// sort-prefix ordering against a physical key the emitter is not written for
// (cerberus issue #3186). Deriving it means the seed cannot drift from the
// shape chsql's own TableShape registry reports for these tables, because both
// read the same schema.Metrics column fields.
func MetricsSeedDDL(table string) string {
	m := schema.DefaultOTelMetrics()
	sortKey := append(m.SortingKeyPrefix(), fmt.Sprintf("%s(%s)", metricsSeedTimestampKeyFn, m.TimestampColumn))
	return fmt.Sprintf(`
CREATE OR REPLACE TABLE %s (
    %s String,
    %s Map(String, String),
    %s Map(String, String) DEFAULT map(),
    %s LowCardinality(String) DEFAULT '',
    %s DateTime64(9),
    %s Float64
) ENGINE = MergeTree ORDER BY (%s);
`, table,
		m.MetricNameColumn,
		m.AttributesColumn,
		m.ResourceAttributesColumn,
		m.ServiceNameColumn,
		m.TimestampColumn,
		m.ValueColumn,
		strings.Join(sortKey, ", "))
}

package chsqltest

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestMetricsSeedDDLOrdersLikeARealMetricsTable pins the seed's ORDER BY to
// the sorting key every real cerberus metrics table carries. The seed used to
// declare `(MetricName, Attributes, TimeUnix)` — dropping ServiceName and the
// function-wrapped timestamp key — so every chDB round-trip in internal/chsql
// validated PREWHERE promotion and sort-prefix ordering against a physical key
// the emitter is not written for (cerberus issue #3186).
func TestMetricsSeedDDLOrdersLikeARealMetricsTable(t *testing.T) {
	t.Parallel()

	m := schema.DefaultOTelMetrics()
	ddl := MetricsSeedDDL(m.SumTable)

	open := strings.LastIndex(ddl, "ORDER BY (")
	if open < 0 {
		t.Fatalf("seed DDL declares no ORDER BY:\n%s", ddl)
	}
	tail := ddl[open+len("ORDER BY ("):]
	// The tuple's own closing paren is the one immediately before the
	// statement terminator; the last key element is itself function-wrapped,
	// so a plain "first )" would cut the tuple short.
	closeIdx := strings.Index(tail, ");")
	if closeIdx < 0 {
		t.Fatalf("seed DDL's ORDER BY tuple is unterminated:\n%s", ddl)
	}
	got := strings.TrimSpace(tail[:closeIdx])

	want := strings.Join(append(m.SortingKeyPrefix(), metricsSeedTimestampKeyFn+"("+m.TimestampColumn+")"), ", ")
	if got != want {
		t.Fatalf("seed ORDER BY\n got: %s\nwant: %s", got, want)
	}
}

// TestMetricsSeedDDLNamesEveryColumnFromSchema pins the column names too: a
// schema override that renames a column must not leave the seed declaring the
// old name, which would make the round-trip tests silently seed a table the
// emitter cannot read.
func TestMetricsSeedDDLNamesEveryColumnFromSchema(t *testing.T) {
	t.Parallel()

	m := schema.DefaultOTelMetrics()
	ddl := MetricsSeedDDL(m.SumTable)
	for _, col := range []string{
		m.MetricNameColumn,
		m.AttributesColumn,
		m.ResourceAttributesColumn,
		m.ServiceNameColumn,
		m.TimestampColumn,
		m.ValueColumn,
	} {
		if !strings.Contains(ddl, col) {
			t.Errorf("seed DDL never declares schema column %q:\n%s", col, ddl)
		}
	}
}

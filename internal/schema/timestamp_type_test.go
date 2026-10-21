package schema

import (
	"strings"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/sqltemplates"
)

// nanosTimestampType is the ClickHouse type of the traces and logs timestamp
// columns.
const nanosTimestampType = "DateTime64(9)"

// nestedOpener is the token that follows a Nested column's name in a CREATE
// TABLE template, and nestedCloser the token that ends its field list.
const (
	nestedOpener = "Nested"
	nestedCloser = ")"
)

// columnType returns the declared type of column in a CREATE TABLE template:
// the token after the first top-level definition line that names the column,
// bare or backtick-quoted.
func columnType(t *testing.T, ddl, column string) string {
	t.Helper()
	for line := range strings.SplitSeq(ddl, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.Trim(fields[0], "`") == column {
			return strings.TrimSuffix(fields[1], ",")
		}
	}
	t.Fatalf("no definition of column %q in the template — the parser is broken, not the DDL:\n%s", column, ddl)
	return ""
}

// nestedFieldType returns the declared type of field inside the Nested
// column nested of a CREATE TABLE template, and false when the template
// declares no such Nested column.
func nestedFieldType(t *testing.T, ddl, nested, field string) (string, bool) {
	t.Helper()
	inside := false
	for line := range strings.SplitSeq(ddl, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !inside {
			inside = len(fields) >= 2 && strings.Trim(fields[0], "`") == nested && strings.HasPrefix(fields[1], nestedOpener)
			continue
		}
		if strings.HasPrefix(fields[0], nestedCloser) {
			break
		}
		if len(fields) >= 2 && strings.Trim(fields[0], "`") == field {
			return strings.TrimSuffix(fields[1], ","), true
		}
	}
	if inside {
		t.Fatalf("Nested %s declares no field %q — the parser is broken, not the DDL:\n%s", nested, field, ddl)
	}
	return "", false
}

// TestTimestampColumnTypes_InPinnedExporterDDL pins the timestamp column
// types of every table internal/schema/ddl renders from the pinned
// exporter's templates: DateTime64(9) for traces and logs, and for metrics
// the ColumnType each MetricsTimestampPrecision promises — TimeUnix,
// StartTimeUnix and, on the tables that carry exemplars, the Nested
// Exemplars.TimeUnix. A fork-tag bump that changes one builds cleanly and
// passes every chDB suite that seeds its own tables, and then fails every
// query against a table cerberus created itself; this test is where such a
// bump goes red.
func TestTimestampColumnTypes_InPinnedExporterDDL(t *testing.T) {
	type table struct{ ddl, column, want string }
	tables := map[string]table{
		"traces": {sqltemplates.TracesCreateTable, DefaultOTelTraces().TimestampColumn, nanosTimestampType},
		"logs":   {sqltemplates.LogsCreateTable, DefaultOTelLogs().TimestampColumn, nanosTimestampType},
	}
	m := DefaultOTelMetrics()
	exemplars := 0
	for _, precision := range allMetricsTimestampPrecisions {
		want := precision.ColumnType()
		for name, ddl := range metricsTemplates(precision) {
			prefix := precision.String() + "/metrics_" + name
			tables[prefix] = table{ddl, m.TimestampColumn, want}
			tables[prefix+"/start"] = table{ddl, m.StartTimeColumn, want}
			got, ok := nestedFieldType(t, ddl, m.ExemplarsColumn, ExemplarTimestampField)
			if !ok {
				continue
			}
			exemplars++
			if got != want {
				t.Errorf("%s: %s.%s is %s in the pinned exporter's DDL, want %s",
					prefix, m.ExemplarsColumn, ExemplarTimestampField, got, want)
			}
		}
	}
	if exemplars == 0 {
		t.Fatalf("no metrics template declares a Nested %s column — the parser is broken, not the DDL", m.ExemplarsColumn)
	}
	for name, tbl := range tables {
		if got := columnType(t, tbl.ddl, tbl.column); got != tbl.want {
			t.Errorf("%s: %s is %s in the pinned exporter's DDL, want %s", name, tbl.column, got, tbl.want)
		}
	}
}

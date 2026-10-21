package schema

import (
	"strings"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/sqltemplates"
)

// nanosTimestampType is the ClickHouse type of the traces and logs timestamp
// columns, and of the metrics ones under MetricsTimestampNanos.
const nanosTimestampType = "DateTime64(9)"

// metricsTimestampTypes is the metrics timestamp column type each precision
// provisions. The read path serves both: the boot requirements check detects
// which one a deployment carries.
var metricsTimestampTypes = map[MetricsTimestampPrecision]string{
	MetricsTimestampNanos:   nanosTimestampType,
	MetricsTimestampSeconds: "DateTime",
}

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

// TestTimestampColumnsAreNanosDateTime64_InUpstreamDDL pins the timestamp type
// of every table internal/schema/ddl renders from the pinned exporter's
// templates: DateTime64(9) for traces and logs, and for metrics the type each
// MetricsTimestampPrecision promises. A fork-tag bump that changes one builds
// cleanly and passes every chDB suite that seeds its own tables, and then
// fails every query against a table cerberus created itself; this test is
// where such a bump goes red.
func TestTimestampColumnsAreNanosDateTime64_InUpstreamDDL(t *testing.T) {
	type table struct{ ddl, column, want string }
	tables := map[string]table{
		"traces": {sqltemplates.TracesCreateTable, DefaultOTelTraces().TimestampColumn, nanosTimestampType},
		"logs":   {sqltemplates.LogsCreateTable, DefaultOTelLogs().TimestampColumn, nanosTimestampType},
	}
	m := DefaultOTelMetrics()
	for precision, p := range metricsPrecisions {
		want := metricsTimestampTypes[precision]
		for name, ddl := range metricsTemplates(p) {
			prefix := string(precision) + "/metrics_" + name
			tables[prefix] = table{ddl, m.TimestampColumn, want}
			tables[prefix+"/start"] = table{ddl, m.StartTimeColumn, want}
		}
	}
	for name, tbl := range tables {
		if got := columnType(t, tbl.ddl, tbl.column); got != tbl.want {
			t.Errorf("%s: %s is %s in the pinned exporter's DDL, want %s", name, tbl.column, got, tbl.want)
		}
	}
}

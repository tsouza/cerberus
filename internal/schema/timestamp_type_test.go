package schema

import (
	"strings"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter/sqltemplates"
)

// nanosTimestampType is the ClickHouse type cerberus's emitter requires of
// every signal's timestamp column: it reads them through
// toUnixTimestamp64Nano / fromUnixTimestamp64Nano, which ClickHouse rejects on
// any other type with ILLEGAL_TYPE_OF_ARGUMENT.
const nanosTimestampType = "DateTime64(9)"

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
// templates. A fork-tag bump that changes it builds cleanly and passes every
// chDB suite that seeds its own tables, and then fails every query against a
// table cerberus created itself; this test is where such a bump goes red.
func TestTimestampColumnsAreNanosDateTime64_InUpstreamDDL(t *testing.T) {
	metricsTS := DefaultOTelMetrics().TimestampColumn
	tables := map[string]struct{ ddl, column string }{
		"traces": {sqltemplates.TracesCreateTable, DefaultOTelTraces().TimestampColumn},
		"logs":   {sqltemplates.LogsCreateTable, DefaultOTelLogs().TimestampColumn},
	}
	for name, ddl := range metricsTemplates() {
		tables["metrics_"+name] = struct{ ddl, column string }{ddl, metricsTS}
	}
	for name, tbl := range tables {
		if got := columnType(t, tbl.ddl, tbl.column); got != nanosTimestampType {
			t.Errorf("%s: %s is %s in the pinned exporter's DDL, want %s", name, tbl.column, got, nanosTimestampType)
		}
	}
}

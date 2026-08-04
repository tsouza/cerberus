//go:build chdb

package spec

import (
	"strings"
	"testing"
)

// TestExpandStarProjection_CTEUnionResolvesAliases pins the round-trip
// harness's ability to expand a top-level `SELECT *` that sits over a
// UNION of CTE references — the exact shape chsql.intersectQuery emits
// for a Tempo-compatible `&&` whose arms folded to trace granularity
// (`({…} | count() > 0) && ({…} | count() > 0)`).
//
// Without CTE-aware name discovery the star stays a bare `*`, the Map
// column (`ResourceAttrs`) rides through unwrapped, and chdb-go's
// parquet driver panics with `could not cast to type: MAP`. The
// expander must reach through the parenthesised UNION into the first
// branch's CTE body and borrow its projection aliases so the outer
// SELECT names the columns and rewriteMapProjections can wrap the Map.
func TestExpandStarProjection_CTEUnionResolvesAliases(t *testing.T) {
	t.Parallel()

	const cteBody = "SELECT * FROM (SELECT `TraceId` AS `TraceId`, " +
		"toFloat64(count(1)) AS `Value`, any(`SpanName`) AS `MetricName`, " +
		"any(`ResourceAttributes`) AS `ResourceAttrs`, min(`Timestamp`) AS `TimeUnix`, " +
		"min(toUnixTimestamp64Nano(`Timestamp`)) AS `TraceStartNs`, " +
		"max((toUnixTimestamp64Nano(`Timestamp`) + toInt64(`Duration`))) AS `TraceEndNs` " +
		"FROM `otel_traces` GROUP BY `TraceId`) WHERE (`Value` > 0)"
	query := "WITH _setand_l_1 AS (" + cteBody + "), _setand_r_1 AS (" + cteBody + ") " +
		"SELECT * FROM ((SELECT * FROM _setand_l_1) UNION ALL (SELECT * FROM _setand_r_1)) " +
		"WHERE `TraceId` IN (SELECT `TraceId` FROM _setand_l_1) " +
		"AND `TraceId` IN (SELECT `TraceId` FROM _setand_r_1) LIMIT 1 BY `TraceId`"

	got := expandStarProjection(query, nil)

	// The outer star must have been replaced by the CTE body's explicit
	// alias list.
	wantCols := []string{
		"`TraceId`", "`Value`", "`MetricName`", "`ResourceAttrs`",
		"`TimeUnix`", "`TraceStartNs`", "`TraceEndNs`",
	}
	_, body := stripWithHead(got)
	head, _ := splitOuterSelect(body)
	proj := strings.TrimSpace(head)
	if proj == "*" {
		t.Fatalf("outer star was not expanded:\n%s", got)
	}
	for _, c := range wantCols {
		if !strings.Contains(proj, c) {
			t.Errorf("expanded outer projection missing %s:\n%s", c, proj)
		}
	}

	// The CTE definitions and the trace-granularity dedup key must
	// survive untouched — only the outer projection is rewritten.
	if !strings.Contains(got, "WITH _setand_l_1 AS") {
		t.Errorf("CTE head was dropped:\n%s", got)
	}
	if !strings.Contains(got, "LIMIT 1 BY `TraceId`") {
		t.Errorf("trace-granularity LIMIT BY key was altered:\n%s", got)
	}

	// The Map-wrap pass must now find the named Map column and wrap it.
	full := rewriteMapProjections(got)
	if !strings.Contains(full, "toJSONString(`ResourceAttrs`)") {
		t.Errorf("Map column ResourceAttrs was not wrapped in toJSONString:\n%s", full)
	}
}

// TestExpandStarProjection_BareTableResolvesFromSeedDDL pins #1431: the
// emitSearchTraceLimit drain shape `SELECT s.* FROM (SELECT * FROM
// <table> ...) AS s ...` has no inner SUBQUERY projection list to borrow
// column names from — the inner relation is a bare table scan. Without a
// fallback to the seed's own CREATE TABLE DDL, expandQualifiedStar bails
// out unchanged and a Map column (SpanAttributes) rides through
// unwrapped, which panics chdb-go's parquet driver with `could not cast
// to type: MAP`.
func TestExpandStarProjection_BareTableResolvesFromSeedDDL(t *testing.T) {
	t.Parallel()

	const seed = "CREATE TABLE otel_traces (\n" +
		"    TraceId String,\n" +
		"    SpanId String,\n" +
		"    Timestamp DateTime64(9),\n" +
		"    SpanName String,\n" +
		"    SpanAttributes Map(String, String)\n" +
		") ENGINE = MergeTree ORDER BY (Timestamp, SpanId);\n" +
		"INSERT INTO otel_traces VALUES ('t1', 's1', toDateTime64('2026-01-01 00:00:00', 9), 'checkout', map());"
	query := "SELECT s.* FROM (SELECT * FROM `otel_traces` PREWHERE (`SpanName` = ?) " +
		"WHERE (`Timestamp` >= fromUnixTimestamp64Nano(?))) AS s WHERE `TraceId` IN " +
		"(SELECT `TraceId` FROM (SELECT * FROM `otel_traces` PREWHERE (`SpanName` = ?)) " +
		"GROUP BY `TraceId` ORDER BY min(`Timestamp`) DESC, `TraceId` LIMIT 2)"

	got := expandStarProjection(query, seedTableColumns(seed))

	head, _ := splitOuterSelect(got)
	proj := strings.TrimSpace(head)
	if proj == "s.*" || proj == "*" {
		t.Fatalf("outer qualified star over a bare table scan was not expanded:\n%s", got)
	}
	wantCols := []string{"s.`TraceId`", "s.`SpanId`", "s.`Timestamp`", "s.`SpanName`", "s.`SpanAttributes`"}
	for _, c := range wantCols {
		if !strings.Contains(proj, c) {
			t.Errorf("expanded outer projection missing %s:\n%s", c, proj)
		}
	}

	// The inner IN-subquery scan (never itself starred at the outer
	// level) and the ORDER BY / LIMIT tie-break clause must survive
	// untouched — only the outer s.* projection is rewritten.
	if !strings.Contains(got, "GROUP BY `TraceId` ORDER BY min(`Timestamp`) DESC, `TraceId` LIMIT 2") {
		t.Errorf("inner tie-break subquery was altered:\n%s", got)
	}

	// The Map-wrap pass must now find the named Map column and wrap it.
	full := rewriteMapProjections(got)
	if !strings.Contains(full, "toJSONString(") || !strings.Contains(full, "SpanAttributes") {
		t.Errorf("Map column SpanAttributes was not wrapped in toJSONString:\n%s", full)
	}
}

// TestExpandStarProjection_BareTableWithoutSeedStillBails asserts the
// pre-#1431 fallback behaviour is preserved when no seed-derived column
// map is available (tableCols == nil): a qualified star over a bare
// table scan is left unchanged rather than guessed at.
func TestExpandStarProjection_BareTableWithoutSeedStillBails(t *testing.T) {
	t.Parallel()

	query := "SELECT s.* FROM (SELECT * FROM `otel_traces` WHERE (`SpanName` = ?)) AS s " +
		"WHERE `TraceId` IN (SELECT `TraceId` FROM (SELECT * FROM `otel_traces`) GROUP BY `TraceId`)"

	got := expandStarProjection(query, nil)
	if got != query {
		t.Errorf("expected query to pass through unchanged without a seed column map, got:\n%s", got)
	}
}

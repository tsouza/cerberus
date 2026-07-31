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

	got := expandStarProjection(query)

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

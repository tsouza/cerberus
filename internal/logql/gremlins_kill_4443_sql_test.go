package logql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file kills LIVED gremlins mutants of the phase4-logql-other-b leg
// (cerberus issue #4443) through the SQL a query lowers to.

// TestRangeAggregationFilterPastTheLineStaysAboveWhereOrderHolds pins the
// end of the ordered prefix in WHERE order: past the line filter
// [orderedScanPrefix] finds, a filter that reads a parser's labels and a
// key of a map the stage reads as a whole stays above the SELECT that
// reads otel_logs, although the level filter ahead of it moved there. Only
// the filters up to the line filter read the stage in pipeline order where
// the table is read.
func TestRangeAggregationFilterPastTheLineStaysAboveWhereOrderHolds(t *testing.T) {
	t.Parallel()
	const query = `sum(count_over_time({job="wide"} | detected_level="error" | tenant!="tenant-7" |= "orders/5" | logfmt | region="eu" [5m]))`
	const level, line, region = "lower(`_logql_level_source`)", "position(`Body`", "if(mapContains(`_logql_labels_"
	sql, _ := rangeAggregationSQLUnder(t, query, chplan.SchemaStrategies{PrewhereInWhereOrder: true})
	scope := scopeAround(t, sql, "FROM `otel_logs`")
	where := scope[strings.Index(scope, "FROM `otel_logs`"):]
	if !strings.Contains(where, level) || !strings.Contains(where, line) {
		t.Fatalf("in WHERE order, the SELECT reading otel_logs does not run the level and line filters: %s", where)
	}
	if strings.Contains(where, region) {
		t.Errorf("in WHERE order, the region filter after the line filter runs in the SELECT reading otel_logs: %s", where)
	}
	if !strings.Contains(sql, region) {
		t.Errorf("the statement does not run the region filter: %s", sql)
	}
}

// TestRangeAggregationLevelOfASeverityOnlySchema pins that a schema whose
// only level column is its severity text reads that column as the level
// source, with no value of its own for it, and normalises it in place.
func TestRangeAggregationLevelOfASeverityOnlySchema(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelLogs()
	s.AttributesColumn = ""
	s.SeverityNumberColumn = ""
	start := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	for _, query := range []string{
		`sum(count_over_time({job="api"} | detected_level="error" [5m]))`,
		`sum by (detected_level) (count_over_time({job="api"} [5m]))`,
	} {
		l := &logql.Lang{Schema: s, Start: start, End: start.Add(24 * time.Hour), Step: time.Hour}
		dr, err := (&engine.Engine{Optimizer: optimizer.Default()}).DryRunSQL(context.Background(), l, query)
		if err != nil {
			t.Fatalf("DryRunSQL(%q): %v", query, err)
		}
		if strings.Contains(dr.SQL, "`_logql_level_source`") {
			t.Errorf("%s: the statement defines a level source of its own over the severity column: %s", query, dr.SQL)
		}
		if want := "lower(`" + s.SeverityColumn + "`)"; !strings.Contains(dr.SQL, want) {
			t.Errorf("%s: the statement does not normalise %s: %s", query, want, dr.SQL)
		}
	}
}

// TestLogQueryFilterReadingANewKeyKeepsItsPlaceBehindALevelFilter pins
// that behind a filter that reads a value of the stack, a log query folds
// a later filter into the stream scan's filter only when the columns read
// ahead of it already read every column it reads: a key of a map column
// counts as read only when that key, or the whole map, is. The region
// filter reads ResourceAttributes['region'], which nothing ahead of it
// reads, so it keeps its place behind the level filter rather than read
// that key for the rows the level drops; the tenant filter, ahead of the
// level filter, folds.
func TestLogQueryFilterReadingANewKeyKeepsItsPlaceBehindALevelFilter(t *testing.T) {
	t.Parallel()
	const level, tenant, region = "(`_logql_detected_level` = ?)", "`ResourceAttributes`[?]) != ?)", "`ResourceAttributes`[?]) = ?)"
	sql := logQuerySQL(t, `{job="api"} | tenant!="x" | detected_level="error" | region="eu"`)
	scope := scopeAround(t, sql, "FROM `otel_logs`")
	where := scope[strings.Index(scope, "FROM `otel_logs`"):]
	at := map[string]int{}
	for _, filter := range []string{tenant, level, region} {
		if at[filter] = strings.Index(where, filter); at[filter] < 0 {
			t.Fatalf("the SELECT reading otel_logs does not run %s: %s", filter, where)
		}
	}
	if at[tenant] > at[level] || at[level] > at[region] {
		t.Errorf("the SELECT reading otel_logs does not run tenant, level, region in that order: %s", where)
	}
}

package logql_test

import (
	"strings"
	"testing"
)

// logsScanWhere marks where the WHERE clause of the SELECT that reads the
// logs table begins in a log query's statement.
const logsScanWhere = "FROM `otel_logs` WHERE "

// TestLogQueryFilterConjunctOrder pins the order of a log query's filters in
// the WHERE of the SELECT that reads the table. ClickHouse runs a multi-step
// PREWHERE in the order of the WHERE conjuncts and reads each column at the
// first step that needs it, so a filter belongs ahead of one the pipeline
// runs first only when it reads no column, and no key of a map column, the
// earlier steps do not already read. A line filter moved ahead of a `detected_level` filter reads the body
// of every row the level filter would have dropped; one behind a parser's
// label filter, or behind a line filter the scan already runs, reads nothing
// new.
func TestLogQueryFilterConjunctOrder(t *testing.T) {
	t.Parallel()
	const (
		// The statement binds each literal as a parameter.
		level   = "(`_logql_detected_level` = ?)"
		failed  = "match(`Body`, ?)"
		request = "(position(`Body`, ?) > 0)"
		// A label filter after `| json` reads the parsed labels map.
		parsed = "mapContains(`_logql_labels_"
		// A structured-metadata filter with no parser reads the attribute
		// map as a whole; the level filter reads only some of its keys.
		metadata = "if(mapContains(`LogAttributes`, ?)"
	)
	for _, tc := range []struct {
		name  string
		query string
		order []string
	}{
		{
			name:  "level_then_line",
			query: `{service_name="tempo"} | detected_level="error" |~ "(?i)failed"`,
			order: []string{level, failed},
		},
		{
			name:  "line_then_level",
			query: `{service_name="tempo"} |~ "(?i)failed" | detected_level="error"`,
			order: []string{failed, level},
		},
		{
			name:  "level_then_parsed_then_line",
			query: `{service_name="tempo"} | detected_level="error" | json | status >= 400 |= "request"`,
			order: []string{level, parsed, request},
		},
		{
			name:  "line_ahead_of_parsed",
			query: `{service_name="tempo"} | json | status >= 400 |= "request"`,
			order: []string{request, parsed},
		},
		{
			name:  "level_then_metadata",
			query: `{service_name="tempo"} | detected_level="error" | foo="bar"`,
			order: []string{level, metadata},
		},
		{
			name:  "line_ahead_of_level_behind_a_line",
			query: `{service_name="tempo"} |= "request" | detected_level="error" |~ "(?i)failed"`,
			order: []string{request, failed, level},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scan := scopeAround(t, logQuerySQL(t, tc.query), logsScanWhere)
			where := scan[strings.Index(scan, logsScanWhere)+len(logsScanWhere):]
			prev, prevAt := "", -1
			for _, conjunct := range tc.order {
				i := strings.Index(where, conjunct)
				if i < 0 {
					t.Fatalf("the table's WHERE does not read %s: %s", conjunct, where)
				}
				if i < prevAt {
					t.Errorf("the table's WHERE reads %s ahead of %s: %s", conjunct, prev, where)
				}
				prev, prevAt = conjunct, i
			}
		})
	}
}

// TestNestedLogQueryRunsLineFilterAtScan pins the nested form's side: past
// the statement bound the stage renders one SELECT per layer, where a filter
// reads the subquery below it and ClickHouse cannot run it as the table is
// read. A line filter there joins the scan's own filter, wherever the
// pipeline runs it.
func TestNestedLogQueryRunsLineFilterAtScan(t *testing.T) {
	t.Parallel()
	const request = "(position(`Body`, ?) > 0)"
	sql := logQuerySQL(t, `{service_name="tempo"} | detected_level="error" | json | status >= 400 | duration > 10ms |= "request"`)
	if definingScopeReads(t, sql, "_logql_labels_1") {
		t.Fatalf("the stage renders fused, not nested: %s", sql)
	}
	if scan := scopeAround(t, sql, logsScanWhere); !strings.Contains(scan, request) {
		t.Errorf("the SELECT that reads the table does not run the line filter: %s", scan)
	}
}

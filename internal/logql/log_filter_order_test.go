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
// runs first only when it reads no column the earlier steps do not already
// read. A line filter moved ahead of a `detected_level` filter reads the body
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
			name:  "line_ahead_of_level_behind_a_line",
			query: `{service_name="tempo"} |= "request" | detected_level="error" |~ "(?i)failed"`,
			order: []string{request, failed, level},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql := logQuerySQL(t, tc.query)
			at := strings.Index(sql, logsScanWhere)
			if at < 0 {
				t.Fatalf("no SELECT reads the logs table with a WHERE: %s", sql)
			}
			where := sql[at+len(logsScanWhere):]
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

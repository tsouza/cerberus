package logql_test

import (
	"strings"
	"testing"
)

// logsScanWhere marks where the WHERE clause of the SELECT that reads the
// logs table begins in a log query's statement.
const logsScanWhere = "FROM `otel_logs` WHERE "

// TestLogQueryFiltersKeepPipelineOrder pins that a log query's filters reach
// the WHERE of the SELECT that reads the table in the order its pipeline
// writes them. ClickHouse runs a multi-step PREWHERE in the order of the
// WHERE conjuncts, so a line filter moved ahead of a `detected_level` filter
// that the pipeline runs first reads the body of every row the level filter
// would have dropped.
func TestLogQueryFiltersKeepPipelineOrder(t *testing.T) {
	t.Parallel()
	const (
		// The statement binds each literal as a parameter.
		level  = "(`_logql_detected_level` = ?)"
		failed = "match(`Body`, ?)"
		first  = "(position(`Body`, ?) > 0)"
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
			name:  "line_level_line",
			query: `{service_name="tempo"} |= "request" | detected_level="error" |~ "(?i)failed"`,
			order: []string{first, level, failed},
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
					t.Errorf("the table's WHERE reads %s ahead of %s, which the pipeline runs first: %s", conjunct, prev, where)
				}
				prev, prevAt = conjunct, i
			}
		})
	}
}

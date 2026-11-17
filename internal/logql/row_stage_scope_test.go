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

// rangeAggregationSQL renders q as a range query over one day.
func rangeAggregationSQL(t *testing.T, q string) (string, chplan.Node) {
	t.Helper()
	start := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	l := &logql.Lang{Schema: schema.DefaultOTelLogs(), Start: start, End: start.Add(24 * time.Hour), Step: time.Hour}
	dr, err := (&engine.Engine{Optimizer: optimizer.Default()}).DryRunSQL(context.Background(), l, q)
	if err != nil {
		t.Fatalf("DryRunSQL(%q): %v", q, err)
	}
	return dr.SQL, dr.Plan
}

// TestRangeAggregationFiltersRunWhereTheTableIsRead pins that a range
// aggregation evaluates its filters, and the values they read, in the
// SELECT that reads the logs table: a line filter or a label filter on a
// stored column joins the scan's own predicates, the detected_level
// source reads the table's map there, and a filter on a parsed label or on
// the unwrap source runs there with the parser's labels map it reads.
// Behind a subquery ClickHouse reads a map key as the whole map and cannot
// place the filter in PREWHERE.
func TestRangeAggregationFiltersRunWhereTheTableIsRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query string
		// reads are what the SELECT reading the table must contain.
		reads []string
	}{
		{`sum(count_over_time({job="wide"} |= "orders/5 " [5m]))`, []string{"position(`Body`", "AS `_logql_level_source`"}},
		{`sum(count_over_time({job="wide"} | detected_level="fatal" [5m]))`, []string{"AS `_logql_level_source`"}},
		{`sum_over_time({job="api"} | logfmt | unwrap duration(latency) [5m])`, []string{"AS `_logql_unwrap_source`", "(`_logql_unwrap_source` != ?)"}},
		{`sum by (level) (count_over_time({service_name="api"} | logfmt | level=~"error|warn" [1h]))`, []string{"AS `_logql_labels_0`", "mapContains(`_logql_labels_0`"}},
		{`sum by (category) (count_over_time({job="api"} | unpack | __error__="" [5m]))`, []string{"AS `_logql_labels_0`", "mapContains(`_logql_labels_0`"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			sql, _ := rangeAggregationSQL(t, tc.query)
			scope := scopeAround(t, sql, "FROM `otel_logs`")
			for _, want := range tc.reads {
				if !strings.Contains(scope, want) {
					t.Errorf("the SELECT reading otel_logs does not contain %s: %s", want, scope)
				}
			}
		})
	}
}

// TestRangeAggregationMultiplyingValuesStayNested pins the other side: a
// value that reads another value many times — the detected_level
// normalisation reads its source once per level it compares, an error
// mark reads the labels map once per key, an unwrap conversion reads its
// parts many times — is defined in a SELECT of its own, reading the value
// below it as a column. In one SELECT, ClickHouse's analyzer copies an
// alias's expression into every place that reads it.
func TestRangeAggregationMultiplyingValuesStayNested(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query string
		// nested are aliases defined outside the SELECT that reads the
		// table.
		nested []string
	}{
		{`sum(count_over_time({job="wide"} | detected_level="fatal" [5m]))`, []string{"_logql_detected_level"}},
		{`max_over_time({service_name="worker"} | json | bytes > 100 | unwrap bytes [1h]) by (tenant)`, []string{"_logql_labels_1", "_logql_detected_level"}},
		{`avg_over_time({service_name="web-server"} | json | status >= 400 | unwrap duration(duration) [1h]) by (method)`, []string{"_logql_labels_1", "_logql_unwrap_stripped"}},
		{`sum_over_time({job="api"} | logfmt | unwrap duration(latency) | __error__ = "" [5m])`, []string{"_logql_unwrap_stripped", "_logql_unwrap_labels"}},
		{`count_over_time({service_name="mimir"} | logfmt | latency > 250ms or latency < 1ms [5m])`, []string{"_logql_filter_1_stripped", "_logql_filter_2_stripped"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			sql, _ := rangeAggregationSQL(t, tc.query)
			scope := scopeAround(t, sql, "FROM `otel_logs`")
			for _, alias := range tc.nested {
				if !strings.Contains(sql, "AS `"+alias+"`") {
					t.Fatalf("%s is not defined: %s", alias, sql)
				}
				if strings.Contains(scope, "AS `"+alias+"`") {
					t.Errorf("%s is defined in the SELECT reading otel_logs: %s", alias, scope)
				}
			}
		})
	}
}

// TestRangeAggregationWholeMapReadsStayAboveTheScan pins that a label
// filter on a stored label, which reads LogAttributes as a whole through
// mapContains, stays above the scan, and that the detected_level source
// then reads LogAttributes there too: a key subcolumn read in the scan's
// scope would read the map a second time.
func TestRangeAggregationWholeMapReadsStayAboveTheScan(t *testing.T) {
	t.Parallel()
	sql, _ := rangeAggregationSQL(t, `sum(count_over_time({job="wide"} | tenant="tenant-7" [5m]))`)
	scope := scopeAround(t, sql, "FROM `otel_logs`")
	for _, absent := range []string{"mapContains(`LogAttributes`", "AS `_logql_level_source`"} {
		if strings.Contains(scope, absent) {
			t.Errorf("the SELECT reading otel_logs contains %s: %s", absent, scope)
		}
	}
	if !strings.Contains(sql, "mapContains(`LogAttributes`") {
		t.Fatalf("the label filter is missing: %s", sql)
	}
}

// TestLogQueryRowStageIsOneScope pins the other side: a log query's label
// filter reads the labels map in the SELECT that defines it.
func TestLogQueryRowStageIsOneScope(t *testing.T) {
	t.Parallel()
	if !definingScopeReads(t, logQuerySQL(t, corpusParserQuery), "_logql_labels_1") {
		t.Fatal("the corpus query's label filter does not read _logql_labels_1 in the SELECT that defines it")
	}
}

// TestLogQueryRowStageNestsPastTheStatementBound pins the fallback: two
// numeric filters after `| json` each stamp an error label into a new
// labels map that reads the previous one about a dozen times, so with
// every alias read replaced by its expression the stage is megabytes of
// SQL — the statement an inlining lowering refused. In one SELECT
// ClickHouse's analyzer performs that replacement, so the stage renders
// nested, one SELECT per layer, and the statement stays small.
func TestLogQueryRowStageNestsPastTheStatementBound(t *testing.T) {
	t.Parallel()
	sql := logQuerySQL(t, `{service_name="web"} | json | status >= 400 | duration > 10ms`)
	if definingScopeReads(t, sql, "_logql_labels_0") {
		t.Errorf("the SELECT defining _logql_labels_0 also reads it: %s", sql)
	}
	if !definingScopeReads(t, logQuerySQL(t, `{service_name="web"} | json | status >= 400`), "_logql_labels_0") {
		t.Error("one numeric filter after `| json` no longer reads _logql_labels_0 in the SELECT that defines it")
	}
}

// definingScopeReads reports whether the innermost SELECT of sql that defines
// alias also reads it, outside the subqueries it reads from.
func definingScopeReads(t *testing.T, sql, alias string) bool {
	t.Helper()
	quoted := "`" + alias + "`"
	def := strings.LastIndex(sql, "AS "+quoted)
	if def < 0 {
		t.Fatalf("no definition of %s in %s", alias, sql)
	}
	return strings.Count(scopeAt(t, sql, def), quoted) > 1
}

// scopeAround returns the text of the innermost SELECT of sql holding the
// first occurrence of marker, without the subqueries it reads from.
func scopeAround(t *testing.T, sql, marker string) string {
	t.Helper()
	at := strings.Index(sql, marker)
	if at < 0 {
		t.Fatalf("no %s in %s", marker, sql)
	}
	return scopeAt(t, sql, at)
}

// scopeAt returns the text of the innermost subquery of sql holding the
// byte at, without the subqueries it reads from.
func scopeAt(t *testing.T, sql string, at int) string {
	t.Helper()
	open, depth := -1, 0
	for i := at; i >= 0; i-- {
		switch sql[i] {
		case ')':
			depth++
		case '(':
			if depth == 0 {
				open = i
			}
			depth--
		}
		if open >= 0 {
			break
		}
	}
	if open < 0 {
		t.Fatalf("byte %d is not inside a subquery: %s", at, sql)
	}
	// Keep the scope's own text: drop every parenthesised SELECT inside it.
	var scope strings.Builder
	depth = 0
	skip := -1
	for i := open + 1; i < len(sql); i++ {
		c := sql[i]
		if c == '(' {
			if skip < 0 && strings.HasPrefix(sql[i+1:], "SELECT ") {
				skip = depth
			}
			depth++
		}
		if c == ')' {
			if depth == 0 {
				break
			}
			depth--
			if skip == depth {
				skip = -1
				continue
			}
		}
		if skip < 0 {
			scope.WriteByte(c)
		}
	}
	return scope.String()
}

package logql_test

import (
	"context"
	"regexp"
	"strconv"
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
	return rangeAggregationSQLUnder(t, q, chplan.SchemaStrategies{})
}

// rangeAggregationSQLUnder renders q as a range query over one day under
// strategies.
func rangeAggregationSQLUnder(t *testing.T, q string, strategies chplan.SchemaStrategies) (string, chplan.Node) {
	t.Helper()
	start := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	l := &logql.Lang{Schema: schema.DefaultOTelLogs(), Start: start, End: start.Add(24 * time.Hour), Step: time.Hour, Strategies: strategies}
	dr, err := (&engine.Engine{Optimizer: optimizer.Default()}).DryRunSQL(context.Background(), l, q)
	if err != nil {
		t.Fatalf("DryRunSQL(%q): %v", q, err)
	}
	return dr.SQL, dr.Plan
}

// TestRangeAggregationFiltersRunWhereTheTableIsRead pins that a range
// aggregation evaluates its filters, and the values they read, in the
// SELECT that reads the logs table: a line filter joins the scan's own
// predicates, the detected_level source reads the table's map there, in
// WHERE order a detected_level filter runs there over that source, an
// absent_over_time reads its line filter there, and a filter on a parsed
// label or on
// the unwrap source runs there with the parser's labels map it reads.
// Behind a subquery ClickHouse reads a map key as the whole map and cannot
// place the filter in PREWHERE.
func TestRangeAggregationFiltersRunWhereTheTableIsRead(t *testing.T) {
	t.Parallel()
	inWhereOrder := chplan.SchemaStrategies{PrewhereInWhereOrder: true}
	for _, tc := range []struct {
		query string
		// strategies are the ones the query is rendered under.
		strategies chplan.SchemaStrategies
		// reads are what the SELECT reading the table must contain.
		reads []string
	}{
		{`sum(count_over_time({job="wide"} |= "orders/5 " [5m]))`, chplan.SchemaStrategies{}, []string{"position(`Body`", "AS `_logql_level_source`"}},
		{`sum(count_over_time({job="wide"} | detected_level="fatal" [5m]))`, inWhereOrder, []string{"AS `_logql_level_source`", "has(array(?), lower(`_logql_level_source`))"}},
		{`sum by (detected_level) (count_over_time({job="wide"} | detected_level="debug" or detected_level="info" or detected_level="custom" [5m]))`, inWhereOrder, []string{"(has(array(?, ?, ?, ?, ?), lower(`_logql_level_source`)) OR has(array(?), `_logql_level_source`))"}},
		{`absent_over_time({job="api"} |= "timeout" [5m])`, chplan.SchemaStrategies{}, []string{"position(`Body`"}},
		{`sum_over_time({job="api"} | logfmt | unwrap duration(latency) [5m])`, chplan.SchemaStrategies{}, []string{"AS `_logql_unwrap_source`", "(`_logql_unwrap_source` != ?)"}},
		{`sum by (level) (count_over_time({service_name="api"} | logfmt | level=~"error|warn" [1h]))`, chplan.SchemaStrategies{}, []string{"AS `_logql_labels_0`", "mapContains(`_logql_labels_0`"}},
		{`sum by (category) (count_over_time({job="api"} | unpack | __error__="" [5m]))`, chplan.SchemaStrategies{}, []string{"AS `_logql_labels_0`", "mapContains(`_logql_labels_0`"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			sql, _ := rangeAggregationSQLUnder(t, tc.query, tc.strategies)
			scope := scopeAround(t, sql, "FROM `otel_logs`")
			for _, want := range tc.reads {
				if !strings.Contains(scope, want) {
					t.Errorf("the SELECT reading otel_logs does not contain %s: %s", want, scope)
				}
			}
		})
	}
}

// TestRangeAggregationScanFiltersKeepTheirOrder pins that, in WHERE order,
// the filters a range aggregation evaluates where the table is read keep
// the order the pipeline gives them: a detected_level filter written ahead
// of a line filter runs ahead of it, so a server that runs a multi-step
// PREWHERE in WHERE order reads the line only for the rows the level keeps.
func TestRangeAggregationScanFiltersKeepTheirOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query string
		first string
		then  string
	}{
		{`sum(count_over_time({job="wide"} | detected_level="error" |= "orders/5" [5m]))`, "lower(`_logql_level_source`)", "position(`Body`"},
		{`sum(count_over_time({job="wide"} |= "orders/5" | detected_level="error" [5m]))`, "position(`Body`", "lower(`_logql_level_source`)"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			sql, _ := rangeAggregationSQLUnder(t, tc.query, chplan.SchemaStrategies{PrewhereInWhereOrder: true})
			scope := scopeAround(t, sql, "FROM `otel_logs`")
			first, then := strings.Index(scope, tc.first), strings.Index(scope, tc.then)
			if first < 0 || then < 0 || first > then {
				t.Errorf("the SELECT reading otel_logs does not read %s ahead of %s: %s", tc.first, tc.then, scope)
			}
		})
	}
}

// levelThenLineBehindAWholeMapRead are range aggregations whose level filter
// a line filter follows, with a filter that reads LogAttributes whole in
// between or a grouping label that reads it whole.
var levelThenLineBehindAWholeMapRead = []string{
	`sum(count_over_time({job="wide"} | detected_level="error" | tenant!="tenant-7" |= "orders/5" [5m]))`,
	`sum(count_over_time({job="wide"} | detected_level="error" | json | status >= 400 |= "orders/5" [5m]))`,
	`sum by (tenant) (count_over_time({job="wide"} | detected_level="error" |= "orders/5" [5m]))`,
	`sum(bytes_over_time({job="wide"} | detected_level!="info" | tenant!="tenant-7" |= "request" [5m]))`,
	`absent_over_time({job="wide"} | detected_level="error" | tenant!="tenant-7" |= "orders/5" [5m])`,
	`sum(rate({job="wide"} | detected_level=~"err.*" | tenant!="tenant-7" |= "orders/5" [5m]))`,
	`sum(count_over_time({job="wide"} | detected_level!~"info|debug" | tenant!="tenant-7" |= "orders/5" [5m]))`,
}

// TestRangeAggregationLevelFilterLeadsWhereOrderHolds pins that on servers
// that run a multi-step PREWHERE in WHERE order
// (chplan.SchemaStrategies.PrewhereInWhereOrder) a level filter that a line
// filter follows runs in the SELECT that reads otel_logs, ahead of the line
// filter, even when the stage reads LogAttributes whole: ClickHouse then
// reads the body only for the rows the level keeps. On every other server
// the statement is the one a reordering server reads best: the line filter
// joins the scan's predicates and the level source stays above.
func TestRangeAggregationLevelFilterLeadsWhereOrderHolds(t *testing.T) {
	t.Parallel()
	const level, line, tenant = "lower(`_logql_level_source`)", "position(`Body`", "mapContains(`LogAttributes`"
	for _, query := range levelThenLineBehindAWholeMapRead {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			sql, _ := rangeAggregationSQLUnder(t, query, chplan.SchemaStrategies{PrewhereInWhereOrder: true})
			scope := scopeAround(t, sql, "FROM `otel_logs`")
			where := scope[strings.Index(scope, "FROM `otel_logs`"):]
			first, then := strings.Index(where, level), strings.Index(where, line)
			if first < 0 || then < 0 || first > then {
				t.Errorf("in WHERE order, the SELECT reading otel_logs does not read %s ahead of %s: %s", level, line, where)
			}
			// A label filter the pipeline runs after the level filter runs
			// after it there too.
			if at := strings.Index(where, tenant); strings.Contains(query, "tenant!=") && (at < first || at > then) {
				t.Errorf("in WHERE order, the tenant filter does not run between %s and %s: %s", level, line, where)
			}
			sql, _ = rangeAggregationSQLUnder(t, query, chplan.SchemaStrategies{})
			scope = scopeAround(t, sql, "FROM `otel_logs`")
			if strings.Contains(scope, "AS `_logql_level_source`") || !strings.Contains(scope, line) {
				t.Errorf("on a reordering server, the SELECT reading otel_logs defines the level source or does not read the line: %s", scope)
			}
		})
	}
}

// TestRangeAggregationFilterBetweenLevelAndLineJoinsThemWhereOrderHolds pins
// that in WHERE order a filter the pipeline runs between the level filter
// and the line filter runs between them in the SELECT that reads otel_logs,
// even when its value multiplies the parser's labels past the scope's
// expansion bound: the stage reads up to the line filter in pipeline order,
// so the line is read only for the rows both filters keep.
func TestRangeAggregationFilterBetweenLevelAndLineJoinsThemWhereOrderHolds(t *testing.T) {
	t.Parallel()
	const query = `sum(count_over_time({job="wide"} | detected_level="error" | json | status >= 400 |= "orders/5" [5m]))`
	sql, _ := rangeAggregationSQLUnder(t, query, chplan.SchemaStrategies{PrewhereInWhereOrder: true})
	scope := scopeAround(t, sql, "FROM `otel_logs`")
	where := scope[strings.Index(scope, "FROM `otel_logs`"):]
	level := strings.Index(where, "lower(`_logql_level_source`)")
	status := strings.Index(where, "toFloat64OrZero(")
	line := strings.Index(where, "position(`Body`")
	if level < 0 || status < level || line < status {
		t.Errorf("the WHERE of the SELECT reading otel_logs does not run level, status, line in that order: %s", where)
	}
	sql, _ = rangeAggregationSQLUnder(t, query, chplan.SchemaStrategies{})
	scope = scopeAround(t, sql, "FROM `otel_logs`")
	if strings.Contains(scope[strings.Index(scope, "FROM `otel_logs`"):], "toFloat64OrZero(") {
		t.Errorf("on a reordering server the status filter runs where the table is read: %s", scope)
	}
}

// TestRangeAggregationLevelRegexLeadsWhereOrderHolds pins that a
// detected_level regex matcher reads the level source, as an equality does,
// and in WHERE order runs in the SELECT that reads otel_logs ahead of a
// line filter written after it, even for a pattern whose predicate reads
// the source past the scope's expansion bound: a server that keeps WHERE
// order then reads the
// body only for the rows the level keeps. On a reordering server, which
// chooses the order itself, it reads the normalised level
// ([TestRangeAggregationLevelFilterReadsTheNormalisedLevelOnAReorderingServer]).
func TestRangeAggregationLevelRegexLeadsWhereOrderHolds(t *testing.T) {
	t.Parallel()
	const level, line = "lower(`_logql_level_source`)", "position(`Body`"
	for _, query := range []string{
		`sum(rate({job="wide"} | detected_level=~"err.*" |= "error" [5m]))`,
		`sum(count_over_time({job="wide"} | detected_level=~"error|fatal" |= "request" [5m]))`,
		`sum(count_over_time({job="wide"} | detected_level!~"info" |= "request" [5m]))`,
	} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			sql, _ := rangeAggregationSQLUnder(t, query, chplan.SchemaStrategies{PrewhereInWhereOrder: true})
			scope := scopeAround(t, sql, "FROM `otel_logs`")
			where := scope[strings.Index(scope, "FROM `otel_logs`"):]
			first, then := strings.Index(where, level), strings.Index(where, line)
			if first < 0 || then < 0 || first > then {
				t.Errorf("in WHERE order, the SELECT reading otel_logs does not read %s ahead of %s: %s", level, line, where)
			}
			// The filter itself reads the level source: only
			// levelMatchesExpr renders it as a set membership of its
			// lowercased value.
			if !strings.Contains(sql, "has(array(") || !strings.Contains(sql, "), "+level+")") {
				t.Errorf("in WHERE order, the level filter does not read the level source: %s", sql)
			}
		})
	}
}

// TestRangeAggregationLevelFilterLeadsALaterLabelFilterWhereOrderHolds pins
// that in WHERE order a level filter that a parsed label's filter follows
// runs in the SELECT that reads otel_logs, ahead of that filter, even for a
// level pattern whose predicate reads the source past the scope's expansion
// bound: a server that keeps WHERE order then parses the body only for the
// rows the level keeps. The regex matcher, alone or OR-ed, is the shape
// that stayed above the scan while the label filter moved ahead of it. A
// label filter, an error filter or an unwrap's filter after the label
// filter still runs there after it, so joining the scope does not push
// them out; the equality and the negative matcher fit the bound and pin
// that they keep leading. A label filter whose own reads the conjunct ahead
// of it already reads stays where the bound puts the one ahead of it, last.
func TestRangeAggregationLevelFilterLeadsALaterLabelFilterWhereOrderHolds(t *testing.T) {
	t.Parallel()
	const level, label, unwrap = "lower(`_logql_level_source`)", "mapContains(`_logql_labels_", "`_logql_unwrap_source` != "
	for _, tc := range []struct {
		query string
		// inOrder are the markers the WHERE of the SELECT reading otel_logs
		// carries, each after the one before it.
		inOrder []string
	}{
		{`sum by (method) (count_over_time({job="wide"} | detected_level=~"error|fatal" | logfmt | status="500" [5m]))`, []string{level, label}},
		{`sum by (method) (count_over_time({job="wide"} | detected_level!~"info" | logfmt | status="500" [5m]))`, []string{level, label}},
		{`sum by (method) (count_over_time({job="wide"} | detected_level="error" | logfmt | status="500" [5m]))`, []string{level, label}},
		{`sum by (method) (count_over_time({job="wide"} | detected_level=~"error|fatal" | logfmt | status="500" | method="GET" [5m]))`, []string{level, label, label}},
		{`sum by (method) (count_over_time({job="wide"} | detected_level=~"error|fatal" | logfmt | status="500" | __error__="" [5m]))`, []string{level, label, label}},
		{`sum(sum_over_time({job="wide"} | detected_level=~"error|fatal" | logfmt | status="500" | unwrap latency [5m]))`, []string{level, label, unwrap}},
		{`sum(count_over_time({job="wide"} | detected_level=~"error|fatal" or detected_level="warn" | logfmt | status="500" [5m]))`, []string{level, label}},
		{`sum(count_over_time({job="wide"} | detected_level=~"error|fatal" | logfmt | status=~"5.*" | method="GET" [5m]))`, []string{level, label}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			sql, _ := rangeAggregationSQLUnder(t, tc.query, chplan.SchemaStrategies{PrewhereInWhereOrder: true})
			scope := scopeAround(t, sql, "FROM `otel_logs`")
			where := scope[strings.Index(scope, "FROM `otel_logs`"):]
			rest := where
			for _, marker := range tc.inOrder {
				at := strings.Index(rest, marker)
				if at < 0 {
					t.Fatalf("in WHERE order, the SELECT reading otel_logs does not run %q after the markers before it %q: %s", marker, tc.inOrder, where)
				}
				rest = rest[at+len(marker):]
			}
		})
	}
}

// TestRangeAggregationOvertakingFilterThatReadsNoNewColumnLeavesTheOrder
// pins that in WHERE order a filter left above the scan is not brought
// ahead of a later moved filter that reads only the table columns it
// already reads: the later one spares no column read by running after it,
// so a regex alternation the expansion bound leaves above keeps running
// after the equality that moved.
func TestRangeAggregationOvertakingFilterThatReadsNoNewColumnLeavesTheOrder(t *testing.T) {
	t.Parallel()
	alternation := make([]string, 12)
	for i := range alternation {
		alternation[i] = `status=~"` + strconv.Itoa(i) + `.*"`
	}
	query := `sum(count_over_time({job="wide"} | logfmt | ` + strings.Join(alternation, " or ") + ` | method="GET" [5m]))`
	sql, _ := rangeAggregationSQLUnder(t, query, chplan.SchemaStrategies{PrewhereInWhereOrder: true})
	scope := scopeAround(t, sql, "FROM `otel_logs`")
	where := scope[strings.Index(scope, "FROM `otel_logs`"):]
	if strings.Contains(where, "match(") {
		t.Errorf("in WHERE order, the SELECT reading otel_logs runs the status alternation, which reads no column the method filter does not: %s", where)
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
		{`quantile_over_time(0.9, {service_name="web-server"} | json | status >= 400 | unwrap duration(duration) [1h]) by (method)`, []string{"_logql_labels_1", "_logql_unwrap_stripped"}},
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

// TestRangeAggregationWholeMapReadsStayAboveTheScan pins that a map the
// stage reads as a whole keeps the scan's scope from reading it again by
// key: a label filter on a stored label, which reads LogAttributes whole
// through mapContains, stays above the scan when no line filter runs in the
// scan's filter, and so does the
// detected_level source; a range aggregation whose grouping label reads
// LogAttributes whole leaves the source above the scan too, and one that
// groups by detected_level alone moves it into the scan's scope. It holds
// on a server that keeps WHERE order as on one that does not: with no
// ordered prefix, no place in the stack lets a value that reads such a map
// by key move, the first two included.
func TestRangeAggregationWholeMapReadsStayAboveTheScan(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query       string
		scanDefines bool
	}{
		{`sum(count_over_time({job="wide"} | tenant="tenant-7" [5m]))`, false},
		{`count_over_time({job="wide"} | tenant="tenant-7" [5m])`, false},
		{`sum(rate({job="wide"} | tenant=~"tenant-7|tenant-8" [1m]))`, false},
		{`sum by (env) (rate({job="api"}[5m]))`, false},
		{`sum by (detected_level) (count_over_time({job="api"}[5m]))`, true},
	} {
		for _, whereOrder := range []bool{false, true} {
			t.Run(tc.query+"/where order "+strconv.FormatBool(whereOrder), func(t *testing.T) {
				t.Parallel()
				sql, _ := rangeAggregationSQLUnder(t, tc.query, chplan.SchemaStrategies{PrewhereInWhereOrder: whereOrder})
				if !strings.Contains(sql, "AS `_logql_level_source`") {
					t.Fatalf("the detected_level source is not defined: %s", sql)
				}
				scope := scopeAround(t, sql, "FROM `otel_logs`")
				if strings.Contains(scope, "mapContains(`LogAttributes`") {
					t.Errorf("the SELECT reading otel_logs reads LogAttributes whole: %s", scope)
				}
				if got := strings.Contains(scope, "AS `_logql_level_source`"); got != tc.scanDefines {
					t.Errorf("the SELECT reading otel_logs defines the detected_level source = %v, want %v: %s", got, tc.scanDefines, scope)
				}
			})
		}
	}
}

// TestRangeAggregationWholeMapFilterLeadsALineFilterOnAReorderingServer
// pins that on a server that orders a multi-step PREWHERE itself, a label
// filter that reads LogAttributes whole runs in the SELECT that reads
// otel_logs when a line filter, written before or after it, runs there:
// ClickHouse then runs both in PREWHERE and reads the columns the stage
// reads past them only for the granules both keep. The detected_level
// source stays above the scan.
func TestRangeAggregationWholeMapFilterLeadsALineFilterOnAReorderingServer(t *testing.T) {
	t.Parallel()
	const tenant, line = "mapContains(`LogAttributes`, ?)", "position(`Body`"
	for _, query := range []string{
		`sum(count_over_time({job="wide"} | tenant="tenant-7" |= "orders/5" [5m]))`,
		`sum(count_over_time({service_name="tempo", pod="pod-8"} | tenant="tenant-7" |= "status=500" [1h]))`,
		`sum(count_over_time({job="wide"} |= "orders/5" | tenant!="tenant-7" [5m]))`,
		`sum(count_over_time({job="wide"} |= "orders/5" | detected_level="error" | tenant="tenant-7" [5m]))`,
		`sum(count_over_time({job="wide"} | detected_level="error" | tenant="tenant-7" |= "orders/5" [5m]))`,
		`sum by (detected_level) (count_over_time({job="wide"} | detected_level=~"err.*" | tenant!="tenant-7" |= "orders/5" [5m]))`,
	} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			sql, _ := rangeAggregationSQL(t, query)
			scope := scopeAround(t, sql, "FROM `otel_logs`")
			if !strings.Contains(scope, tenant) || !strings.Contains(scope, line) {
				t.Errorf("on a reordering server, the SELECT reading otel_logs does not read both %s and %s: %s", tenant, line, scope)
			}
			if strings.Contains(scope, "AS `_logql_level_source`") {
				t.Errorf("on a reordering server, the SELECT reading otel_logs defines the level source: %s", scope)
			}
		})
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

// levelFilterShapes are range aggregations with every kind of
// detected_level matcher a range aggregation lowers over the level source
// in WHERE order.
var levelFilterShapes = []string{
	`sum(count_over_time({job="jsonapp"} | detected_level="unknown" [5m]))`,
	`sum(count_over_time({job="jsonapp"} | detected_level="info" or detected_level="unknown" [5m]))`,
	`sum(count_over_time({job="jsonapp"} | detected_level!="info" [5m]))`,
	`sum(count_over_time({job="jsonapp"} | detected_level=~"unknown" [5m]))`,
	`sum(count_over_time({job="jsonapp"} | detected_level=~"info|unknown" [5m]))`,
	`sum(count_over_time({job="jsonapp"} | detected_level!~"unknown" [5m]))`,
	`sum(count_over_time({job="wide"} | detected_level=~"err.*" |= "error" [5m]))`,
}

// TestRangeAggregationLevelFilterReadsTheNormalisedLevelOnAReorderingServer
// pins that a range aggregation's detected_level filter reads the
// normalised level, not the level source, unless every server runs a
// multi-step PREWHERE in WHERE order. ClickHouse moves the filter to
// PREWHERE either way; over the source, the scan hands on the source
// rather than the level, and on 26.8 a line without a level gives an empty
// source, so the reader sizes its blocks for a narrower row and an
// aggregation whose filter keeps every row peaks at about twice the
// memory. In WHERE order the filter reads the source, so that it runs in
// the SELECT that reads otel_logs ahead of a line filter written after
// it.
func TestRangeAggregationLevelFilterReadsTheNormalisedLevelOnAReorderingServer(t *testing.T) {
	t.Parallel()
	const source, level = "lower(`_logql_level_source`))", "`_logql_detected_level`"
	for _, query := range levelFilterShapes {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			sql, _ := rangeAggregationSQLUnder(t, query, chplan.SchemaStrategies{})
			if strings.Contains(sql, source) {
				t.Errorf("on a reordering server, a filter reads the level source: %s", sql)
			}
			if !levelFilterReads(sql, level) {
				t.Errorf("on a reordering server, the level filter does not read %s: %s", level, sql)
			}
			sql, _ = rangeAggregationSQLUnder(t, query, chplan.SchemaStrategies{PrewhereInWhereOrder: true})
			if !strings.Contains(sql, source) {
				t.Errorf("in WHERE order, the level filter does not read the level source: %s", sql)
			}
		})
	}
}

// levelFilterReads reports whether a WHERE of sql opens on a predicate
// over column: a comparison, or the UTF-8 guard around a regex match.
func levelFilterReads(sql, column string) bool {
	return regexp.MustCompile(`WHERE \(*(if\(isValidUTF8\()?` + regexp.QuoteMeta(column)).MatchString(sql)
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

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

// TestRangeAggregationRowStageStaysNested pins that a range aggregation's
// per-row stage renders one SELECT per layer: no Project of its plan is
// marked OneScopeRows, and the SELECT that defines the first labels map
// reads it nowhere else. In one SELECT, ClickHouse's analyzer copies an
// alias's expression into every place that reads it, and the labels maps
// of these shapes read one another many times over.
func TestRangeAggregationRowStageStaysNested(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		`max_over_time({service_name="worker"} | json | bytes > 100 | unwrap bytes [1h]) by (tenant)`,
		`avg_over_time({service_name="web-server"} | json | status >= 400 | unwrap duration(duration) [1h]) by (method)`,
		`quantile_over_time(0.9, {service_name="web-server"} | json | status >= 400 | unwrap duration(duration) [1h]) by (method)`,
		`sum by (level) (count_over_time({service_name="api"} | logfmt | level=~"error|warn" [1h]))`,
	} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			start := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
			l := &logql.Lang{Schema: schema.DefaultOTelLogs(), Start: start, End: start.Add(24 * time.Hour), Step: time.Hour}
			dr, err := (&engine.Engine{Optimizer: optimizer.Default()}).DryRunSQL(context.Background(), l, q)
			if err != nil {
				t.Fatalf("DryRunSQL: %v", err)
			}
			if marked := oneScopeRowsProjects(dr.Plan); marked > 0 {
				t.Errorf("%d Project(s) of the plan are marked OneScopeRows, want none", marked)
			}
			if definingScopeReads(t, dr.SQL, "_logql_labels_0") {
				t.Errorf("the SELECT defining _logql_labels_0 also reads it: %s", dr.SQL)
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

// oneScopeRowsProjects counts the Projects of plan marked OneScopeRows.
func oneScopeRowsProjects(plan chplan.Node) int {
	n := 0
	if p, ok := plan.(*chplan.Project); ok && p.OneScopeRows {
		n++
	}
	for _, child := range plan.Children() {
		n += oneScopeRowsProjects(child)
	}
	return n
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
	open, depth := -1, 0
	for i := def; i >= 0; i-- {
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
		t.Fatalf("the definition of %s is not inside a subquery: %s", alias, sql)
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
	return strings.Count(scope.String(), quoted) > 1
}

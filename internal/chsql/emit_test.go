package chsql_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/test/spec"
)

// fixtureDir is resolved relative to this test file.
var fixtureDir = filepath.Join("..", "..", "test", "spec", "chsql")

// plans maps fixture base name → the chplan tree to emit for that fixture.
// Adding a new spec case is:
//  1. Add an entry here.
//  2. Run `GOLDEN_UPDATE=1 just test-spec` (creates the fixture).
//  3. Inspect `git diff test/spec/chsql/` and commit.
var plans = map[string]chplan.Node{
	"scan_basic": &chplan.Scan{Table: "otel_metrics_gauge"},
	"scan_columns": &chplan.Scan{
		Table:   "otel_metrics_gauge",
		Columns: []string{"TimeUnix", "Value", "Attributes"},
	},

	"filter_eq": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "http_requests_total"},
		},
	},
	"filter_match": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpMatch,
			Left:  &chplan.ColumnRef{Name: "ResourceAttributes_service_name"},
			Right: &chplan.LitString{V: "^api-.*"},
		},
	},
	"filter_and": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op: chplan.OpAnd,
			Left: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "MetricName"},
				Right: &chplan.LitString{V: "http_requests_total"},
			},
			Right: &chplan.Binary{
				Op:    chplan.OpGt,
				Left:  &chplan.ColumnRef{Name: "Value"},
				Right: &chplan.LitFloat{V: 0.5},
			},
		},
	},

	"project_alias": &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "t"},
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "v"},
		},
	},

	"aggregate_sum_by": &chplan.Aggregate{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: "job"},
		},
		AggFuncs: []chplan.AggFunc{
			{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "total"},
		},
	},

	"range_window_rate": &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	},
	"range_window_increase": &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "increase",
		Range:           10 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	},
	"range_window_sum_over_time": &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Func:            "sum_over_time",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	},
	"range_window_avg_over_time": &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Func:            "avg_over_time",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	},
	"range_window_max_over_time": &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Func:            "max_over_time",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	},
	"range_window_count_over_time": &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Func:            "count_over_time",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	},

	"limit_basic": &chplan.Limit{
		Input: &chplan.Scan{Table: "otel_logs"},
		Count: 1000,
	},

	"filter_map_access": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op: chplan.OpEq,
			Left: &chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: "Attributes"},
				Key: &chplan.LitString{V: "job"},
			},
			Right: &chplan.LitString{V: "api"},
		},
	},
}

func TestEmit(t *testing.T) {
	t.Parallel()

	// Every plan in the map must have a fixture; every fixture must have a plan.
	spec.Walk(t, fixtureDir, func(t *testing.T, c *spec.Case) {
		plan, ok := plans[c.Name]
		if !ok {
			t.Fatalf("no plan registered for fixture %s; add it to plans in emit_test.go", c.Name)
		}
		sql, args, err := chsql.Emit(plan)
		if err != nil {
			t.Fatalf("Emit failed: %v", err)
		}
		spec.Match(t, c, map[string]string{
			"sql":  sql,
			"args": formatArgs(args),
		})
	})

	// Catch fixtures that were deleted but their plan map entry wasn't.
	missing := plansWithoutFixtures(t)
	if len(missing) > 0 {
		t.Errorf("plans without fixtures (run with GOLDEN_UPDATE=1 to create): %v", missing)
	}
}

func plansWithoutFixtures(t *testing.T) []string {
	t.Helper()
	var missing []string
	for name := range plans {
		path := filepath.Join(fixtureDir, name+".txtar")
		if _, err := spec.Load(path); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

func formatArgs(args []any) string {
	if len(args) == 0 {
		return "(none)\n"
	}
	var b strings.Builder
	for i, a := range args {
		fmt.Fprintf(&b, "[%d] %T = %#v\n", i, a, a)
	}
	return b.String()
}

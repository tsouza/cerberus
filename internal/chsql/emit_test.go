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

	// OrderBy — single-key DESC. Tempo `/api/search/recent` lowers to
	// Limit(OrderBy(Scan, Timestamp DESC), N).
	"order_by_timestamp_desc": &chplan.Limit{
		Input: &chplan.OrderBy{
			Input: &chplan.Scan{Table: "otel_traces"},
			Keys: []chplan.OrderKey{
				{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true},
			},
		},
		Count: 20,
	},
	// OrderBy — two-key (composite sort).
	"order_by_composite": &chplan.OrderBy{
		Input: &chplan.Scan{Table: "otel_traces"},
		Keys: []chplan.OrderKey{
			{Expr: &chplan.ColumnRef{Name: "ServiceName"}, Desc: false},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Desc: true},
		},
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

	// VectorJoin — PromQL `on(...)` vector matching.
	"vector_join_on_job": &chplan.VectorJoin{
		Left:             &chplan.Scan{Table: "otel_metrics_gauge"},
		Right:            &chplan.Scan{Table: "otel_metrics_sum"},
		Op:               chplan.OpAdd,
		Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	},
	// VectorJoin — PromQL `ignoring(...)` vector matching.
	"vector_join_ignoring_instance": &chplan.VectorJoin{
		Left:             &chplan.Scan{Table: "otel_metrics_gauge"},
		Right:            &chplan.Scan{Table: "otel_metrics_sum"},
		Op:               chplan.OpSub,
		Match:            chplan.VectorMatch{Labels: []string{"instance"}, On: false},
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	},

	// StructuralJoin — TraceQL `>` (parent_of).
	"structural_join_child": &chplan.StructuralJoin{
		Left:               &chplan.Scan{Table: "otel_traces"},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 chplan.StructuralChild,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
	},
	// StructuralJoin — TraceQL `<` (child_of).
	"structural_join_parent": &chplan.StructuralJoin{
		Left:               &chplan.Scan{Table: "otel_traces"},
		Right:              &chplan.Scan{Table: "otel_traces"},
		Op:                 chplan.StructuralParent,
		TraceIDColumn:      "TraceId",
		SpanIDColumn:       "SpanId",
		ParentSpanIDColumn: "ParentSpanId",
	},

	// MapWithoutKeys used inside an Aggregate group-key: PromQL `without`.
	"aggregate_sum_without": &chplan.Aggregate{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		GroupBy: []chplan.Expr{
			&chplan.MapWithoutKeys{
				Map:  &chplan.ColumnRef{Name: "Attributes"},
				Keys: []string{"instance", "pod"},
			},
		},
		AggFuncs: []chplan.AggFunc{
			{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "total"},
		},
	},

	// Aggregate with parameterised CH aggregate: quantile(0.95)(value).
	"aggregate_quantile_param": &chplan.Aggregate{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: "Attributes"},
		},
		AggFuncs: []chplan.AggFunc{
			{
				Name:   "quantile",
				Params: []chplan.Expr{&chplan.LitFloat{V: 0.95}},
				Args:   []chplan.Expr{&chplan.ColumnRef{Name: "Value"}},
				Alias:  "p95",
			},
		},
	},

	// LineContent variants (LogQL line filters).
	"filter_line_contains": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_logs"},
		Predicate: &chplan.LineContent{
			Source:  &chplan.ColumnRef{Name: "Body"},
			Pattern: "ERROR",
		},
	},
	"filter_line_not_contains": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_logs"},
		Predicate: &chplan.LineContent{
			Source:  &chplan.ColumnRef{Name: "Body"},
			Pattern: "DEBUG",
			Negated: true,
		},
	},
	"filter_line_regex": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_logs"},
		Predicate: &chplan.LineContent{
			Source:  &chplan.ColumnRef{Name: "Body"},
			Pattern: `failed: \w+`,
			IsRegex: true,
		},
	},
	"filter_line_not_regex": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_logs"},
		Predicate: &chplan.LineContent{
			Source:  &chplan.ColumnRef{Name: "Body"},
			Pattern: `health.*ok`,
			IsRegex: true,
			Negated: true,
		},
	},

	// RangeWindow with offset modifier (PromQL `rate(...)[5m] offset 1h`).
	"range_window_rate_offset": &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		Offset:          time.Hour,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	},

	// RangeWindow with the LogQL-specific log_rate function.
	"range_window_log_rate": &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_logs"},
		Func:            "log_rate",
		Range:           5 * time.Minute,
		TimestampColumn: "Timestamp",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "ResourceAttributes"}},
	},

	// Matrix-shape RangeWindow — emits one row per anchor across
	// [End-OuterRange, End] spaced by Step. Used by PromQL subqueries
	// `rate(m[5m])[1h:5m]` (P0 #4).
	"range_window_matrix_rate": &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            5 * time.Minute,
		OuterRange:      time.Hour,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	},

	// Matrix-shape RangeWindow + sum_over_time (the inner reducer in
	// the canonical `max_over_time(rate(...)[1h:5m])` shape).
	"range_window_matrix_sum_over_time": &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Func:            "sum_over_time",
		Range:           5 * time.Minute,
		Step:            time.Minute,
		OuterRange:      30 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	},

	// Identity-flagged RangeWindow — the "last value in window" shape
	// used by bare-vector subqueries (`up[5m:1m]`).
	"range_window_identity": &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Identity:        true,
		Range:           time.Minute,
		Step:            time.Minute,
		OuterRange:      5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	},

	// FieldAccess — TraceQL dotted attribute access.
	"filter_field_access": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_traces"},
		Predicate: &chplan.Binary{
			Op: chplan.OpEq,
			Left: &chplan.FieldAccess{
				Source: &chplan.ColumnRef{Name: "SpanAttributes"},
				Path:   "http.status_code",
			},
			Right: &chplan.LitInt{V: 500},
		},
	},

	// FuncCall — generic CH function expression (length on a column).
	"project_func_call_length": &chplan.Project{
		Input: &chplan.Scan{Table: "otel_logs"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Alias: "t"},
			{
				Expr: &chplan.FuncCall{
					Name: "length",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}},
				},
				Alias: "body_bytes",
			},
		},
	},

	// nested_filters_no_fuse: two explicit Filter wrappers should emit
	// nested subqueries; the optimizer's filter-fusion only fires when
	// the driver runs over the tree. At the chsql layer, the IR is
	// emitted as-is.
	"nested_filters_no_fuse": &chplan.Filter{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Predicate: &chplan.Binary{
				Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "MetricName"}, Right: &chplan.LitString{V: "up"},
			},
		},
		Predicate: &chplan.Binary{
			Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "job"}, Right: &chplan.LitString{V: "api"},
		},
	},

	// project_on_aggregate: outer Project rewrapping an Aggregate's
	// output. Common pattern after the wrap-sample projection in
	// api/prom/handler.go.
	"project_on_aggregate": &chplan.Project{
		Input: &chplan.Aggregate{
			Input:   &chplan.Scan{Table: "otel_metrics_gauge"},
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			AggFuncs: []chplan.AggFunc{
				{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "total"},
			},
		},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Attributes"}, Alias: "attrs"},
			{Expr: &chplan.ColumnRef{Name: "total"}, Alias: "v"},
		},
	},

	// filter_on_aggregate: HAVING-shape. Filter sits on top of an
	// Aggregate's grouped output. The scalar-filter lowering for TraceQL's
	// `| count() > 0` produces this shape.
	"filter_on_aggregate": &chplan.Filter{
		Input: &chplan.Aggregate{
			Input:   &chplan.Scan{Table: "otel_metrics_gauge"},
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			AggFuncs: []chplan.AggFunc{
				{Name: "count", Args: []chplan.Expr{&chplan.LitInt{V: 1}}, Alias: "Value"},
			},
		},
		Predicate: &chplan.Binary{
			Op: chplan.OpGt, Left: &chplan.ColumnRef{Name: "Value"}, Right: &chplan.LitInt{V: 0},
		},
	},

	// MetricsAggregate (bare emission, no wrapping RangeWindow) — the
	// TraceQL instant-metric shape. SQL is byte-equivalent to a plain
	// chplan.Aggregate with the per-Op CH function name.
	"metrics_aggregate_rate_bare": &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpRate,
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	},
	"metrics_aggregate_sum_over_time_bare": &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpSumOverTime,
		Attr:       &chplan.ColumnRef{Name: "Duration"},
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	},
	"metrics_aggregate_quantile_over_time_bare": &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpQuantileOverTime,
		Attr:       &chplan.ColumnRef{Name: "Duration"},
		Quantiles:  []float64{0.95},
		ValueAlias: "Value",
		Inner:      &chplan.Scan{Table: "otel_traces"},
	},

	// RangeWindow wrapping MetricsAggregate — the matrix shape used
	// by TraceQL's /api/metrics/query_range handler. Each per-span row
	// is fanned across the N evaluation anchors via arrayJoin(range())
	// and the outer SELECT applies the Op-specific reducer per
	// (group-by, anchor) bucket.
	"range_window_metrics_rate": &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpRate,
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Range:           5 * time.Minute,
		Step:            time.Minute,
		OuterRange:      time.Hour,
		TimestampColumn: "Timestamp",
	},
	"range_window_metrics_count_over_time_by": &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:             chplan.MetricsOpCountOverTime,
			GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}},
			GroupByAliases: []string{"service"},
			ValueAlias:     "Value",
			Inner:          &chplan.Scan{Table: "otel_traces"},
		},
		Step:            time.Minute,
		OuterRange:      10 * time.Minute,
		TimestampColumn: "Timestamp",
	},
	"range_window_metrics_sum_over_time_attr": &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpSumOverTime,
			Attr:       &chplan.ColumnRef{Name: "Duration"},
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Step:            time.Minute,
		OuterRange:      5 * time.Minute,
		TimestampColumn: "Timestamp",
	},
	"range_window_metrics_quantile_over_time_attr": &chplan.RangeWindow{
		Input: &chplan.MetricsAggregate{
			Op:         chplan.MetricsOpQuantileOverTime,
			Attr:       &chplan.ColumnRef{Name: "Duration"},
			Quantiles:  []float64{0.95},
			ValueAlias: "Value",
			Inner:      &chplan.Scan{Table: "otel_traces"},
		},
		Step:            time.Minute,
		OuterRange:      5 * time.Minute,
		TimestampColumn: "Timestamp",
	},

	// vector_join_set_and: VectorJoin with OpAnd (set-intersection
	// shape). Unusual operator on this node but the IR allows it.
	"vector_join_set_and": &chplan.VectorJoin{
		Left:             &chplan.Scan{Table: "otel_metrics_gauge"},
		Right:            &chplan.Scan{Table: "otel_metrics_sum"},
		Op:               chplan.OpAnd,
		Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	},

	// func_call_zero_args: CH function with no arguments — `now()`.
	"project_func_call_zero_args": &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{Expr: &chplan.FuncCall{Name: "now"}, Alias: "current_time"},
		},
	},

	// map_access_special_key: MapAccess key containing a literal dot
	// and underscores — common shape for OTel `service.name`,
	// `http.status_code`, etc. The emitter must bind the key as a
	// parameter (not splice into the SQL), so CH-special chars in the
	// key are no-op.
	"filter_map_access_dotted_key": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_traces"},
		Predicate: &chplan.Binary{
			Op: chplan.OpEq,
			Left: &chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: "SpanAttributes"},
				Key: &chplan.LitString{V: "http.status_code"},
			},
			Right: &chplan.LitInt{V: 200},
		},
	},

	// deeply_nested_binary: 4-level nested Binary tree. Verifies
	// emitBinary parenthesizes correctly across multiple depths.
	"filter_deeply_nested_binary": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op: chplan.OpAnd,
			Left: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "MetricName"},
				Right: &chplan.LitString{V: "up"},
			},
			Right: &chplan.Binary{
				Op: chplan.OpOr,
				Left: &chplan.Binary{
					Op: chplan.OpAnd,
					Left: &chplan.Binary{
						Op:    chplan.OpEq,
						Left:  &chplan.ColumnRef{Name: "job"},
						Right: &chplan.LitString{V: "api"},
					},
					Right: &chplan.Binary{
						Op:    chplan.OpGt,
						Left:  &chplan.ColumnRef{Name: "Value"},
						Right: &chplan.LitFloat{V: 0.5},
					},
				},
				Right: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.ColumnRef{Name: "MetricName"},
					Right: &chplan.LitString{V: "down"},
				},
			},
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

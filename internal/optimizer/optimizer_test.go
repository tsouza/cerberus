package optimizer_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/test/spec"
)

var fixtureDir = filepath.Join("..", "..", "test", "spec", "optimizer")

// inputs maps fixture name → unoptimized plan tree. The fixture records
// both the unoptimized SQL (sanity check that the input was lowered as
// expected) and the optimized SQL (the actual rule output).
var inputs = map[string]chplan.Node{
	// filter_fusion: Filter(Filter(scan, p1), p2) should fold to a single
	// Filter with `p1 AND p2`.
	"filter_fusion": &chplan.Filter{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Predicate: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "MetricName"},
				Right: &chplan.LitString{V: "up"},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "job"},
			Right: &chplan.LitString{V: "api"},
		},
	},

	// constant_fold_and: `true AND X` → X, so the Filter's predicate drops
	// to the metric-name comparison alone.
	"constant_fold_and": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op:   chplan.OpAnd,
			Left: &chplan.LitBool{V: true},
			Right: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "MetricName"},
				Right: &chplan.LitString{V: "up"},
			},
		},
	},

	// constant_fold_arith: `1 + 2 = 3` collapses to `LitBool(true)` (an
	// always-true predicate), and the surrounding `LitBool(true) AND X`
	// then collapses to `X`. Demonstrates rule composition.
	"constant_fold_arith": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op: chplan.OpAnd,
			Left: &chplan.Binary{
				Op: chplan.OpEq,
				Left: &chplan.Binary{
					Op:    chplan.OpAdd,
					Left:  &chplan.LitInt{V: 1},
					Right: &chplan.LitInt{V: 2},
				},
				Right: &chplan.LitInt{V: 3},
			},
			Right: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "MetricName"},
				Right: &chplan.LitString{V: "up"},
			},
		},
	},

	// projection_pushdown: Project([Value, TimeUnix], Scan(table, *))
	// should narrow the Scan's column list.
	"projection_pushdown": &chplan.Project{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "v"},
			{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "t"},
		},
	},

	// filter_fusion_after_constant_fold: outer predicate is
	// `LitBool(true) AND <real>` — constant-fold reduces to `<real>`,
	// then filter-fusion can run on the result. Tests that the
	// driver re-applies rules after a change (fixpoint behaviour).
	"filter_fusion_after_constant_fold": &chplan.Filter{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Predicate: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "MetricName"},
				Right: &chplan.LitString{V: "up"},
			},
		},
		Predicate: &chplan.Binary{
			Op:   chplan.OpAnd,
			Left: &chplan.LitBool{V: true},
			Right: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "job"},
				Right: &chplan.LitString{V: "api"},
			},
		},
	},

	// nested_filter_fold: three-deep Filter(Filter(Filter(Scan)))
	// with non-trivial predicates. Verifies fusion runs to fixpoint
	// (collapses all three Filters into one AND-of-three).
	"nested_filter_fold": &chplan.Filter{
		Input: &chplan.Filter{
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
		Predicate: &chplan.Binary{
			Op: chplan.OpGt, Left: &chplan.ColumnRef{Name: "Value"}, Right: &chplan.LitFloat{V: 0},
		},
	},

	// constant_fold_idempotent: an already-folded predicate (no
	// constants to reduce) — the fixture records optimized SQL
	// byte-identical to unoptimized. Sanity check that the driver
	// doesn't rewrite already-optimal plans into something
	// semantically equivalent-but-different.
	"constant_fold_idempotent": &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_gauge"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "MetricName"},
			Right: &chplan.LitString{V: "up"},
		},
	},

	// filter_aggregate_transpose_passes: Filter over Aggregate where
	// the predicate references a bare group-by column (`job`). The
	// FilterAggregateTranspose rule pushes the Filter under the
	// Aggregate; the optimized SQL shows the WHERE clause inside the
	// GROUP BY's FROM subquery.
	"filter_aggregate_transpose_passes": &chplan.Filter{
		Input: &chplan.Aggregate{
			Input:   &chplan.Scan{Table: "otel_metrics_gauge"},
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
			AggFuncs: []chplan.AggFunc{
				{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "job"},
			Right: &chplan.LitString{V: "api"},
		},
	},

	// filter_aggregate_transpose_blocked: predicate touches the
	// aggregate-output column `sum_value`, which doesn't exist in the
	// Aggregate's input. The transpose rule declines; optimized SQL is
	// byte-identical to unoptimized.
	"filter_aggregate_transpose_blocked": &chplan.Filter{
		Input: &chplan.Aggregate{
			Input:   &chplan.Scan{Table: "otel_metrics_gauge"},
			GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
			AggFuncs: []chplan.AggFunc{
				{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "sum_value"},
			Right: &chplan.LitFloat{V: 0},
		},
	},

	// filter_range_window_transpose_passes: Filter over RangeWindow
	// where the predicate references a bare series-identifying group
	// key (`Attributes`). The FilterRangeWindowTranspose rule pushes
	// the Filter under the RangeWindow; the optimized SQL shows the WHERE clause inside the
	// arraySort/groupArray subquery, before the windowed aggregation.
	"filter_range_window_transpose_passes": &chplan.Filter{
		Input: &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            "rate",
			Range:           5 * time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		},
		Predicate: &chplan.Binary{
			Op: chplan.OpEq,
			Left: &chplan.MapAccess{
				Map: &chplan.ColumnRef{Name: "Attributes"},
				Key: &chplan.LitString{V: "job"},
			},
			Right: &chplan.LitString{V: "api"},
		},
	},

	// filter_range_window_transpose_blocked: predicate touches the
	// per-sample `Value` column, which becomes the windowed-function
	// input (and is not in the RangeWindow's passthrough series-key
	// set). The transpose rule declines; optimized SQL is byte-identical
	// to unoptimized.
	"filter_range_window_transpose_blocked": &chplan.Filter{
		Input: &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            "rate",
			Range:           5 * time.Minute,
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpGt,
			Left:  &chplan.ColumnRef{Name: "Value"},
			Right: &chplan.LitFloat{V: 0},
		},
	},

	// filter_range_window_transpose_logql: LogQL-style shape with a
	// bare-column series identity (`ServiceName`) rather than the
	// OTel-CH map; the rule pushes the Filter under the RangeWindow
	// when the predicate sticks to that column.
	"filter_range_window_transpose_logql": &chplan.Filter{
		Input: &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_logs"},
			Func:            "rate",
			Range:           5 * time.Minute,
			TimestampColumn: "Timestamp",
			ValueColumn:     "BodyBytes",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "ServiceName"},
			Right: &chplan.LitString{V: "api"},
		},
	},

	// subquery_matrix_opaque: a matrix-shape RangeWindow with a
	// FilterFusion-friendly nested Filter underneath. The optimizer
	// should fuse the inner two Filters but NOT alter the matrix
	// RangeWindow itself — the OuterRange / Step / Identity fields
	// are preserved, the inner Scan is reachable, and the SQL stays
	// structurally equivalent except for the inner filter fold.
	// Regression test for P0 4.3+ optimizer-vs-matrix interaction.
	"subquery_matrix_opaque": &chplan.RangeWindow{
		Input: &chplan.Filter{
			Input: &chplan.Filter{
				Input: &chplan.Scan{Table: "otel_metrics_sum"},
				Predicate: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.ColumnRef{Name: "MetricName"},
					Right: &chplan.LitString{V: "http_requests_total"},
				},
			},
			Predicate: &chplan.Binary{
				Op: chplan.OpEq,
				Left: &chplan.MapAccess{
					Map: &chplan.ColumnRef{Name: "Attributes"},
					Key: &chplan.LitString{V: "job"},
				},
				Right: &chplan.LitString{V: "api"},
			},
		},
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            5 * time.Minute,
		OuterRange:      time.Hour,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	},

	// pushdown_through_filter: Project(Filter(Scan)) — pushdown's
	// widened pattern narrows the inner Scan's column list to the
	// union of refs(Projections) ∪ refs(Filter.Predicate). The
	// Filter stays in place between the (narrowed) Scan and the
	// Project. Locks the v0.2 widening of ProjectionPushdown that
	// lets it see through an intervening Filter.
	"pushdown_through_filter": &chplan.Project{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Predicate: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "MetricName"},
				Right: &chplan.LitString{V: "up"},
			},
		},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "v"},
			{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "t"},
		},
	},

	// pushdown_select_wrap_carriers: the Tempo /api/search
	// `| select(...)` wrap shape — Project(Filter(Scan)) where the
	// selected attribute values ride INSIDE the canonical Attributes
	// map() expression as FieldAccess lookups on their carrier maps
	// (SpanAttributes here), and the predicate carries a
	// NestedArrayExists whose carrier (`Events`) is a plain string
	// field, not a child ColumnRef. ProjectionPushdown's narrowed
	// Scan.Columns must retain BOTH carriers: before the walker
	// learned FieldAccess / NestedArrayExists, SpanAttributes and
	// Events were pruned and ClickHouse failed outer-scope resolution
	// with error 47 (UNKNOWN_IDENTIFIER) — the compose-smoke 502 on
	// `{ status = error } | select(span.http.method, ...)`.
	"pushdown_select_wrap_carriers": &chplan.Project{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: "otel_traces"},
			Predicate: &chplan.Binary{
				Op: chplan.OpAnd,
				Left: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.ColumnRef{Name: "StatusCode"},
					Right: &chplan.LitString{V: "Error"},
				},
				Right: &chplan.NestedArrayExists{
					Column:   "Events",
					SubField: "Attributes",
					Key:      "exception.type",
					Op:       chplan.OpEq,
					Value:    &chplan.LitString{V: "IOError"},
				},
			},
		},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "SpanName"}, Alias: "MetricName"},
			{Expr: &chplan.FuncCall{Name: "mapConcat", Args: []chplan.Expr{
				&chplan.ColumnRef{Name: "ResourceAttributes"},
				&chplan.FuncCall{Name: "map", Args: []chplan.Expr{
					&chplan.LitString{V: "__cerberus_sel_str_http.method"},
					&chplan.FieldAccess{
						Source: &chplan.ColumnRef{Name: "SpanAttributes"},
						Path:   "http.method",
					},
				}},
			}}, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Alias: "TimeUnix"},
			{Expr: &chplan.FuncCall{
				Name: "toFloat64",
				Args: []chplan.Expr{&chplan.ColumnRef{Name: "Duration"}},
			}, Alias: "Value"},
		},
	},
}

func TestOptimizer(t *testing.T) {
	t.Parallel()

	spec.Walk(t, fixtureDir, func(t *testing.T, c *spec.Case) {
		input, ok := inputs[c.Name]
		if !ok {
			t.Fatalf("no plan registered for fixture %s; add it to optimizer_test.go", c.Name)
		}

		unoptSQL, _, err := chsql.Emit(context.Background(), input)
		if err != nil {
			t.Fatalf("Emit unoptimized: %v", err)
		}

		opt := optimizer.Default().Run(context.Background(), input)
		optSQL, _, err := chsql.Emit(context.Background(), opt)
		if err != nil {
			t.Fatalf("Emit optimized: %v", err)
		}

		spec.Match(t, c, map[string]string{
			"unoptimized":        unoptSQL,
			"optimized":          optSQL,
			"chplan_unoptimized": spec.PrintChplan(input),
			"chplan_optimized":   spec.PrintChplan(opt),
		})
	})
}

func TestDriver_FixpointTerminates(t *testing.T) {
	t.Parallel()

	// A pathological plan with deeply nested redundant filters. Each pass
	// of FilterFusion collapses one level; the driver should converge in
	// at most N+1 iterations and not hit the iteration cap.
	plan := chplan.Node(&chplan.Scan{Table: "otel_metrics_gauge"})
	const depth = 12
	for i := 0; i < depth; i++ {
		plan = &chplan.Filter{
			Input:     plan,
			Predicate: &chplan.LitBool{V: true},
		}
	}

	out := optimizer.Default().Run(context.Background(), plan)
	// After ConstantFoldHeuristic + FilterFusion converges, the
	// deeply-stacked `LitBool(true)` predicates collapse and the
	// Filters fuse — we expect a single Filter(Scan) (a non-trivial
	// predicate would normally remain here; with all-true predicates
	// the Filter itself becomes redundant but our seed rules don't yet
	// drop trivially-true Filters).
	switch v := out.(type) {
	case *chplan.Filter:
		if _, ok := v.Input.(*chplan.Scan); !ok {
			t.Fatalf("expected Filter(Scan), got Filter(%T)", v.Input)
		}
	default:
		t.Fatalf("expected Filter(Scan), got %T", out)
	}
}

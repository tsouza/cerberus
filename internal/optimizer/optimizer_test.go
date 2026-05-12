package optimizer_test

import (
	"path/filepath"
	"testing"

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
}

func TestOptimizer(t *testing.T) {
	t.Parallel()

	spec.Walk(t, fixtureDir, func(t *testing.T, c *spec.Case) {
		input, ok := inputs[c.Name]
		if !ok {
			t.Fatalf("no plan registered for fixture %s; add it to optimizer_test.go", c.Name)
		}

		unoptSQL, _, err := chsql.Emit(input)
		if err != nil {
			t.Fatalf("Emit unoptimized: %v", err)
		}

		opt := optimizer.Default().Run(input)
		optSQL, _, err := chsql.Emit(opt)
		if err != nil {
			t.Fatalf("Emit optimized: %v", err)
		}

		spec.Match(t, c, map[string]string{
			"unoptimized": unoptSQL,
			"optimized":   optSQL,
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

	out := optimizer.Default().Run(plan)
	// After ConstantFold + FilterFusion converges, the deeply-stacked
	// `LitBool(true)` predicates collapse and the Filters fuse — we expect
	// a single Filter(Scan) (a non-trivial predicate would normally remain
	// here; with all-true predicates the Filter itself becomes redundant
	// but our seed rules don't yet drop trivially-true Filters).
	switch v := out.(type) {
	case *chplan.Filter:
		if _, ok := v.Input.(*chplan.Scan); !ok {
			t.Fatalf("expected Filter(Scan), got Filter(%T)", v.Input)
		}
	default:
		t.Fatalf("expected Filter(Scan), got %T", out)
	}
}

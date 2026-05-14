package chsql_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/test/spec"
)

// prewhereFixtureDir is the spec directory for the PREWHERE / sort-
// key-ordering fixtures. Lives outside test/spec/chsql/ so the chsql
// emitter golden set and the codegen-rule golden set stay separable —
// late-materialisation fixtures land under test/spec/codegen/late_mat/
// on the same pattern.
var prewhereFixtureDir = filepath.Join("..", "..", "test", "spec", "codegen", "prewhere")

// prewherePlans are the chplan trees the PREWHERE fixtures pin. The trees
// are constructed bare (no optimizer pass) so the goldens reflect
// codegen behaviour exclusively.
var prewherePlans = map[string]chplan.Node{
	// `SELECT Body FROM otel_logs WHERE Timestamp > T AND SeverityNumber
	// >= 9` — the projection touches the wide `Body` column, both
	// predicates are cheap and reference no wide column, so they
	// partition into PREWHERE except the last one (kept in WHERE so
	// the rendered SQL retains a WHERE clause).
	"wide_column_excluded": &chplan.Filter{
		Input: &chplan.Scan{
			Table:   "otel_logs",
			Columns: []string{"Body"},
		},
		Predicate: &chplan.Binary{
			Op: chplan.OpAnd,
			Left: &chplan.Binary{
				Op:    chplan.OpGt,
				Left:  &chplan.ColumnRef{Name: "Timestamp"},
				Right: &chplan.LitInt{V: time.Unix(0, 0).UnixNano()},
			},
			Right: &chplan.Binary{
				Op:    chplan.OpGe,
				Left:  &chplan.ColumnRef{Name: "SeverityNumber"},
				Right: &chplan.LitInt{V: 9},
			},
		},
	},

	// Mixed conjunct shape: a cheap-no-wide predicate, a cheap-but-
	// touches-Body predicate, and a function-call predicate. The first
	// promotes to PREWHERE; the second and third stay in WHERE because
	// they touch the wide column / aren't cheap.
	"partial_promotion": &chplan.Filter{
		Input: &chplan.Scan{
			Table:   "otel_logs",
			Columns: []string{"Body", "Timestamp"},
		},
		Predicate: &chplan.Binary{
			Op: chplan.OpAnd,
			Left: &chplan.Binary{
				Op: chplan.OpAnd,
				Left: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.ColumnRef{Name: "ServiceName"},
					Right: &chplan.LitString{V: "api"},
				},
				Right: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.ColumnRef{Name: "Body"},
					Right: &chplan.LitString{V: "boom"},
				},
			},
			Right: &chplan.FuncCall{
				Name: "match",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: "Body"},
					&chplan.LitString{V: "regex"},
				},
			},
		},
	},

	// Projection has no wide column → PREWHERE not emitted; WHERE order
	// still respects sort prefix. Predicates are intentionally in
	// reverse-rank order (Timestamp before ServiceName) so the rewrite
	// has something to do.
	"no_wide_no_promotion": &chplan.Filter{
		Input: &chplan.Scan{
			Table:   "otel_logs",
			Columns: []string{"Timestamp", "SeverityText"},
		},
		Predicate: &chplan.Binary{
			Op: chplan.OpAnd,
			Left: &chplan.Binary{
				Op:    chplan.OpGt,
				Left:  &chplan.ColumnRef{Name: "Timestamp"},
				Right: &chplan.LitInt{V: 0},
			},
			Right: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "ServiceName"},
				Right: &chplan.LitString{V: "api"},
			},
		},
	},

	// Multi-conjunct WHERE, no wide columns projected, no promotion —
	// verifies the sort-prefix ordering pass alone. Input order:
	// (SeverityText, Timestamp, ServiceName); expected ordering by
	// sort-key rank: (ServiceName=0, SeverityText=1, Timestamp=2).
	"sort_prefix_order": &chplan.Filter{
		Input: &chplan.Scan{
			Table:   "otel_logs",
			Columns: []string{"Timestamp", "SeverityText", "ServiceName"},
		},
		Predicate: &chplan.Binary{
			Op: chplan.OpAnd,
			Left: &chplan.Binary{
				Op: chplan.OpAnd,
				Left: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.ColumnRef{Name: "SeverityText"},
					Right: &chplan.LitString{V: "ERROR"},
				},
				Right: &chplan.Binary{
					Op:    chplan.OpGt,
					Left:  &chplan.ColumnRef{Name: "Timestamp"},
					Right: &chplan.LitInt{V: 0},
				},
			},
			Right: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "ServiceName"},
				Right: &chplan.LitString{V: "api"},
			},
		},
	},
}

// TestEmit_Prewhere walks the PREWHERE-specific fixture set. Mirrors
// the shape of TestEmit so the golden-update loop is the same.
func TestEmit_Prewhere(t *testing.T) {
	t.Parallel()
	spec.Walk(t, prewhereFixtureDir, func(t *testing.T, c *spec.Case) {
		plan, ok := prewherePlans[c.Name]
		if !ok {
			t.Fatalf("no prewhere plan registered for fixture %s; add it to prewherePlans", c.Name)
		}
		sql, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit failed: %v", err)
		}
		spec.Match(t, c, map[string]string{
			"sql":    sql,
			"args":   formatArgs(args),
			"chplan": spec.PrintChplan(plan),
		})
	})

	// Catch fixtures that were deleted but their plan map entry wasn't.
	for name := range prewherePlans {
		path := filepath.Join(prewhereFixtureDir, name+".txtar")
		if _, err := spec.Load(path); err != nil {
			t.Errorf("prewhere plan %q has no fixture at %s (rerun with GOLDEN_UPDATE=1 to create)", name, path)
		}
	}
}

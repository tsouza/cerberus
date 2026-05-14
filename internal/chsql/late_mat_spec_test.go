package chsql_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/test/spec"
)

// lateMatFixtureDir holds the TXTAR goldens for the
// late-materialisation codegen rewrite. Kept separate from the general
// chsql/ fixture directory so the rewrite's emission shape is easy to
// scan as a group; the trigger pattern and the no-op fall-through cases
// live side by side.
var lateMatFixtureDir = filepath.Join("..", "..", "test", "spec", "codegen", "late_mat")

// lateMatPlans maps each fixture base name to its source chplan tree.
// Mirrors the pattern in emit_test.go's plans map — adding a fixture is
//  1. Add an entry here.
//  2. Run `GOLDEN_UPDATE=1 just test-spec` to materialise the fixture.
//  3. Review the diff and commit both files.
var lateMatPlans = map[string]chplan.Node{
	// logs_body_simple: the canonical case from the issue.
	// Project(Limit(Filter(Scan))) — wide column Body in projection
	// + severity predicate + LIMIT 100. Expected: two-stage SELECT
	// with the wide column fetched only after the inner thin SELECT
	// has applied the predicate and the limit.
	"logs_body_simple": &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Body"}},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
			{Expr: &chplan.ColumnRef{Name: "SeverityNumber"}},
		},
		Input: &chplan.Limit{
			Count: 100,
			Input: &chplan.Filter{
				Predicate: &chplan.Binary{
					Op:    chplan.OpGe,
					Left:  &chplan.ColumnRef{Name: "SeverityNumber"},
					Right: &chplan.LitInt{V: 9},
				},
				Input: &chplan.Scan{Table: "otel_logs"},
			},
		},
	},

	// logs_no_wide_no_rewrite: same query shape without wide columns.
	// Expected: plain single SELECT, no JOIN — the rewrite gate skips
	// because no projection ColumnRef hits the wide-column list.
	"logs_no_wide_no_rewrite": &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
			{Expr: &chplan.ColumnRef{Name: "SeverityNumber"}},
			{Expr: &chplan.ColumnRef{Name: "ServiceName"}},
		},
		Input: &chplan.Limit{
			Count: 100,
			Input: &chplan.Filter{
				Predicate: &chplan.Binary{
					Op:    chplan.OpGe,
					Left:  &chplan.ColumnRef{Name: "SeverityNumber"},
					Right: &chplan.LitInt{V: 9},
				},
				Input: &chplan.Scan{Table: "otel_logs"},
			},
		},
	},

	// logs_no_limit_no_rewrite: wide column projected but no LIMIT.
	// Expected: plain single SELECT — without a LIMIT the two-stage
	// rewrite would materialise every row twice (once in inner, once
	// in JOIN side), which is a net loss.
	"logs_no_limit_no_rewrite": &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Body"}},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
		},
		Input: &chplan.Filter{
			Predicate: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: "ServiceName"},
				Right: &chplan.LitString{V: "api"},
			},
			Input: &chplan.Scan{Table: "otel_logs"},
		},
	},

	// logs_multiple_wide: projection includes Body AND
	// ResourceAttributes — both fetched from the `w` JOIN side; the
	// inner SELECT projects only Timestamp + the row-key tuple.
	"logs_multiple_wide": &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Body"}},
			{Expr: &chplan.ColumnRef{Name: "ResourceAttributes"}},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
		},
		Input: &chplan.Limit{
			Count: 50,
			Input: &chplan.Filter{
				Predicate: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.ColumnRef{Name: "ServiceName"},
					Right: &chplan.LitString{V: "api"},
				},
				Input: &chplan.Scan{Table: "otel_logs"},
			},
		},
	},

	// traces_spanattributes: the traces analog — wide SpanAttributes
	// + LIMIT. Confirms the rewrite is wired for the traces table
	// shape too.
	"traces_spanattributes": &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "SpanAttributes"}},
			{Expr: &chplan.ColumnRef{Name: "TraceId"}},
			{Expr: &chplan.ColumnRef{Name: "SpanId"}},
			{Expr: &chplan.ColumnRef{Name: "SpanName"}},
		},
		Input: &chplan.Limit{
			Count: 25,
			Input: &chplan.Filter{
				Predicate: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.ColumnRef{Name: "ServiceName"},
					Right: &chplan.LitString{V: "api"},
				},
				Input: &chplan.Scan{Table: "otel_traces"},
			},
		},
	},
}

// TestEmitLateMatFixtures runs the late-materialisation TXTAR fixtures.
// Same shape as the main TestEmit harness in emit_test.go.
func TestEmitLateMatFixtures(t *testing.T) {
	t.Parallel()

	spec.Walk(t, lateMatFixtureDir, func(t *testing.T, c *spec.Case) {
		plan, ok := lateMatPlans[c.Name]
		if !ok {
			t.Fatalf("no plan registered for fixture %s; add it to lateMatPlans in late_mat_spec_test.go", c.Name)
		}
		sql, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit failed: %v", err)
		}
		spec.Match(t, c, map[string]string{
			"sql":    sql,
			"args":   formatLateMatArgs(args),
			"chplan": spec.PrintChplan(plan),
		})
	})
}

// formatLateMatArgs mirrors the formatArgs helper in emit_test.go.
// Duplicated rather than exported so the late-mat suite stays free to
// change its formatting independently of the main emit suite.
func formatLateMatArgs(args []any) string {
	if len(args) == 0 {
		return "(none)\n"
	}
	var b strings.Builder
	for i, a := range args {
		fmt.Fprintf(&b, "[%d] %T = %#v\n", i, a, a)
	}
	return b.String()
}

//go:build chdb

// Construct: projection_pushdown — narrowed Scan.Columns realized benefit.
//
// A NEW registered shape the Phase-1 audit confirmed (the ~3.95x/11x
// column-read win) but which had no standalone guard. The optimizer's
// ProjectionPushdown rule narrows a Scan's column list to the union of
// columns an enclosing Project consumes, so on the OTel CH schema's WIDE
// tables only the few columns the plan actually reads are scanned, instead
// of `SELECT *`.
//
// Unlike the other constructs this is a column-read WIN, not a fan-out: THE
// REAL MULTIPLIER is the TABLE WIDTH W (column count). The narrowed Scan
// reads a FIXED 2 columns regardless of W, so its wall stays ~flat
// (sub-linear) as the table widens; the un-narrowed `SELECT *` shape's wall
// grows with W (it materialises every column). Param = W, swept by
// rebuilding the table with more columns (Reseed) at a FIXED row count.
//
// This construct asserts the optimizer ACTUALLY narrows (precondition:
// the emitted SQL projects only the consumed columns, not `*`) and that the
// narrowed read is sub-linear in W — the realized benefit, measured on
// in-process chDB, not just the rule firing in a plan snapshot.
package scaling

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
)

const projWideTable = "wide_metrics"

// projConsumed are the only columns the enclosing Project reads — the
// narrowed Scan must read exactly these, regardless of table width.
var projConsumed = []string{"key_col", "val_col"}

func init() {
	register(Construct{
		Name:        "projection_pushdown",
		Param:       "table width W",
		Why:         "ProjectionPushdown lost — Scan reads all W columns (SELECT *) instead of the consumed few",
		ScanRowsSQL: "SELECT count() FROM " + projWideTable,
		// The narrowed read materialises exactly the consumed rows once; its
		// intermediate is the scan itself. Bound at 1.1x scan_rows.
		CardinalityBound: 1.1,
		SubLinearSlack:   0.9,
		Reseed: func(t *testing.T, db *sql.DB, param int64) {
			execAll(t, db, "DROP TABLE IF EXISTS "+projWideTable, projWideDDL(int(param)),
				projWideInsert(int(param)))
		},
		Points: func(t *testing.T) []Point {
			widths := []int64{8, 32, 96}
			pts := make([]Point, 0, len(widths))
			for _, w := range widths {
				narrowed := emitNarrowedScanSQL(t)
				// Precondition: the optimizer must have NARROWED the scan —
				// the emitted SQL projects the consumed columns, not `*`.
				if strings.Contains(narrowed, "SELECT *") || strings.Contains(narrowed, "SELECT `*`") {
					t.Fatalf("projection_pushdown: optimizer did not narrow the Scan — emitted `SELECT *`:\n%s",
						narrowed)
				}
				for _, col := range projConsumed {
					if !strings.Contains(narrowed, col) {
						t.Fatalf("projection_pushdown: narrowed SQL is missing consumed column %q:\n%s",
							col, narrowed)
					}
				}
				pts = append(pts, Point{
					Param:     w,
					SQL:       narrowed,
					LevelSQLs: []string{narrowed},
				})
			}
			return pts
		},
	})
}

// emitNarrowedScanSQL builds a `Project(Scan(wide_metrics))` plan that
// consumes only projConsumed, runs the PRODUCTION optimizer (whose
// ProjectionPushdown rule narrows Scan.Columns to the consumed set), and
// emits the resulting SQL — the narrowed read under test. The same emitted
// SQL serves every width point (the query reads the same 2 columns); the
// Reseed widens the underlying TABLE so a lost narrowing would force CH to
// read W columns.
func emitNarrowedScanSQL(t *testing.T) string {
	t.Helper()
	plan := chplan.Node(&chplan.Project{
		Input: &chplan.Scan{Table: projWideTable},
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "key_col"}},
			{Expr: &chplan.ColumnRef{Name: "val_col"}},
		},
	})
	optimized := optimizer.Default().Run(context.Background(), plan)
	sqlText, _, err := chsql.Emit(context.Background(), optimized)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sqlText
}

// projWideDDL builds a table with `width` columns: key_col + val_col (the
// consumed pair) plus width-2 wide padding columns the narrowed read must
// skip.
func projWideDDL(width int) string {
	var cols strings.Builder
	cols.WriteString("key_col String, val_col Float64")
	for i := 0; i < width-2; i++ {
		fmt.Fprintf(&cols, ", pad_%d String", i)
	}
	return fmt.Sprintf("CREATE TABLE %s (%s) ENGINE = MergeTree() ORDER BY key_col;",
		projWideTable, cols.String())
}

// projWideInsert fills the wide table with a FIXED row count, padding the
// wide columns with a non-trivial string so `SELECT *` has real bytes to
// read (the read win the narrowing realises). Row count is constant across
// widths so W is the only variable.
func projWideInsert(width int) string {
	var sel strings.Builder
	sel.WriteString("concat('k', toString(number)) AS key_col, toFloat64(number) AS val_col")
	for i := 0; i < width-2; i++ {
		fmt.Fprintf(&sel, ", repeat('p', 64) AS pad_%d", i)
	}
	return fmt.Sprintf("INSERT INTO %s SELECT %s FROM numbers(120000);", projWideTable, sel.String())
}

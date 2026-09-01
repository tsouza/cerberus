//go:build chdb

// chDB-backed proof that chplan.WindowExpr evaluates correctly as a
// non-aggregating pre-pass ahead of a GROUP BY Aggregate, standalone from
// any lowering (issue #2865, PR 1 of 3: the primitive only — no promql
// call site wires into it here).
//
// The shape under test is the one issue #2865's own empirical validation
// used: a Project stage computes `min(Scale) OVER (PARTITION BY route) AS
// _winMergedScale` ahead of an Aggregate that GROUP BYs on route and reads
// _winMergedScale as a plain per-row column inside a SIBLING aggregate's
// argument (`sum(Scale - _winMergedScale)`) — the exact "resolve a value
// in an earlier, independent pass so a sibling aggregate's per-row
// argument can see it" contract chplan.ScalarSubquery solves for a single
// group, generalised here to multiple groups without a JOIN or a
// subquery. The Project-stage column and the Aggregate's own output alias
// are deliberately DIFFERENT names (_winMergedScale vs. mergedScale),
// mirroring the issue's own probe SQL: reusing one alias for both lets CH
// resolve the sibling aggregate's reference back to the OUTPUT alias
// instead of the per-row input column, nesting one aggregate inside
// another (ILLEGAL_AGGREGATION).
//
// None of these tests mark themselves parallel: chdb-go caches one
// session per process (internal/chsqltest's package doc, #2074/#1987),
// so every chDB test in internal/chsql must run serially —
// test/regression/chsql_chdb_isolation_test.go pins it.
package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

// windowExprPlan builds the Project(WindowExpr) -> Aggregate plan every
// test in this file shares: _winMergedScale is computed once per row in
// the Project stage, then read back both as a passthrough aggregate
// (`any(_winMergedScale) AS mergedScale`) and inside a sibling
// aggregate's own argument (`sum(Scale - _winMergedScale)`), proving the
// window value is visible to a GROUP BY aggregate the same way a resolved
// ScalarSubquery scalar is.
func windowExprPlan(scanTable string) chplan.Node {
	return &chplan.Aggregate{
		Input: &chplan.Project{
			Input: &chplan.Scan{Table: scanTable, Columns: []string{"route", "Scale"}},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "route"}, Alias: "route"},
				{Expr: &chplan.ColumnRef{Name: "Scale"}, Alias: "Scale"},
				{
					Expr: &chplan.WindowExpr{
						Fn:          chplan.FnMin,
						Args:        []chplan.Expr{&chplan.ColumnRef{Name: "Scale"}},
						PartitionBy: []chplan.Expr{&chplan.ColumnRef{Name: "route"}},
					},
					Alias: "_winMergedScale",
				},
			},
		},
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "route"}},
		AggFuncs: []chplan.AggFunc{
			{Fn: chplan.FnAny, Args: []chplan.Expr{&chplan.ColumnRef{Name: "_winMergedScale"}}, Alias: "mergedScale"},
			{
				Fn: chplan.FnSum,
				Args: []chplan.Expr{&chplan.Binary{
					Op:    chplan.OpSub,
					Left:  &chplan.ColumnRef{Name: "Scale"},
					Right: &chplan.ColumnRef{Name: "_winMergedScale"},
				}},
				Alias: "scaleDiffSum",
			},
		},
	}
}

// emitAgainstInlineRows emits windowExprPlan and substitutes its scan
// table reference for an inline dataset, mirroring
// TestEmitAggregate_RawMapKeyDoesNotSplitSeries's hand-built-plan / no-DDL
// approach (canonical_series_keys_chdb_test.go).
func emitAgainstInlineRows(t *testing.T, rows string) string {
	t.Helper()
	const scanTable = "windowSeries"
	gotSQL, args, err := chsql.Emit(context.Background(), windowExprPlan(scanTable))
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(args) != 0 {
		t.Fatalf("plan has no parameters, got args %v", args)
	}
	idx := strings.Index(gotSQL, "`"+scanTable+"`")
	if idx < 0 {
		t.Fatalf("emitted SQL does not reference `%s`: %s", scanTable, gotSQL)
	}
	return gotSQL[:idx] + "(" + rows + ")" + gotSQL[idx+len(scanTable)+2:]
}

// TestWindowExpr_MultiGroupMergesPerPartition pins the multi-group case
// issue #2865 identifies WindowExpr as solving without a JOIN: two
// `route='a'` rows (Scale 0 and 1) and one `route='b'` row (Scale 5).
// mergedScale must be each GROUP's own min(Scale), not the whole table's:
// route='a' -> 0, route='b' -> 5. scaleDiffSum proves _winMergedScale was
// resolved BEFORE the outer sum reads it as a per-row argument: route='a'
// sums (0-0)+(1-0)=1, route='b' sums (5-5)=0.
func TestWindowExpr_MultiGroupMergesPerPartition(t *testing.T) {
	const rows = `SELECT 'a' AS route, 0 AS Scale ` +
		`UNION ALL SELECT 'a' AS route, 1 AS Scale ` +
		`UNION ALL SELECT 'b' AS route, 5 AS Scale`
	query := emitAgainstInlineRows(t, rows)

	db := chsqltest.OpenIsolatedChDB(t)
	rowsResult, err := db.Query("SELECT `route`, `mergedScale`, `scaleDiffSum` FROM (" + query + ") ORDER BY `route`")
	if err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
	defer rowsResult.Close()

	type got struct {
		route              string
		mergedScale, diffs int64
	}
	var out []got
	for rowsResult.Next() {
		var g got
		if err := rowsResult.Scan(&g.route, &g.mergedScale, &g.diffs); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, g)
	}
	if err := rowsResult.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	want := []got{{"a", 0, 1}, {"b", 5, 0}}
	if len(out) != len(want) {
		t.Fatalf("got %d groups, want %d: %+v", len(out), len(want), out)
	}
	for i, w := range want {
		if out[i] != w {
			t.Errorf("group %d: got %+v, want %+v", i, out[i], w)
		}
	}
}

// TestWindowExpr_EmptyPartitionKeyMatchesWholeSetScalar pins the
// degenerate no-by()/without() case: an empty PartitionBy renders `OVER
// ()`, ClickHouse's whole-result-set partition — every row is annotated
// with the SAME whole-table min(Scale), reproducing
// chplan.ScalarSubquery's existing single-group behavior without a
// subquery. All three rows collapse into one group (no GroupBy at all, so
// this proves the window value itself, not the grouping), and
// mergedScale must be the table-wide min: 0.
func TestWindowExpr_EmptyPartitionKeyMatchesWholeSetScalar(t *testing.T) {
	plan := &chplan.Aggregate{
		Input: &chplan.Project{
			Input: &chplan.Scan{Table: "windowSeries", Columns: []string{"route", "Scale"}},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "route"}, Alias: "route"},
				{Expr: &chplan.ColumnRef{Name: "Scale"}, Alias: "Scale"},
				{
					Expr: &chplan.WindowExpr{
						Fn:   chplan.FnMin,
						Args: []chplan.Expr{&chplan.ColumnRef{Name: "Scale"}},
					},
					Alias: "_winMergedScale",
				},
			},
		},
		AggFuncs: []chplan.AggFunc{
			{Fn: chplan.FnAny, Args: []chplan.Expr{&chplan.ColumnRef{Name: "_winMergedScale"}}, Alias: "mergedScale"},
		},
	}

	gotSQL, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if len(args) != 0 {
		t.Fatalf("plan has no parameters, got args %v", args)
	}
	const rows = `SELECT 'a' AS route, 0 AS Scale ` +
		`UNION ALL SELECT 'a' AS route, 1 AS Scale ` +
		`UNION ALL SELECT 'b' AS route, 5 AS Scale`
	idx := strings.Index(gotSQL, "`windowSeries`")
	if idx < 0 {
		t.Fatalf("emitted SQL does not reference `windowSeries`: %s", gotSQL)
	}
	query := gotSQL[:idx] + "(" + rows + ")" + gotSQL[idx+len("`windowSeries`"):]

	db := chsqltest.OpenIsolatedChDB(t)
	var mergedScale int64
	if err := db.QueryRow("SELECT `mergedScale` FROM (" + query + ")").Scan(&mergedScale); err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
	if mergedScale != 0 {
		t.Fatalf("got mergedScale=%d, want 0 (whole-set min across all three rows)", mergedScale)
	}
}

// TestWindowExpr_EmptyInputYieldsZeroRows pins the empty-partition
// degenerate case issue #2865 verified empirically: a WindowExpr Project
// stage over an empty input produces an empty Project output, so the
// outer GROUP BY naturally emits zero rows — no special-case handling
// needed, it falls out of plain GROUP BY semantics over an empty input.
func TestWindowExpr_EmptyInputYieldsZeroRows(t *testing.T) {
	const rows = `SELECT 'a' AS route, 0 AS Scale WHERE 1 = 0`
	query := emitAgainstInlineRows(t, rows)

	db := chsqltest.OpenIsolatedChDB(t)
	var count int64
	if err := db.QueryRow("SELECT count() FROM (" + query + ")").Scan(&count); err != nil {
		t.Fatalf("query %s: %v", query, err)
	}
	if count != 0 {
		t.Fatalf("got %d rows from an empty input, want 0", count)
	}
}

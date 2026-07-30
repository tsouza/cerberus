package chsql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestMutation_ProjectionOutputsColumn pins the alias-wins rule that decides
// whether a Project still carries the canonical timestamp column. An alias
// RENAMES the output, so an aliased projection outputs its alias and nothing
// else — the expression underneath is no longer visible to anything above.
// Getting this wrong flips a vector join between the canonical (per-row
// TimeUnix) and derived (one row per series) shapes, which changes the join key
// and therefore the answer.
//
// Kills both CONDITIONALS_NEGATION mutants in vector_join.go's
// projectionOutputsColumn:
//   - `p.Alias != ""` -> `==`: the "renamed away" case below falls through to
//     the ColumnRef arm and wrongly reports the shadowed column as an output,
//     while the "bare column ref" case compares "" to col and reports false.
//   - `p.Alias == col` -> `!=`: the alias cases invert outright.
func TestMutation_ProjectionOutputsColumn(t *testing.T) {
	t.Parallel()

	const col = "TimeUnix"
	for _, tc := range []struct {
		name string
		proj chplan.Projection
		want bool
	}{
		{
			name: "alias names the column",
			proj: chplan.Projection{Expr: &chplan.ColumnRef{Name: "Timestamp"}, Alias: col},
			want: true,
		},
		{
			name: "alias renames the column away",
			// The expression IS the column, but the alias republishes it under
			// another name, so col is no longer an output of this projection.
			proj: chplan.Projection{Expr: &chplan.ColumnRef{Name: col}, Alias: "anchor_ts"},
		},
		{
			name: "bare column ref",
			proj: chplan.Projection{Expr: &chplan.ColumnRef{Name: col}},
			want: true,
		},
		{
			name: "bare column ref to another column",
			proj: chplan.Projection{Expr: &chplan.ColumnRef{Name: "Value"}},
		},
		{
			name: "unaliased non-column expression",
			proj: chplan.Projection{Expr: &chplan.FuncCall{Name: "now64"}},
		},
	} {
		if got := projectionOutputsColumn(tc.proj, col); got != tc.want {
			t.Errorf("%s: projectionOutputsColumn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

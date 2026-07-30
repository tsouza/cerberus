package chsql

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestMutation_EmitProject_ExplicitProjectionsSupersedeReplacements pins that
// the two projection modes are mutually exclusive, with the explicit list
// winning. Replacements express the `SELECT * REPLACE (…)` shape — keep every
// column, rewrite a few — which only makes sense when no explicit list is
// given. Emitting both would append a `*` to a curated SELECT list, so the
// query would return the whole row alongside the projected columns and every
// consumer reading positionally would decode the wrong fields.
//
// Kills emit_node.go's `len(p.Projections) == 0 && len(p.Replacements) > 0`
// INVERT_LOGICAL (`||`), which takes the star-replace branch whenever EITHER
// side holds — including this both-populated plan.
func TestMutation_EmitProject_ExplicitProjectionsSupersedeReplacements(t *testing.T) {
	t.Parallel()

	sql := emitNodeSQL(t, &chplan.Project{
		Input:        &chplan.Scan{Table: "otel_traces"},
		Projections:  []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "TraceId"}, Alias: "tid"}},
		Replacements: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "SpanName"}, Alias: "name"}},
	})
	// Pin the whole SELECT list, not just the presence of the projection: the
	// mutant appends `, * REPLACE (…)` to exactly this list.
	if !strings.HasPrefix(sql, "SELECT `TraceId` AS `tid` FROM ") {
		t.Fatalf("the SELECT list must be the explicit projection alone: %q", sql)
	}
}

// TestMutation_EmitProject_ReplacementsOnlyRenderStarReplace pins the
// complement so the assertion above cannot pass by ignoring Replacements
// entirely: with no explicit list, the replacements render as `* REPLACE (…)`.
func TestMutation_EmitProject_ReplacementsOnlyRenderStarReplace(t *testing.T) {
	t.Parallel()

	sql := emitNodeSQL(t, &chplan.Project{
		Input:        &chplan.Scan{Table: "otel_traces"},
		Replacements: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "SpanName"}, Alias: "name"}},
	})
	if !strings.Contains(sql, "* REPLACE (") || !strings.Contains(sql, "AS `name`") {
		t.Fatalf("replacements-only Project must render `* REPLACE (… AS `name`)`: %q", sql)
	}
}

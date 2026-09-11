package spec

import (
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestBindFixtureSchemas(t *testing.T) {
	scan := &chplan.Scan{Table: "logs", Roles: []chplan.Column{{Name: "Timestamp", Role: chplan.RoleTimestamp}}}
	plan := &chplan.Project{Input: scan, Replacements: []chplan.Projection{{Expr: &chplan.LitString{V: "rewritten"}, Alias: "Body"}}}
	catalog := map[string][]string{"logs": {"Body"}}
	bound := bindFixtureSchemas(plan, catalog)
	want := chplan.Schema{Columns: []chplan.Column{{Name: "Body"}}}
	if !bound.RowType().Equal(want) {
		t.Fatalf("sparse wildcard schema: %#v", bound.RowType())
	}
	if len(scan.Columns) != 0 || !plan.RowType().Open {
		t.Fatal("binding mutated source plan")
	}
	bound.(*chplan.Project).Input.(*chplan.Scan).Columns[0] = "changed"
	if !slices.Equal(catalog["logs"], []string{"Body"}) {
		t.Fatal("binding aliases fixture catalog")
	}
	project := &chplan.Project{Input: scan, Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "Body"}, Alias: "wrong"}}}
	if bindFixtureSchemas(project, catalog).RowType().Equal(want) {
		t.Fatal("fixture binding hid a wrong output alias")
	}
	union := &chplan.Scan{UnionTables: []string{"logs", "other"}, Roles: scan.Roles}
	catalog["other"] = []string{"Body"}
	if got := bindFixtureSchemas(union, catalog).RowType(); !got.Equal(want) {
		t.Fatalf("matching union catalogs: %#v", got)
	}
	catalog["other"] = []string{"different"}
	if !bindFixtureSchemas(union, catalog).RowType().Open {
		t.Fatal("different table-function schemas were guessed")
	}
}

func TestBindFixtureSchemasExplicitColumns(t *testing.T) {
	scan := &chplan.Scan{Table: "logs", Columns: []string{"Timestamp", "Body"}, Roles: []chplan.Column{{Name: "Timestamp", Role: chplan.RoleTimestamp}}}
	bound := bindFixtureSchemas(scan, map[string][]string{"logs": {"Body"}})
	if !bound.RowType().Equal(scan.RowType()) {
		t.Fatalf("explicit projection changed: %#v", bound.RowType())
	}
}

func TestBindFixtureSchemasDatabaseQualified(t *testing.T) {
	scan := &chplan.Scan{Database: "Observability", Table: "Logs"}
	bound := bindFixtureSchemas(scan, map[string][]string{"observability.logs": {"Body"}, "logs": {"wrong"}})
	want := chplan.Schema{Columns: []chplan.Column{{Name: "Body"}}}
	if !bound.RowType().Equal(want) {
		t.Fatalf("database-qualified catalog lookup: %#v", bound.RowType())
	}
}

func TestBindFixtureSchemasExpressionSubquery(t *testing.T) {
	scan := &chplan.Scan{Table: "logs"}
	plan := &chplan.Project{Input: &chplan.OneRow{}, Projections: []chplan.Projection{{Expr: &chplan.ScalarSubquery{Input: scan}, Alias: "scalar"}}}
	bound := bindFixtureSchemas(plan, map[string][]string{"logs": {"Body"}}).(*chplan.Project)
	inner := bound.Projections[0].Expr.(*chplan.ScalarSubquery).Input
	want := chplan.Schema{Columns: []chplan.Column{{Name: "Body"}}}
	if !inner.RowType().Equal(want) {
		t.Fatalf("expression subquery was not bound: %#v", inner.RowType())
	}
	if len(scan.Columns) != 0 {
		t.Fatal("binding mutated source expression subquery")
	}
}

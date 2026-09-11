package spec

import (
	"slices"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// AssertRowTypeMatchesDriver checks physical output names and order on an
// executed fixture, including queries returning zero rows. Non-roundtrip
// fixtures and the untagged execution stub carry no driver metadata.
func AssertRowTypeMatchesDriver(t *testing.T, plan chplan.Node, result RoundTripResult) {
	t.Helper()
	if !result.seeded {
		return
	}
	want := bindFixtureSchemas(plan, result.tableColumns).RowType()
	comparedNamedColumns := 0
	if !want.Open && len(want.Columns) != len(result.projectionColumns) {
		t.Fatalf("RowType has %d columns; driver has %d: schema=%#v driver=%q", len(want.Columns), len(result.projectionColumns), want, result.projectionColumns)
	}
	for i, column := range want.Columns {
		if column.Name == "" {
			continue
		}
		comparedNamedColumns++
		if want.Open {
			found := false
			for _, name := range result.projectionColumns {
				if name == column.Name {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("RowType open output %q absent from driver columns %q", column.Name, result.projectionColumns)
			}
		} else if column.Name != result.projectionColumns[i] {
			t.Errorf("RowType column %d = %q; driver = %q (all columns %q)", i, column.Name, result.projectionColumns[i], result.projectionColumns)
		}
	}
	if !t.Failed() {
		t.Logf("RowType driver verified: root=%T open=%t compared_named_columns=%d", plan, want.Open, comparedNamedColumns)
	}
}

// bindFixtureSchemas substitutes the fixture's actual physical table columns
// on a clone. Minimal seeds often omit configured columns unused by a query;
// their wildcard schema is consequently narrower than the deployed table.
// The original plan and emitted SQL remain untouched. The catalog comes from
// CREATE TABLE declarations, never from the query's result metadata.
func bindFixtureSchemas(plan chplan.Node, tables map[string][]string) chplan.Node {
	bound := chplan.CloneNode(plan)
	chplan.WalkDeep(bound, func(n chplan.Node) bool {
		scan, ok := n.(*chplan.Scan)
		if !ok || len(scan.Columns) != 0 {
			return true
		}
		lookup := func(table string) []string {
			if scan.Database != "" {
				table = scan.Database + "." + table
			}
			return tables[strings.ToLower(table)]
		}
		columns := lookup(scan.Table)
		if len(scan.UnionTables) != 0 {
			columns = lookup(scan.UnionTables[0])
			for _, table := range scan.UnionTables[1:] {
				if !slices.Equal(columns, lookup(table)) {
					return true
				}
			}
		}
		if len(columns) != 0 {
			scan.Columns = slices.Clone(columns)
		}
		return true
	})
	return bound
}

// AssertPlanScanRoles checks every storage leaf, including expression subqueries.
// Physical schema/shape semantics have independent literal, sealed-kind-complete
// contracts in chplan; executed fixture outputs are checked against driver metadata.
func AssertPlanScanRoles(t *testing.T, plan chplan.Node) {
	t.Helper()
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if scan, ok := n.(*chplan.Scan); ok && scan.Database != "system" && len(scan.Roles) == 0 {
			t.Errorf("storage Scan %q/%q has no declared roles", scan.Table, scan.UnionTables)
		}
		return true
	})
}

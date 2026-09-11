package spec

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
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

var contractedRowShapeKinds = sync.OnceValues(func() (map[string]bool, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, fmt.Errorf("cannot find repository from test working directory")
		}
		dir = parent
	}
	path := filepath.Join(dir, "internal", "chplan", "row_shape.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, err
	}
	kinds := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "RowShapeOf" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				pointer, ok := expr.(*ast.StarExpr)
				if !ok {
					continue
				}
				if name, ok := pointer.X.(*ast.Ident); ok {
					kinds[name.Name] = true
				}
			}
			return true
		})
	}
	if len(kinds) == 0 {
		return nil, fmt.Errorf("RowShapeOf has no contracted type-switch cases")
	}
	return kinds, nil
})

// AssertRowShapeAgreement derives the legacy classifier's contracted kinds
// from its own switch. Other kinds deliberately retain a documentation default.
func AssertRowShapeAgreement(t *testing.T, plan chplan.Node) {
	t.Helper()
	kinds, err := contractedRowShapeKinds()
	if err != nil {
		t.Fatalf("read legacy shape contract: %v", err)
	}
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if scan, ok := n.(*chplan.Scan); ok && scan.Database != "system" && len(scan.Roles) == 0 {
			t.Errorf("storage Scan %q/%q has no declared roles", scan.Table, scan.UnionTables)
		}
		kind := reflect.TypeOf(n).Elem().Name()
		legacy := chplan.RowShapeOf(n)
		if !kinds[kind] {
			if legacy != chplan.SampleRowShape {
				t.Errorf("uncontracted %s legacy shape = %s", kind, legacy)
			}
			return true
		}
		physical := chplan.RowShapeFromSchema(n.RowType())
		reason := rowShapeDivergence(n)
		if legacy != physical && reason == "" {
			t.Errorf("%s legacy=%s physical=%s schema=%#v", kind, legacy, physical, n.RowType())
		}
		return true
	})
}

// rowShapeDivergence describes legacy classifier branches whose contract is
// deliberately weaker than their physical output. These predicates describe
// actual SELECT structure; they are not fixture or node-name exemptions.
func rowShapeDivergence(n chplan.Node) string {
	s := n.RowType()
	physical, legacy := chplan.RowShapeFromSchema(s), chplan.RowShapeOf(n)
	if physical == legacy {
		return ""
	}
	switch v := n.(type) {
	case *chplan.Project:
		if legacy != chplan.SampleRowShape || s.Has(chplan.RoleDiscriminator) {
			return ""
		}
		if s.Has(chplan.RoleHistogramField) {
			return "projection republishes histogram fields without discriminator"
		}
		if !s.Has(chplan.RoleMetricName) && s.Has(chplan.RoleAnchor) {
			return "projection republishes grid outputs"
		}
		if !s.Has(chplan.RoleMetricName) && !s.Has(chplan.RoleTimestamp) && !s.Has(chplan.RoleAnchor) && s.Has(chplan.RoleValue) {
			return "projection publishes scalar or reduced output"
		}

	case *chplan.HistogramFloatVectorJoin:
		if legacy == chplan.SampleRowShape && physical == chplan.HistogramRowShape && s.Has(chplan.RoleValue) && s.Has(chplan.RoleMetricName) {
			return "float-histogram join preserves payload and float value"
		}
	case *chplan.RangeWindow:
		if s.Has(chplan.RoleMetricName) && s.Has(chplan.RoleValue) && !s.Has(chplan.RoleHistogramField) && !s.Has(chplan.RoleDiscriminator) {
			for _, key := range v.GroupBy {
				ref, ok := key.(*chplan.ColumnRef)
				if !ok {
					continue
				}
				column, found := v.Input.RowType().ByName(ref.Name)
				if found && column.Role == chplan.RoleMetricName && ((v.OuterRange > 0 && legacy == chplan.GridWindowRowShape) || (v.OuterRange == 0 && legacy == chplan.ReducedWindowRowShape)) {
					return "window group keys preserve metric name"
				}
			}
		}
	case *chplan.OrderBy:
		if legacy == chplan.RowShapeOf(v.Input) && s.Equal(v.Input.RowType()) {
			return delegatedShapeDivergence(v.Input)
		}
	case *chplan.UnionAll:
		if len(v.Inputs) > 0 && legacy == chplan.RowShapeOf(v.Inputs[0]) && s.Equal(v.Inputs[0].RowType()) {
			return delegatedShapeDivergence(v.Inputs[0])
		}
	}
	return ""
}

func delegatedShapeDivergence(input chplan.Node) string {
	if reason := rowShapeDivergence(input); reason != "" {
		return reason
	}
	kinds, err := contractedRowShapeKinds()
	if err == nil && !kinds[reflect.TypeOf(input).Elem().Name()] && chplan.RowShapeOf(input) == chplan.SampleRowShape {
		return "delegated documentation default from non-sample input"
	}
	return ""
}

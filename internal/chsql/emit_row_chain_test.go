package chsql

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// rowChainPlan builds a consumer marked OneScopeRows over a row chain: a
// scan filtered on Timestamp, a layer defining _lower from Body, a filter
// reading _lower and LogAttributes['level'], and a consumer selecting Body
// by name.
func rowChainPlan(layer []chplan.Projection) *chplan.Project {
	scan := &chplan.Filter{
		Input:     &chplan.Scan{Table: "otel_logs", Columns: []string{"Body", "LogAttributes", "Timestamp"}},
		Predicate: &chplan.Binary{Op: chplan.OpGe, Left: &chplan.ColumnRef{Name: "Timestamp"}, Right: &chplan.LitInt{V: 0}},
	}
	filter := &chplan.Filter{
		Input: &chplan.Project{Input: scan, Projections: layer},
		Predicate: &chplan.Binary{
			Op:   chplan.OpAnd,
			Left: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "_lower"}, Right: &chplan.LitString{V: "a"}},
			Right: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.MapAccess{Map: &chplan.ColumnRef{Name: "LogAttributes"}, Key: &chplan.LitString{V: "level"}},
				Right: &chplan.LitString{V: "error"},
			},
		},
	}
	return &chplan.Project{
		Input:        filter,
		Projections:  []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "Body"}, Alias: "Line"}},
		OneScopeRows: true,
	}
}

func lowerBodyLayer() []chplan.Projection {
	return []chplan.Projection{
		{Expr: &chplan.ColumnRef{Name: "Body"}},
		{Expr: &chplan.ColumnRef{Name: "LogAttributes"}},
		{Expr: &chplan.FuncCall{Fn: chplan.FnLower, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}}}, Alias: "_lower"},
	}
}

// TestEmitProject_FusesARowChainIntoOneSelect pins that a filter over a
// per-row layer over a filtered scan renders as one SELECT whose WHERE
// reads both the layer's alias and the table's own map column, in the
// scope that reads the table.
func TestEmitProject_FusesARowChainIntoOneSelect(t *testing.T) {
	t.Parallel()
	sql := emitNodeSQL(t, rowChainPlan(lowerBodyLayer()))
	want := "SELECT `Body` AS `Line` FROM (SELECT `Body`, `LogAttributes`, lower(`Body`) AS `_lower` FROM `otel_logs` " +
		"WHERE (`Timestamp` >= ?) AND ((`_lower` = ?) AND (`LogAttributes`[?] = ?)))"
	if sql != want {
		t.Fatalf("fused row chain:\n got %s\nwant %s", sql, want)
	}
}

// TestEmitProject_RowChainFusionRefusals pins each shape the fusion must
// leave nested: one SELECT would change what a name means, or what a
// value is evaluated over.
func TestEmitProject_RowChainFusionRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		plan  func() *chplan.Project
		nests string
	}{
		{
			// In one SELECT the alias would shadow the column the scan's
			// predicate reads.
			name: "alias names a column the scan's filter reads",
			plan: func() *chplan.Project {
				layer := lowerBodyLayer()
				layer = append(layer, chplan.Projection{Expr: &chplan.LitInt{V: 1}, Alias: "Timestamp"})
				return rowChainPlan(layer)
			},
		},
		{
			// Only a consumer marked OneScopeRows reads its Input fused.
			name: "consumer is not marked",
			plan: func() *chplan.Project {
				p := rowChainPlan(lowerBodyLayer())
				p.OneScopeRows = false
				return p
			},
		},
		{
			// A filter below a layer that defines Body reads the table's
			// Body; in one SELECT it would read the alias.
			name: "alias names a table column a node below it reads",
			plan: func() *chplan.Project {
				p := rowChainPlan(lowerBodyLayer())
				p.Input.(*chplan.Filter).Input.(*chplan.Project).Input.(*chplan.Filter).Input.(*chplan.Scan).Columns = nil
				p.Input = &chplan.Project{Input: p.Input, Projections: []chplan.Projection{
					{Expr: &chplan.ColumnRef{Name: "_lower"}},
					{Expr: &chplan.FuncCall{Fn: chplan.FnLower, Args: []chplan.Expr{&chplan.ColumnRef{Name: "_lower"}}}, Alias: "Body"},
				}}
				return p
			},
		},
		{
			// A filter below the layer that defines Level reads the
			// table's Level; in one SELECT it would read the alias.
			name: "filter below a layer reads a column its alias names",
			plan: func() *chplan.Project {
				p := rowChainPlan(lowerBodyLayer())
				layer := p.Input.(*chplan.Filter).Input.(*chplan.Project)
				scanFilter := layer.Input.(*chplan.Filter)
				scanFilter.Input.(*chplan.Scan).Columns = nil
				layer.Projections = append(layer.Projections, chplan.Projection{Expr: &chplan.LitString{V: "x"}, Alias: "Level"})
				layer.Input = &chplan.Filter{
					Input:     scanFilter,
					Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "Level"}, Right: &chplan.LitString{V: "error"}},
				}
				return p
			},
		},
		{
			// A layer reading a column a later layer's alias names reads
			// the table's column.
			name: "layer reads a column a later alias names",
			plan: func() *chplan.Project {
				p := rowChainPlan(lowerBodyLayer())
				p.Input.(*chplan.Filter).Input.(*chplan.Project).Input.(*chplan.Filter).Input.(*chplan.Scan).Columns = nil
				f := p.Input.(*chplan.Filter)
				f.Input = &chplan.Project{Input: f.Input, Projections: []chplan.Projection{
					{Expr: &chplan.ColumnRef{Name: "Body"}},
					{Expr: &chplan.ColumnRef{Name: "LogAttributes"}},
					{Expr: &chplan.ColumnRef{Name: "_lower"}},
					{Expr: &chplan.LitInt{V: 1}, Alias: "Severity"},
				}}
				lower := f.Input.(*chplan.Project).Input.(*chplan.Project)
				lower.Projections = append(lower.Projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: "Severity"}, Alias: "_severity"})
				return p
			},
		},
		{
			// Without a select list the consumer would return the fused
			// SELECT's extra columns.
			name: "consumer selects every column",
			plan: func() *chplan.Project {
				p := rowChainPlan(lowerBodyLayer())
				p.Projections = nil
				return p
			},
		},
		{
			// With no filter above the layer there is nothing to fuse.
			name: "no filter above a layer",
			plan: func() *chplan.Project {
				p := rowChainPlan(lowerBodyLayer())
				p.Input = p.Input.(*chplan.Filter).Input
				return p
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql := emitNodeSQL(t, tc.plan())
			if strings.Count(sql, "FROM (SELECT") < 2 {
				t.Fatalf("the chain must stay nested, one SELECT per node above the scan: %s", sql)
			}
		})
	}
}

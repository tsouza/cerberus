package traceql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestReadSelectedAttributesFrom pins each condition under which a select()
// keeps a materialized attribute read: a String-kind column on an open row
// type, or on a closed one that names the column. A numeric-kind column,
// or a closed row type that does not name the column, reads the map.
func TestReadSelectedAttributesFrom(t *testing.T) {
	t.Parallel()

	const column = "__cerberus_materialized_rpc.method"
	closedWith := chplan.Schema{Columns: []chplan.Column{{Name: "TraceId"}, {Name: column}}}
	closedWithout := chplan.Schema{Columns: []chplan.Column{{Name: "TraceId"}, {Name: "SpanAttributes"}}}
	open := chplan.Schema{Open: true}

	cases := []struct {
		name    string
		row     chplan.Schema
		numeric bool
		want    string
	}{
		{"open row keeps a string column", open, false, column},
		{"closed row naming the column keeps it", closedWith, false, column},
		{"closed row without the column reads the map", closedWithout, false, ""},
		{"numeric column reads the map on an open row", open, true, ""},
		{"numeric column reads the map on a closed row naming it", closedWith, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &chplan.FieldAccess{
				Source:                    &chplan.ColumnRef{Name: "SpanAttributes"},
				Path:                      "rpc.method",
				MaterializedColumn:        column,
				MaterializedColumnNumeric: tc.numeric,
			}
			readSelectedAttributesFrom([]chplan.Projection{{Expr: f, Alias: "rpc.method"}}, tc.row)
			if f.MaterializedColumn != tc.want {
				t.Errorf("MaterializedColumn = %q, want %q", f.MaterializedColumn, tc.want)
			}
			if f.MaterializedColumn == "" && f.MaterializedColumnNumeric {
				t.Error("a map read still claims a numeric materialized column")
			}
		})
	}
}

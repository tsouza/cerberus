package chplan

import "testing"

type nativeInstantSchemaNode struct {
	schema Schema
}

func (*nativeInstantSchemaNode) planNode()               {}
func (*nativeInstantSchemaNode) Children() []Node        { return nil }
func (n *nativeInstantSchemaNode) Equal(other Node) bool { return n == other }
func (n *nativeInstantSchemaNode) RowType() Schema       { return n.schema }

func nativeInstantNode(schema Schema) *RangeWindowGridNativeInstant {
	return &RangeWindowGridNativeInstant{Input: &nativeInstantSchemaNode{schema: schema}}
}

func TestRangeWindowGridNativeInstantInputValueColumn(t *testing.T) {
	t.Parallel()
	valid := []Column{
		{Name: "labels", Role: RoleAttributes},
		{Name: "sample_time", Role: RoleTimestamp},
		{Name: "physical_value", Role: RoleValue},
	}
	missing := Schema{Columns: append(append([]Column{}, valid[:2]...), Column{})}
	unnamed := Schema{Columns: append(append([]Column{}, valid[:2]...),
		Column{Role: RoleValue}, Column{Name: "later_value", Role: RoleValue})}
	duplicate := Schema{Columns: append(append([]Column{}, valid...),
		Column{Name: "other_value", Role: RoleValue})}
	sharedName := Schema{Columns: append(append([]Column{}, valid...),
		Column{Name: "physical_value", Role: RoleOpaque})}
	tests := []struct {
		name  string
		node  *RangeWindowGridNativeInstant
		want  string
		valid bool
	}{
		{name: "nil_receiver"},
		{name: "nil_input", node: &RangeWindowGridNativeInstant{}},
		{name: "open", node: nativeInstantNode(Schema{Columns: valid, Open: true})},
		{name: "missing", node: nativeInstantNode(missing)},
		{name: "unnamed", node: nativeInstantNode(unnamed)},
		{name: "duplicate_role", node: nativeInstantNode(duplicate)},
		{name: "shared_name", node: nativeInstantNode(sharedName)},
		{name: "valid", node: nativeInstantNode(Schema{Columns: valid}), want: "physical_value", valid: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := tc.node.InputValueColumn()
			if ok != tc.valid || got != tc.want {
				t.Fatalf("InputValueColumn() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.valid)
			}
		})
	}
}

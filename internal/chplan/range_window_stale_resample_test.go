package chplan

import "testing"

type staleResampleSchemaNode struct {
	schema Schema
}

func (*staleResampleSchemaNode) planNode()               {}
func (*staleResampleSchemaNode) Children() []Node        { return nil }
func (n *staleResampleSchemaNode) Equal(other Node) bool { return n == other }
func (n *staleResampleSchemaNode) RowType() Schema       { return n.schema }

func TestRangeWindowStaleResampleInputColumnsRejectsMalformedSchemas(t *testing.T) {
	t.Parallel()
	valid := []Column{
		{Name: "name", Role: RoleMetricName},
		{Name: "labels", Role: RoleAttributes},
		{Name: "time", Role: RoleTimestamp},
		{Name: "value", Role: RoleValue},
	}
	missingSchema := Schema{Columns: append(append([]Column{}, valid[:3]...), Column{})}
	unnamedSchema := Schema{Columns: append(
		append([]Column{}, valid[:3]...),
		Column{Role: RoleValue},
		Column{Name: "later_value", Role: RoleValue},
	)}
	sharedNameSchema := Schema{Columns: []Column{
		{Name: "name", Role: RoleMetricName},
		{Name: "labels", Role: RoleAttributes},
		{Name: "value", Role: RoleTimestamp},
		{Name: "value", Role: RoleValue},
	}}
	tests := []struct {
		name string
		node *RangeWindowStaleResample
	}{
		{name: "nil_receiver"},
		{name: "nil_input", node: &RangeWindowStaleResample{}},
		{name: "open", node: &RangeWindowStaleResample{Input: &Scan{Roles: valid}}},
		{name: "missing", node: &RangeWindowStaleResample{Input: &staleResampleSchemaNode{schema: missingSchema}}},
		{name: "unnamed", node: &RangeWindowStaleResample{Input: &staleResampleSchemaNode{schema: unnamedSchema}}},
		{name: "duplicate_role", node: &RangeWindowStaleResample{Input: &Scan{
			Columns: []string{"name", "labels", "time", "value", "other_value"},
			Roles:   append(append([]Column{}, valid...), Column{Name: "other_value", Role: RoleValue}),
		}}},
		{name: "shared_name", node: &RangeWindowStaleResample{Input: &staleResampleSchemaNode{schema: sharedNameSchema}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := tc.node.InputColumns(); ok {
				t.Fatal("InputColumns unexpectedly accepted malformed schema")
			}
		})
	}
}

func TestRangeWindowStaleResampleInputColumnsResolvesRoles(t *testing.T) {
	t.Parallel()
	roles := []Column{
		{Name: "physical_name", Role: RoleMetricName},
		{Name: "physical_labels", Role: RoleAttributes},
		{Name: "physical_time", Role: RoleTimestamp},
		{Name: "physical_value", Role: RoleValue},
	}
	node := &RangeWindowStaleResample{Input: &Scan{
		Columns: []string{"physical_name", "physical_labels", "physical_time", "physical_value"},
		Roles:   roles,
	}}
	want := StaleResampleColumns{
		MetricName: "physical_name",
		Attributes: "physical_labels",
		Timestamp:  "physical_time",
		Value:      "physical_value",
	}
	got, ok := node.InputColumns()
	if !ok || got != want {
		t.Fatalf("InputColumns() = (%+v, %v), want (%+v, true)", got, ok, want)
	}
}

package chplan

import "testing"

func TestRangeWindowStaleResampleInputColumnsRejectsMalformedSchemas(t *testing.T) {
	t.Parallel()
	valid := []Column{
		{Name: "name", Role: RoleMetricName},
		{Name: "labels", Role: RoleAttributes},
		{Name: "time", Role: RoleTimestamp},
		{Name: "value", Role: RoleValue},
	}
	tests := []struct {
		name string
		node *RangeWindowStaleResample
	}{
		{name: "nil_receiver"},
		{name: "nil_input", node: &RangeWindowStaleResample{}},
		{name: "open", node: &RangeWindowStaleResample{Input: &Scan{Roles: valid}}},
		{name: "missing", node: &RangeWindowStaleResample{Input: &Scan{Columns: []string{"name", "labels", "time"}, Roles: valid[:3]}}},
		{name: "unnamed", node: &RangeWindowStaleResample{Input: &Project{
			Input: &OneRow{},
			Projections: []Projection{
				{Expr: &LitFloat{V: 1}, Alias: "name"},
				{Expr: &LitFloat{V: 1}, Alias: "labels"},
				{Expr: &LitFloat{V: 1}, Alias: "time"},
				{Expr: &LitFloat{V: 1}},
			},
			Roles: append(append([]Column{}, valid[:3]...), Column{Role: RoleValue}),
		}}},
		{name: "duplicate_role", node: &RangeWindowStaleResample{Input: &Scan{
			Columns: []string{"name", "labels", "time", "value", "other_value"},
			Roles:   append(append([]Column{}, valid...), Column{Name: "other_value", Role: RoleValue}),
		}}},
		{name: "shared_name", node: &RangeWindowStaleResample{Input: &Scan{
			Columns: []string{"name", "labels", "time", "value", "value"},
			Roles:   append(append([]Column{}, valid...), Column{Name: "value", Role: RoleTimestamp}),
		}}},
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

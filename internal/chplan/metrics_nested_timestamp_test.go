package chplan

import "testing"

func TestMetricsNestedTimestampColumn(t *testing.T) {
	t.Parallel()
	valid := &Scan{
		Table:   "spans",
		Columns: []string{"physical_timestamp", "Duration"},
		Roles:   []Column{{Name: "physical_timestamp", Role: RoleTimestamp}},
	}
	for name, input := range map[string]interface {
		InputTimestampColumn() (string, bool)
	}{
		"aggregate": &MetricsAggregate{Inner: valid},
		"histogram": &MetricsHistogramOverTime{Inner: valid},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := input.InputTimestampColumn()
			if !ok || got != "physical_timestamp" {
				t.Fatalf("InputTimestampColumn() = (%q, %v), want (physical_timestamp, true)", got, ok)
			}
		})
	}
}

func TestMetricsNestedTimestampColumnRejectsMalformedSchema(t *testing.T) {
	t.Parallel()
	tests := map[string]Node{
		"missing": &Scan{Table: "spans", Columns: []string{"Duration"}},
		"unnamed": &Project{
			Input:       &OneRow{},
			Projections: []Projection{{Expr: &LitFloat{V: 1}}},
			Roles:       []Column{{Role: RoleTimestamp}},
		},
		"duplicate_role": &Scan{
			Table:   "spans",
			Columns: []string{"time_a", "time_b"},
			Roles: []Column{
				{Name: "time_a", Role: RoleTimestamp},
				{Name: "time_b", Role: RoleTimestamp},
			},
		},
		"shared_name": &Project{
			Input: &OneRow{},
			Projections: []Projection{
				{Expr: &LitFloat{V: 1}, Alias: "time"},
				{Expr: &LitFloat{V: 2}, Alias: "time"},
			},
			Roles: []Column{{Name: "time", Role: RoleTimestamp}},
		},
	}
	for name, inner := range tests {
		inner := inner
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, input := range []interface {
				InputTimestampColumn() (string, bool)
			}{
				&MetricsAggregate{Inner: inner},
				&MetricsHistogramOverTime{Inner: inner},
			} {
				if got, ok := input.InputTimestampColumn(); ok {
					t.Fatalf("InputTimestampColumn() = (%q, true), want rejection", got)
				}
			}
		})
	}
}

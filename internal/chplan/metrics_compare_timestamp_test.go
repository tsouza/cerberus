package chplan

import "testing"

func TestMetricsCompareTimestampOwnership(t *testing.T) {
	t.Parallel()
	m := &MetricsCompare{
		Inner: &Scan{Table: "spans", Roles: []Column{{Name: "cohort_time", Role: RoleTimestamp}}},
		RootLookup: &Aggregate{Input: &Filter{Input: &Scan{
			Table: "roots", Roles: []Column{{Name: "root_time", Role: RoleTimestamp}},
		}}},
	}
	if got, ok := m.InputTimestampColumn(); !ok || got != "cohort_time" {
		t.Fatalf("InputTimestampColumn() = (%q, %v), want (cohort_time, true)", got, ok)
	}
	if got, ok := m.RootLookupTimestampColumn(); !ok || got != "root_time" {
		t.Fatalf("RootLookupTimestampColumn() = (%q, %v), want (root_time, true)", got, ok)
	}
}

func TestMetricsCompareTimestampOwnershipRejectsMalformedSchemas(t *testing.T) {
	t.Parallel()
	malformed := []Node{
		&Scan{Table: "spans"},
		&Scan{Table: "spans", Roles: []Column{{Role: RoleTimestamp}}},
		&Scan{Table: "spans", Columns: []string{"a", "b"}, Roles: []Column{
			{Name: "a", Role: RoleTimestamp}, {Name: "b", Role: RoleTimestamp},
		}},
		&Project{Input: &OneRow{}, Projections: []Projection{
			{Expr: &LitInt{V: 1}, Alias: "shared"}, {Expr: &LitInt{V: 2}, Alias: "shared"},
		}, Roles: []Column{{Name: "shared", Role: RoleTimestamp}}},
	}
	for _, node := range malformed {
		if got, ok := (&MetricsCompare{Inner: node}).InputTimestampColumn(); ok {
			t.Errorf("InputTimestampColumn() = (%q, true), want rejection for %T", got, node)
		}
		if got, ok := (&MetricsCompare{RootLookup: node}).RootLookupTimestampColumn(); ok {
			t.Errorf("RootLookupTimestampColumn() = (%q, true), want rejection for %T", got, node)
		}
	}
}

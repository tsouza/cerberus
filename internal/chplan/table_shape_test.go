package chplan

import "testing"

// TestTableShape_Lookups pins each TableShape lookup over a shape that lists
// a column in every role, and over the zero shape, which lists none.
func TestTableShape_Lookups(t *testing.T) {
	t.Parallel()
	s := TableShape{
		SortColumns:                 []string{"MetricName", "ServiceName"},
		WideColumns:                 []string{"ResourceAttributes"},
		IntegerDiscriminatorColumns: []string{"AggregationTemporality"},
		SkipIndexColumns:            []string{"TraceId"},
	}
	if got := s.SortRank("ServiceName"); got != 1 {
		t.Errorf("SortRank(ServiceName) = %d, want 1", got)
	}
	if got := s.SortRank("Value"); got != -1 {
		t.Errorf("SortRank(Value) = %d, want -1", got)
	}
	for name, got := range map[string]bool{
		"IsSortColumn":                 s.IsSortColumn("MetricName") && !s.IsSortColumn("Value"),
		"IsWideColumn":                 s.IsWideColumn("ResourceAttributes") && !s.IsWideColumn("MetricName"),
		"IsIntegerDiscriminatorColumn": s.IsIntegerDiscriminatorColumn("AggregationTemporality") && !s.IsIntegerDiscriminatorColumn("Value"),
		"IsSkipIndexColumn":            s.IsSkipIndexColumn("TraceId") && !s.IsSkipIndexColumn("MetricName"),
	} {
		if !got {
			t.Errorf("%s does not tell a listed column from an unlisted one", name)
		}
	}
	var zero TableShape
	if zero.SortRank("MetricName") != -1 || zero.IsSortColumn("MetricName") || zero.IsWideColumn("MetricName") ||
		zero.IsIntegerDiscriminatorColumn("MetricName") || zero.IsSkipIndexColumn("MetricName") {
		t.Error("the zero shape lists a column")
	}
}

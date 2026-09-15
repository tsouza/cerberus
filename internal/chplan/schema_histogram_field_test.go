package chplan

import "testing"

func TestHistogramPayloadFieldIdentity(t *testing.T) {
	t.Parallel()

	fields := []HistogramField{
		HistogramFieldCount, HistogramFieldSum, HistogramFieldScale,
		HistogramFieldZeroThreshold, HistogramFieldZeroCount,
		HistogramFieldPositiveOffset, HistogramFieldPositiveBucketCounts,
		HistogramFieldNegativeOffset, HistogramFieldNegativeBucketCounts,
	}
	schema := Schema{Columns: HistogramPayloadColumns()}
	for _, field := range fields {
		column, ok := schema.FindHistogramField(field)
		if !ok || column.HistogramField != field {
			t.Fatalf("FindHistogramField(%d) = %#v, %v", field, column, ok)
		}
	}
	if _, ok := schema.FindHistogramField(HistogramFieldNone); ok {
		t.Fatal("zero histogram identity unexpectedly resolved")
	}
	if _, ok := schema.FindHistogramField(HistogramFieldExplicitBounds + 1); ok {
		t.Fatal("out-of-range histogram identity unexpectedly resolved")
	}
}

func TestFindHistogramFieldFailsClosed(t *testing.T) {
	t.Parallel()

	valid := Column{Name: "configured_count", Role: RoleHistogramField, HistogramField: HistogramFieldCount}
	for _, columns := range [][]Column{
		{valid, valid},
		{{Role: RoleHistogramField, HistogramField: HistogramFieldCount}},
		{{Name: "count", Role: RoleOpaque, HistogramField: HistogramFieldCount}},
		{valid, {Name: "configured_count", Role: RoleHistogramField, HistogramField: HistogramFieldSum}},
	} {
		if _, ok := (Schema{Columns: columns}).FindHistogramField(HistogramFieldCount); ok {
			t.Fatalf("malformed histogram identity resolved from %#v", columns)
		}
	}
}

func TestHistogramFieldCloneAndEquality(t *testing.T) {
	t.Parallel()

	scan := &Scan{Table: "samples", Roles: []Column{{Name: "configured_count", Role: RoleHistogramField, HistogramField: HistogramFieldCount}}}
	clone := CloneNode(scan).(*Scan)
	clone.Roles[0].HistogramField = HistogramFieldSum
	if scan.Equal(clone) {
		t.Fatal("scan equality ignored histogram field identity")
	}
	if scan.Roles[0].HistogramField != HistogramFieldCount {
		t.Fatal("clone mutation changed original histogram field identity")
	}
}

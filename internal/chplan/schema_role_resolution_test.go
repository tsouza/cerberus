package chplan

import (
	"errors"
	"testing"
)

// The two resolvers below are the ONE place a plan consumer turns a role (or a
// histogram field) into the column that carries it. Every edge of that
// contract is pinned here, once, so an emitter that consumes the resolver
// inherits the pinned behaviour instead of re-deriving it.

func TestUniqueNamedRoleResolvesTheOneNamedCarrier(t *testing.T) {
	t.Parallel()

	row := Schema{Columns: []Column{
		{Name: "attrs", Role: RoleAttributes},
		{Name: "physical_time", Role: RoleTimestamp},
		{Name: "physical_time_copy"},
		{Name: "v", Role: RoleValue},
	}}
	column, err := row.UniqueNamedRole(RoleTimestamp)
	if err != nil {
		t.Fatalf("UniqueNamedRole(RoleTimestamp): %v", err)
	}
	if column.Name != "physical_time" || column.Role != RoleTimestamp {
		t.Fatalf("UniqueNamedRole(RoleTimestamp) = %#v, want the physical_time carrier", column)
	}

	// An open schema still resolves: openness is a claim about undeclared
	// outputs, and whether an operator tolerates it is that operator's call.
	row.Open = true
	if _, err := row.UniqueNamedRole(RoleTimestamp); err != nil {
		t.Fatalf("UniqueNamedRole over an open schema: %v", err)
	}
}

func TestUniqueNamedRoleFailsClosedOnEveryMalformedShape(t *testing.T) {
	t.Parallel()

	timestamp := Column{Name: "ts", Role: RoleTimestamp}
	for name, tc := range map[string]struct {
		columns []Column
		want    error
	}{
		"zero carriers": {
			columns: []Column{{Name: "v", Role: RoleValue}},
			want:    ErrRoleMissing,
		},
		"two carriers under different names": {
			columns: []Column{timestamp, {Name: "ts2", Role: RoleTimestamp}},
			want:    ErrRoleRepeated,
		},
		"two carriers under the same name": {
			columns: []Column{timestamp, timestamp},
			want:    ErrRoleRepeated,
		},
		"a named and an unnamed carrier": {
			columns: []Column{timestamp, {Role: RoleTimestamp}},
			want:    ErrRoleRepeated,
		},
		"an unnamed carrier": {
			columns: []Column{{Role: RoleTimestamp}, {Name: "v", Role: RoleValue}},
			want:    ErrRoleUnnamed,
		},
		"another role answering to the carrier's name": {
			columns: []Column{timestamp, {Name: "ts", Role: RoleValue}},
			want:    ErrRoleNameShared,
		},
		"an opaque column answering to the carrier's name": {
			columns: []Column{{Name: "ts"}, timestamp},
			want:    ErrRoleNameShared,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			column, err := (Schema{Columns: tc.columns}).UniqueNamedRole(RoleTimestamp)
			if !errors.Is(err, tc.want) {
				t.Fatalf("UniqueNamedRole(%#v) = (%#v, %v), want %v", tc.columns, column, err, tc.want)
			}
			if column != (Column{}) {
				t.Fatalf("a failed resolution must return the zero column, got %#v", column)
			}
		})
	}
}

func TestUniqueNamedRoleIgnoresUnrelatedNameCollisions(t *testing.T) {
	t.Parallel()

	// Two opaque columns sharing a name say nothing about the timestamp
	// carrier; the contract is about the carrier's name only.
	row := Schema{Columns: []Column{
		{Name: "dup"},
		{Name: "dup"},
		{Name: "ts", Role: RoleTimestamp},
	}}
	if _, err := row.UniqueNamedRole(RoleTimestamp); err != nil {
		t.Fatalf("an unrelated repeated name must not fail the resolution: %v", err)
	}
}

func TestUniqueNamedHistogramFieldResolvesThePayloadCarrier(t *testing.T) {
	t.Parallel()

	row := Schema{Columns: append([]Column{{Name: "ts", Role: RoleTimestamp}}, HistogramPayloadColumns()...)}
	for _, want := range HistogramPayloadColumns() {
		column, err := row.UniqueNamedHistogramField(want.HistogramField)
		if err != nil {
			t.Fatalf("UniqueNamedHistogramField(%s): %v", want.HistogramField, err)
		}
		if column != want {
			t.Fatalf("UniqueNamedHistogramField(%s) = %#v, want %#v", want.HistogramField, column, want)
		}
	}
}

func TestUniqueNamedHistogramFieldFailsClosedOnEveryMalformedShape(t *testing.T) {
	t.Parallel()

	count := Column{Name: "configured_count", Role: RoleHistogramField, HistogramField: HistogramFieldCount}
	for name, tc := range map[string]struct {
		columns []Column
		field   HistogramField
		want    error
	}{
		"zero carriers": {
			columns: []Column{{Name: "ts", Role: RoleTimestamp}},
			field:   HistogramFieldCount,
			want:    ErrRoleMissing,
		},
		"the none field": {
			columns: []Column{count},
			field:   HistogramFieldNone,
			want:    ErrRoleMissing,
		},
		"a field past the payload vocabulary": {
			columns: []Column{count},
			field:   HistogramFieldExplicitBounds + 1,
			want:    ErrRoleMissing,
		},
		"two carriers": {
			columns: []Column{count, count},
			field:   HistogramFieldCount,
			want:    ErrRoleRepeated,
		},
		"an unnamed carrier": {
			columns: []Column{{Role: RoleHistogramField, HistogramField: HistogramFieldCount}},
			field:   HistogramFieldCount,
			want:    ErrRoleUnnamed,
		},
		"a carrier under a non-histogram role": {
			columns: []Column{{Name: "count", Role: RoleValue, HistogramField: HistogramFieldCount}},
			field:   HistogramFieldCount,
			want:    ErrHistogramFieldRole,
		},
		"another field answering to the carrier's name": {
			columns: []Column{count, {Name: "configured_count", Role: RoleHistogramField, HistogramField: HistogramFieldSum}},
			field:   HistogramFieldCount,
			want:    ErrRoleNameShared,
		},
		"another role answering to the carrier's name": {
			columns: []Column{count, {Name: "configured_count", Role: RoleValue}},
			field:   HistogramFieldCount,
			want:    ErrRoleNameShared,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			column, err := (Schema{Columns: tc.columns}).UniqueNamedHistogramField(tc.field)
			if !errors.Is(err, tc.want) {
				t.Fatalf("UniqueNamedHistogramField(%#v, %s) = (%#v, %v), want %v", tc.columns, tc.field, column, err, tc.want)
			}
			if column != (Column{}) {
				t.Fatalf("a failed resolution must return the zero column, got %#v", column)
			}
		})
	}
}

// FindHistogramField is the boolean view of UniqueNamedHistogramField; the
// two must never disagree on which schemas resolve.
func TestFindHistogramFieldIsTheBooleanViewOfUniqueNamedHistogramField(t *testing.T) {
	t.Parallel()

	count := Column{Name: "configured_count", Role: RoleHistogramField, HistogramField: HistogramFieldCount}
	for _, columns := range [][]Column{
		{count},
		{count, count},
		{{Role: RoleHistogramField, HistogramField: HistogramFieldCount}},
		{{Name: "count", Role: RoleOpaque, HistogramField: HistogramFieldCount}},
		{count, {Name: "configured_count", Role: RoleHistogramField, HistogramField: HistogramFieldSum}},
		{count, {Name: "configured_count", Role: RoleValue}},
	} {
		row := Schema{Columns: columns}
		wantColumn, err := row.UniqueNamedHistogramField(HistogramFieldCount)
		gotColumn, ok := row.FindHistogramField(HistogramFieldCount)
		if ok != (err == nil) || gotColumn != wantColumn {
			t.Fatalf("FindHistogramField(%#v) = (%#v, %v) disagrees with UniqueNamedHistogramField = (%#v, %v)",
				columns, gotColumn, ok, wantColumn, err)
		}
	}
}

func TestRoleAndHistogramFieldSpellings(t *testing.T) {
	t.Parallel()

	for role := RoleOpaque; role <= RoleParentSpanID; role++ {
		if s := role.String(); s == "" || s == unknownRoleSpelling {
			t.Errorf("ColumnRole(%d) has no spelling", role)
		}
	}
	if s := (RoleParentSpanID + 1).String(); s != unknownRoleSpelling {
		t.Errorf("a role past the vocabulary spells %q, want %q", s, unknownRoleSpelling)
	}
	for field := HistogramFieldNone; field <= HistogramFieldExplicitBounds; field++ {
		if s := field.String(); s == "" || s == unknownRoleSpelling {
			t.Errorf("HistogramField(%d) has no spelling", field)
		}
	}
	if s := (HistogramFieldExplicitBounds + 1).String(); s != unknownRoleSpelling {
		t.Errorf("a field past the vocabulary spells %q, want %q", s, unknownRoleSpelling)
	}
}

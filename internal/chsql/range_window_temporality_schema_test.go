package chsql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestRangeWindowTemporalityResolvesChildSchema(t *testing.T) {
	input := &chplan.Scan{
		Table:   "samples",
		Columns: []string{"physical_temporality"},
		Roles:   []chplan.Column{{Name: "physical_temporality", Role: chplan.RoleTemporality}},
	}
	r := &chplan.RangeWindow{Input: input}
	if err := validateRangeWindowTemporality(r); err != nil {
		t.Fatalf("validate temporality schema: %v", err)
	}
	if got := rangeWindowTemporalityColumn(r); got != "physical_temporality" {
		t.Fatalf("temporality column = %q, want physical_temporality", got)
	}
}

func TestRangeWindowTemporalityRejectsMalformedChildSchema(t *testing.T) {
	tests := map[string]*chplan.Scan{
		"open": {Table: "samples", Roles: []chplan.Column{{Name: "temporality", Role: chplan.RoleTemporality}}},
		"duplicate": {
			Table: "samples", Columns: []string{"a", "b"},
			Roles: []chplan.Column{{Name: "a", Role: chplan.RoleTemporality}, {Name: "b", Role: chplan.RoleTemporality}},
		},
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			r := &chplan.RangeWindow{Input: input}
			if err := validateRangeWindowTemporality(r); err == nil {
				t.Fatal("expected malformed temporality schema to fail")
			}
		})
	}
}

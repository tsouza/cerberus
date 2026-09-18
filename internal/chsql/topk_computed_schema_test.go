package chsql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func TestComputedTopKScalarValueRole(t *testing.T) {
	for _, tc := range []struct {
		name    string
		columns []chplan.Column
		wantErr error
	}{
		{"custom", []chplan.Column{{Name: "sample_value", Role: chplan.RoleValue}}, nil},
		{"value column is not first", []chplan.Column{{Name: "req_id", Role: chplan.RoleAttributes}, {Name: "sample_value", Role: chplan.RoleValue}}, nil},
		// K's scalar schema resolves through chplan.Schema.UniqueNamedRole;
		// each malformed shape below surfaces that resolver's own sentinel.
		{"missing", []chplan.Column{{Name: "Value"}}, chplan.ErrRoleMissing},
		{"unnamed", []chplan.Column{{Role: chplan.RoleValue}}, chplan.ErrRoleUnnamed},
		{"multiple", []chplan.Column{{Name: "first", Role: chplan.RoleValue}, {Name: "second", Role: chplan.RoleValue}}, chplan.ErrRoleRepeated},
		{"duplicate", []chplan.Column{{Name: "sample_value", Role: chplan.RoleValue}, {Name: "sample_value", Role: chplan.RoleValue}}, chplan.ErrRoleRepeated},
		{"ambiguous", []chplan.Column{{Name: "sample_value", Role: chplan.RoleValue}, {Name: "sample_value", Role: chplan.RoleAttributes}}, chplan.ErrRoleNameShared},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := &chplan.TopK{
				Input:     &chplan.Scan{Table: "samples"},
				KExpr:     &chplan.Scan{Table: "scalar_input", Roles: tc.columns},
				Unordered: true,
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error=%v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(sql, "toFloat64(`sample_value`)") {
				t.Fatalf("scalar value schema ignored: %s", sql)
			}
			if !strings.Contains(sql, "* EXCEPT (`_rn`)") {
				t.Fatalf("internal rank leaks into output: %s", sql)
			}
		})
	}
}

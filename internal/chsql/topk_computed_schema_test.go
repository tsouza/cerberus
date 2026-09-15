package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func TestComputedTopKScalarValueRole(t *testing.T) {
	for _, tc := range []struct {
		name      string
		columns   []chplan.Column
		wantError string
	}{
		{"custom", []chplan.Column{{Name: "sample_value", Role: chplan.RoleValue}}, ""},
		// The scalar-value scan below to a non-value column, so the
		// role-finding loop's FIRST iteration must not stop the search:
		// only `continue` past it reaches the RoleValue column on the
		// second iteration. Inverting that `continue` to a `break` exits
		// on the first (non-value) column and leaves kValueColumn empty,
		// turning this accepted shape into the "value role is missing"
		// rejection below.
		{"value column is not first", []chplan.Column{{Name: "req_id", Role: chplan.RoleAttributes}, {Name: "sample_value", Role: chplan.RoleValue}}, ""},
		{"missing", []chplan.Column{{Name: "Value"}}, "value role is missing"},
		{"unnamed", []chplan.Column{{Role: chplan.RoleValue}}, "one named scalar value column"},
		{"multiple", []chplan.Column{{Name: "first", Role: chplan.RoleValue}, {Name: "second", Role: chplan.RoleValue}}, "one named scalar value column"},
		{"duplicate", []chplan.Column{{Name: "sample_value", Role: chplan.RoleValue}, {Name: "sample_value", Role: chplan.RoleValue}}, "one named scalar value column"},
		{"ambiguous", []chplan.Column{{Name: "sample_value", Role: chplan.RoleValue}, {Name: "sample_value", Role: chplan.RoleAttributes}}, "value column is ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := &chplan.TopK{
				Input:     &chplan.Scan{Table: "samples"},
				KExpr:     &chplan.Scan{Table: "scalar_input", Roles: tc.columns},
				Unordered: true,
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error=%v, want %q", err, tc.wantError)
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

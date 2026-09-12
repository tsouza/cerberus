package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func staleResampleWithRoles(roles []chplan.Column) *chplan.RangeWindowStaleResample {
	columns := make([]string, len(roles))
	for i, role := range roles {
		columns[i] = role.Name
	}
	return &chplan.RangeWindowStaleResample{
		Input:    &chplan.Scan{Table: "samples", Columns: columns, Roles: roles},
		Start:    time.Unix(1000, 0).UTC(),
		End:      time.Unix(1060, 0).UTC(),
		Step:     time.Minute,
		Lookback: 5 * time.Minute,
	}
}

func staleResampleTestInput() chplan.Node {
	return staleResampleWithRoles([]chplan.Column{
		{Name: "MetricName", Role: chplan.RoleMetricName},
		{Name: "Attributes", Role: chplan.RoleAttributes},
		{Name: "TimeUnix", Role: chplan.RoleTimestamp},
		{Name: "Value", Role: chplan.RoleValue},
	}).Input
}

func TestRangeWindowStaleResampleResolvesChildSchemaColumns(t *testing.T) {
	t.Parallel()
	roles := []chplan.Column{
		{Name: "source_name", Role: chplan.RoleMetricName},
		{Name: "source_labels", Role: chplan.RoleAttributes},
		{Name: "source_time", Role: chplan.RoleTimestamp},
		{Name: "source_value", Role: chplan.RoleValue},
	}
	plan := staleResampleWithRoles(roles)

	if got := plan.RowType(); !got.Equal(chplan.Schema{Columns: roles}) {
		t.Fatalf("RowType() = %#v, want child-declared sample roles %#v", got, roles)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, fragment := range []string{
		"(`source_time`, `source_value`)",
		"GROUP BY `source_name`, `source_labels`",
		"toDateTime64(`anchor_ts`, 9) AS `source_time`",
		"AS `source_value`",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("SQL missing child-schema-derived fragment %q: %s", fragment, sql)
		}
	}
}

func TestRangeWindowStaleResampleRejectsInvalidChildSchema(t *testing.T) {
	t.Parallel()
	valid := []chplan.Column{
		{Name: "name", Role: chplan.RoleMetricName},
		{Name: "labels", Role: chplan.RoleAttributes},
		{Name: "time", Role: chplan.RoleTimestamp},
		{Name: "value", Role: chplan.RoleValue},
	}
	tests := []struct {
		name  string
		roles []chplan.Column
		open  bool
	}{
		{name: "open", roles: valid, open: true},
		{name: "missing", roles: valid[:3]},
		{name: "unnamed", roles: append(append([]chplan.Column{}, valid[:3]...), chplan.Column{Role: chplan.RoleValue})},
		{name: "duplicate", roles: append(append([]chplan.Column{}, valid...), chplan.Column{Name: "other_value", Role: chplan.RoleValue})},
		{name: "role_conflict", roles: append(append([]chplan.Column{}, valid...), chplan.Column{Name: "value", Role: chplan.RoleTimestamp})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := staleResampleWithRoles(tc.roles)
			if tc.open {
				plan.Input.(*chplan.Scan).Columns = nil
			}
			if _, _, err := chsql.Emit(context.Background(), plan); err == nil {
				t.Fatal("Emit unexpectedly accepted invalid child schema")
			}
		})
	}
}

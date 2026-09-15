package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

func rangeLWRRoleProject(columns ...chplan.Column) chplan.Node {
	projections := make([]chplan.Projection, len(columns))
	for i, column := range columns {
		projections[i] = chplan.Projection{Expr: &chplan.LitFloat{V: 1}, Alias: column.Name}
	}
	return &chplan.Project{Input: &chplan.OneRow{}, Roles: columns, Projections: projections}
}

func rangeLWRInternalTestInput(table string) chplan.Node {
	columns := []chplan.Column{
		{Name: "MetricName", Role: chplan.RoleMetricName},
		{Name: "Attributes", Role: chplan.RoleAttributes},
		{Name: "TimeUnix", Role: chplan.RoleTimestamp},
		{Name: "Value", Role: chplan.RoleValue},
	}
	projections := make([]chplan.Projection, len(columns))
	for i, column := range columns {
		projections[i] = chplan.Projection{Expr: &chplan.ColumnRef{Name: column.Name}, Alias: column.Name}
	}
	return &chplan.Project{Input: &chplan.Scan{Table: table}, Roles: columns, Projections: projections}
}

func TestRangeLWRResolvesPhysicalInputRolesAndPreservesOutputAliases(t *testing.T) {
	t.Parallel()
	input := rangeLWRRoleProject(
		chplan.Column{Name: "physical_metric", Role: chplan.RoleMetricName},
		chplan.Column{Name: "physical_attributes", Role: chplan.RoleAttributes},
		chplan.Column{Name: "physical_timestamp", Role: chplan.RoleTimestamp},
		chplan.Column{Name: "physical_value", Role: chplan.RoleValue},
	)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := &chplan.RangeLWR{
		Input: input, Start: at, End: at, Step: time.Minute, Lookback: 5 * time.Minute,
		MetricNameCol: "MetricName", AttributesCol: "Attributes", TimestampCol: "TimeUnix", ValueCol: "Value",
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"argMax(`physical_value`, `physical_timestamp`)",
		"GROUP BY `physical_metric`, `physical_attributes`, `anchor_ts`",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL does not use resolved child roles in %q: %s", want, sql)
		}
	}
	for _, public := range []string{"MetricName", "Attributes", "TimeUnix", "Value"} {
		if !strings.Contains(sql, "AS `"+public+"`") {
			t.Errorf("SQL does not preserve output alias %q: %s", public, sql)
		}
	}
}

func TestRangeLWRRejectsMalformedChildSchema(t *testing.T) {
	t.Parallel()
	canonical := []chplan.Column{
		{Name: "metric", Role: chplan.RoleMetricName},
		{Name: "attributes", Role: chplan.RoleAttributes},
		{Name: "timestamp", Role: chplan.RoleTimestamp},
		{Name: "value", Role: chplan.RoleValue},
	}
	tests := map[string]chplan.Node{
		"open": &chplan.Scan{Roles: canonical},
		"missing": rangeLWRRoleProject(
			canonical[0], canonical[1], canonical[2],
		),
		"duplicate": rangeLWRRoleProject(
			canonical[0], canonical[1], canonical[2], canonical[3],
			chplan.Column{Name: "other_value", Role: chplan.RoleValue},
		),
		"unnamed": rangeLWRRoleProject(
			canonical[0], canonical[1], canonical[2], chplan.Column{Role: chplan.RoleValue},
		),
		"ambiguous": rangeLWRRoleProject(
			canonical[0], canonical[1], canonical[2], canonical[3],
			chplan.Column{Name: "timestamp", Role: chplan.RoleValue},
		),
	}
	for name, child := range tests {
		child := child
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan := &chplan.RangeLWR{
				Input: child, Step: time.Minute,
				MetricNameCol: "MetricName", AttributesCol: "Attributes", TimestampCol: "TimeUnix", ValueCol: "Value",
			}
			if _, _, err := Emit(context.Background(), plan); err == nil {
				t.Fatal("RangeLWR accepted a malformed child schema")
			}
		})
	}
}

func TestRangeLWRFusionDeclinesRenamedChildRoles(t *testing.T) {
	t.Parallel()
	lwr := rangeLWRFusionTestLWR()
	lwr.Input = rangeLWRRoleProject(
		chplan.Column{Name: "physical_metric", Role: chplan.RoleMetricName},
		chplan.Column{Name: "physical_attributes", Role: chplan.RoleAttributes},
		chplan.Column{Name: "physical_timestamp", Role: chplan.RoleTimestamp},
		chplan.Column{Name: "physical_value", Role: chplan.RoleValue},
	)
	if _, kind := matchRangeLWRFusion(rangeLWRFusionTestAggregate(chplan.FnSum, lwr)); kind != rangeLWRFusionNone {
		t.Fatalf("matchRangeLWRFusion with renamed child roles = %v, want no fusion", kind)
	}
}

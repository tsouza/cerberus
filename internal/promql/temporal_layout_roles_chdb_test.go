//go:build chdb

package promql

import (
	"context"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

const (
	temporalRoleTestMetric         = "live_metric"
	temporalRoleTestSeries         = "live_series"
	temporalRoleTestTimestampNanos = int64(1_767_225_600_000_000_000)
	temporalRoleTestValue          = 7.5
)

func TestTemporalRoleAdaptersExecuteRenamedInputs_ChDB(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	s.MetricNameColumn = "public_name"
	s.AttributesColumn = "public_labels"
	s.TimestampColumn = "public_time"
	s.ValueColumn = "public_value"
	input := temporalRoleTestInput(s)

	for _, tc := range []struct {
		name          string
		adapt         func(chplan.Node, schema.Metrics) (chplan.Node, error)
		discriminator bool
	}{
		{name: "aggregate canonicalization", adapt: canonicalizeMixedFloatArmForAgg},
		{name: "mixed join widening", adapt: widenPlainVectorToMixedShape, discriminator: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := tc.adapt(input, s)
			if err != nil {
				t.Fatal(err)
			}
			emitted, emittedArgs, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			columns := []chsql.Frag{
				chsql.Col(s.MetricNameColumn),
				chsql.Call(string(chplan.FnArrayElement), chsql.Col(s.AttributesColumn), chsql.Lit("series")),
				chsql.Call(string(chplan.FnToUnixNanos), chsql.Col(s.TimestampColumn)),
				chsql.Col(s.ValueColumn),
			}
			if tc.discriminator {
				columns = append(columns, chsql.Col(mixedDiscriminatorColumn))
			}
			query, args := chsql.NewQuery().
				Select(columns...).
				From(chsql.Subquery(chsql.PreRenderedSQL{SQL: emitted, Args: emittedArgs})).
				Build()

			var (
				metric        string
				series        string
				timestamp     int64
				value         float64
				discriminator int64
			)
			row := spec.OpenChDB(t).QueryRow(query, args...)
			if tc.discriminator {
				err = row.Scan(&metric, &series, &timestamp, &value, &discriminator)
			} else {
				err = row.Scan(&metric, &series, &timestamp, &value)
			}
			if err != nil {
				t.Fatal(err)
			}
			if metric != temporalRoleTestMetric || series != temporalRoleTestSeries ||
				timestamp != temporalRoleTestTimestampNanos || value != temporalRoleTestValue {
				t.Fatalf("row = (%q, %q, %d, %v), want (%q, %q, %d, %v)",
					metric, series, timestamp, value,
					temporalRoleTestMetric, temporalRoleTestSeries, temporalRoleTestTimestampNanos, temporalRoleTestValue)
			}
			if tc.discriminator && discriminator != mixedDiscriminatorFloat {
				t.Fatalf("discriminator = %d, want %d", discriminator, mixedDiscriminatorFloat)
			}
		})
	}
}

func temporalRoleTestInput(s schema.Metrics) chplan.Node {
	const (
		physicalMetricName = "physical_name"
		physicalAttributes = "physical_labels"
		physicalTimestamp  = "physical_time"
		physicalValue      = "physical_value"
	)
	roles := []chplan.Column{
		{Name: physicalMetricName, Role: chplan.RoleMetricName},
		{Name: s.MetricNameColumn, Role: chplan.RoleOpaque},
		{Name: physicalAttributes, Role: chplan.RoleAttributes},
		{Name: s.AttributesColumn, Role: chplan.RoleOpaque},
		{Name: physicalTimestamp, Role: chplan.RoleTimestamp},
		{Name: s.TimestampColumn, Role: chplan.RoleOpaque},
		{Name: physicalValue, Role: chplan.RoleValue},
		{Name: s.ValueColumn, Role: chplan.RoleOpaque},
	}
	labelMap := func(value string) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnMap, Args: []chplan.Expr{
			&chplan.InlineString{V: "series"},
			&chplan.LitString{V: value},
		}}
	}
	timestamp := func(nanos int64) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnFromUnixNanos, Args: []chplan.Expr{&chplan.LitInt{V: nanos}}}
	}
	return &chplan.Project{
		Input: &chplan.OneRow{},
		Roles: roles,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: temporalRoleTestMetric}, Alias: physicalMetricName},
			{Expr: &chplan.LitString{V: "decoy_metric"}, Alias: s.MetricNameColumn},
			{Expr: labelMap(temporalRoleTestSeries), Alias: physicalAttributes},
			{Expr: labelMap("decoy_series"), Alias: s.AttributesColumn},
			{Expr: timestamp(temporalRoleTestTimestampNanos), Alias: physicalTimestamp},
			{Expr: timestamp(1), Alias: s.TimestampColumn},
			{Expr: &chplan.LitFloat{V: temporalRoleTestValue}, Alias: physicalValue},
			{Expr: &chplan.LitFloat{V: -1}, Alias: s.ValueColumn},
		},
	}
}

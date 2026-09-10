package promql

import (
	"context"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestRowTypeSyntheticSampleRoles(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	s.MetricNameColumn = "custom_name"
	s.AttributesColumn = "custom_labels"
	s.TimestampColumn = "custom_time"
	s.ValueColumn = "custom_value"
	for _, query := range []string{"time()", "vector(3)", "abs(vector(3))", "vector(scalar(up))", "sum(up)"} {
		t.Run(query, func(t *testing.T) {
			expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range metricRoles(s) {
				got, ok := plan.RowType().ByName(want.Name)
				if !ok || got.Role != want.Role {
					t.Errorf("query %s role %s = %#v, %v; want %#v", query, want.Name, got, ok, want)
				}
			}
		})
	}
}

func TestRowTypeHistogramStorageRoles(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	s.CountColumn = "custom_count"
	s.PositiveBucketCountsColumn = "custom_positive"
	for _, table := range []string{s.HistogramTable, s.ExpHistogramTable} {
		roles := chplan.Schema{Columns: metricScanRoles(s, table)}
		if roles.Has(chplan.RoleValue) {
			t.Errorf("histogram %s falsely declares float Value", table)
		}
		count, ok := roles.ByName(s.CountColumn)
		if !ok || count.Role != chplan.RoleHistogramField {
			t.Errorf("histogram count role: %#v", count)
		}
		if table == s.ExpHistogramTable {
			positive, ok := roles.ByName(s.PositiveBucketCountsColumn)
			if !ok || positive.Role != chplan.RoleHistogramField {
				t.Errorf("native bucket role: %#v", positive)
			}
		}
	}
}

func TestRowTypeOptimizerPreservesDeclarations(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	plan := &chplan.Project{Input: &chplan.OneRow{}, Roles: metricRoles(s), Projections: []chplan.Projection{{
		Expr: &chplan.Binary{Op: chplan.OpAdd, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 2}}, Alias: s.ValueColumn,
	}}}
	want := plan.RowType()
	optimized := optimizer.Default().Run(context.Background(), plan)
	if got := optimized.RowType(); !got.Equal(want) {
		t.Fatalf("optimizer lost output declaration: got %#v, want %#v", got, want)
	}
}

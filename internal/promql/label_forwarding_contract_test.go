package promql

import (
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestAttributeRewriteUnnamedFloatPhysicalColumns(t *testing.T) {
	for _, custom := range []bool{false, true} {
		s := schema.DefaultOTelMetrics()
		name := "default"
		if custom {
			name = "custom"
			s.MetricNameColumn = "metric_id"
			s.AttributesColumn = "arbitrary_labels"
			s.TimestampColumn = "sample_time"
			s.ValueColumn = "sample_value"
		}
		t.Run(name, func(t *testing.T) {
			attributes := chplan.Column{Name: s.AttributesColumn, Role: chplan.RoleAttributes}
			value := chplan.Column{Name: s.ValueColumn, Role: chplan.RoleValue}
			timestamp := chplan.Column{Name: s.TimestampColumn, Role: chplan.RoleTimestamp}
			anchor := chplan.Column{Name: chplan.RangeWindowAnchorColumn, Role: chplan.RoleAnchor}
			metric := chplan.Column{Name: s.MetricNameColumn, Role: chplan.RoleMetricName}
			canonical := []string{s.MetricNameColumn, s.AttributesColumn, s.TimestampColumn, s.ValueColumn}
			for _, tc := range []struct {
				name      string
				columns   []chplan.Column
				open      bool
				emptyName bool
			}{
				{name: "grid", columns: []chplan.Column{attributes, anchor, timestamp, value}, emptyName: true},
				{name: "timestamp_without_anchor", columns: []chplan.Column{attributes, timestamp, value}, emptyName: true},
				{name: "anchor_without_timestamp", columns: []chplan.Column{attributes, anchor, value}},
				{name: "neither_timestamp", columns: []chplan.Column{attributes, value}},
				{name: "name_preserved", columns: []chplan.Column{metric, attributes, timestamp, value}},
				{name: "opaque_name_preserved", columns: []chplan.Column{{Name: s.MetricNameColumn}, attributes, timestamp, value}},
				{name: "other_metric_role_preserved", columns: []chplan.Column{{Name: "another_metric", Role: chplan.RoleMetricName}, attributes, timestamp, value}},
				{name: "open_unknown_name", columns: []chplan.Column{attributes, anchor, timestamp, value}, open: true},
				{name: "opaque_outputs", columns: []chplan.Column{{Name: s.AttributesColumn}, {Name: s.ValueColumn}}},
				{name: "missing_attributes", columns: []chplan.Column{timestamp, value}},
				{name: "missing_value", columns: []chplan.Column{attributes, timestamp}},
				{name: "opaque_attributes", columns: []chplan.Column{{Name: s.AttributesColumn}, timestamp, value}},
				{name: "opaque_timestamp", columns: []chplan.Column{attributes, {Name: s.TimestampColumn}, value}},
				{name: "opaque_value", columns: []chplan.Column{attributes, timestamp, {Name: s.ValueColumn}}},
				{name: "histogram_payload_retains_legacy_boundary", columns: append([]chplan.Column{attributes, anchor, timestamp, value}, chplan.HistogramPayloadColumns()...)},
				{name: "histogram_helper_retains_legacy_boundary", columns: []chplan.Column{attributes, timestamp, value, {Name: chplan.HistogramCountColumn, Role: chplan.RoleHistogramField}}},
				{name: "discriminator_retains_legacy_boundary", columns: []chplan.Column{attributes, timestamp, value, {Name: "sample_kind", Role: chplan.RoleDiscriminator}}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					scan := &chplan.Scan{Roles: slices.Clone(tc.columns)}
					inner := &chplan.Project{Input: scan}
					if !tc.open {
						for _, column := range tc.columns {
							scan.Columns = append(scan.Columns, column.Name)
							inner.Projections = append(inner.Projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: column.Name}})
						}
					}
					if got := chplan.RowShapeOf(inner); got != chplan.SampleRowShape {
						t.Fatalf("fixture legacy shape = %v, want Sample", got)
					}
					newAttrs := &chplan.MapWithoutKeys{Map: &chplan.ColumnRef{Name: s.AttributesColumn}, Keys: []string{"remove_me"}}
					project := projectAttributesOverInner(inner, s, newAttrs)
					if project.Input != inner {
						t.Fatal("rewrite changed its input")
					}
					var names []string
					for _, projection := range project.Projections {
						names = append(names, chplan.ProjectionOutputName(projection))
					}
					if !slices.Equal(names, canonical) {
						t.Fatalf("forwarded columns = %v, want %v", names, canonical)
					}
					if project.Projections[1].Alias != s.AttributesColumn || project.Projections[1].Expr != newAttrs {
						t.Fatal("rewrite did not preserve the requested attributes expression and alias")
					}
					nameProjection := project.Projections[0]
					if tc.emptyName {
						literal, ok := nameProjection.Expr.(*chplan.LitString)
						if !ok || literal.V != "" || nameProjection.Alias != s.MetricNameColumn {
							t.Fatalf("name projection = %#v, want empty string AS %s", nameProjection, s.MetricNameColumn)
						}
					} else {
						column, ok := nameProjection.Expr.(*chplan.ColumnRef)
						if !ok || column.Name != s.MetricNameColumn || nameProjection.Alias != "" {
							t.Fatalf("name projection = %#v, want unchanged %s reference", nameProjection, s.MetricNameColumn)
						}
					}
				})
			}
		})
	}
}

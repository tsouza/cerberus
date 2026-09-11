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
				wantPanic bool
				nameRef   string
			}{
				{name: "grid", columns: []chplan.Column{attributes, anchor, timestamp, value}, emptyName: true},
				{name: "timestamp_without_anchor", columns: []chplan.Column{attributes, timestamp, value}, emptyName: true},
				{name: "anchor_without_timestamp", columns: []chplan.Column{attributes, anchor, value}, wantPanic: true},
				{name: "neither_timestamp", columns: []chplan.Column{attributes, value}, wantPanic: true},
				{name: "name_preserved", columns: []chplan.Column{metric, attributes, timestamp, value}},
				{name: "opaque_name_preserved", columns: []chplan.Column{{Name: s.MetricNameColumn}, attributes, timestamp, value}},
				{name: "other_metric_role_preserved", columns: []chplan.Column{{Name: "another_metric", Role: chplan.RoleMetricName}, attributes, timestamp, value}, nameRef: "another_metric"},
				{name: "open_unknown_name", columns: []chplan.Column{attributes, anchor, timestamp, value}, open: true, wantPanic: true},
				{name: "opaque_outputs", columns: []chplan.Column{{Name: s.AttributesColumn}, {Name: s.ValueColumn}}, wantPanic: true},
				{name: "missing_attributes", columns: []chplan.Column{timestamp, value}, wantPanic: true},
				{name: "missing_value", columns: []chplan.Column{attributes, timestamp}, wantPanic: true},
				{name: "opaque_attributes", columns: []chplan.Column{{Name: s.AttributesColumn}, timestamp, value}, wantPanic: true},
				{name: "opaque_timestamp", columns: []chplan.Column{attributes, {Name: s.TimestampColumn}, value}, wantPanic: true},
				{name: "opaque_value", columns: []chplan.Column{attributes, timestamp, {Name: s.ValueColumn}}, wantPanic: true},
				{name: "histogram_payload_is_not_value_forwardable", columns: append([]chplan.Column{attributes, anchor, timestamp, value}, chplan.HistogramPayloadColumns()...), wantPanic: true},
				{name: "partial_histogram_is_not_value_forwardable", columns: []chplan.Column{attributes, timestamp, value, {Name: chplan.HistogramCountColumn, Role: chplan.RoleHistogramField}}, wantPanic: true},
				{name: "orphan_discriminator_is_not_value_forwardable", columns: []chplan.Column{attributes, timestamp, value, {Name: "sample_kind", Role: chplan.RoleDiscriminator}}, wantPanic: true},
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
					var newAttrs chplan.Expr
					var project *chplan.Project
					call := func() {
						project = mustProjectAttributesOverInner(t, inner, s, func(refs sampleRoleRefs) chplan.Expr {
							if tc.wantPanic {
								t.Fatal("invalid role contract reached the rewrite callback")
							}
							newAttrs = &chplan.MapWithoutKeys{Map: refs.Attributes, Keys: []string{"remove_me"}}
							return newAttrs
						})
					}
					if tc.wantPanic {
						// Keep every malformed-shape control: the role-driven API
						// now fails closed instead of constructing invalid references.
						capturePanic(t, call)
						return
					}
					call()
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
						wantName, wantAlias := s.MetricNameColumn, ""
						if tc.nameRef != "" {
							wantName, wantAlias = tc.nameRef, s.MetricNameColumn
						}
						if !ok || column.Name != wantName || nameProjection.Alias != wantAlias {
							t.Fatalf("name projection = %#v, want %s reference with alias %q", nameProjection, wantName, wantAlias)
						}
					}
				})
			}
		})
	}
}

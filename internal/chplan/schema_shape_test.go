package chplan

import (
	"slices"
	"testing"
)

func TestRowShapeFromValidatedSchema(t *testing.T) {
	name := Column{Name: "metric", Role: RoleMetricName}
	attributes := Column{Name: "labels", Role: RoleAttributes}
	timestamp := Column{Name: "sample_time", Role: RoleTimestamp}
	anchor := Column{Name: "step", Role: RoleAnchor}
	value := Column{Name: "value", Role: RoleValue}
	discriminator := Column{Name: "sample_kind", Role: RoleDiscriminator}
	float := []Column{name, attributes, timestamp, value}
	histogram := HistogramPayloadColumns()
	mixed := append(slices.Clone(float), histogram...)
	mixed = append(mixed, discriminator)

	for _, tc := range []struct {
		name   string
		schema Schema
		want   RowShape
	}{
		{name: "empty", schema: Schema{}, want: SampleRowShape},
		{name: "strict_float", schema: Schema{Columns: float}, want: SampleRowShape},
		{name: "strict_reduced", schema: Schema{Columns: []Column{attributes, value}}, want: ReducedWindowRowShape},
		{name: "named_reduced", schema: Schema{Columns: []Column{name, attributes, value}}, want: ReducedWindowRowShape},
		{name: "strict_grid", schema: Schema{Columns: []Column{attributes, anchor, timestamp, value}}, want: GridWindowRowShape},
		{name: "named_grid", schema: Schema{Columns: []Column{name, attributes, anchor, timestamp, value}}, want: GridWindowRowShape},
		{name: "partial_value", schema: Schema{Columns: []Column{value}}, want: SampleRowShape},
		{name: "partial_anchor", schema: Schema{Columns: []Column{anchor}}, want: SampleRowShape},
		{name: "histogram", schema: Schema{Columns: histogram}, want: HistogramRowShape},
		{name: "histogram_with_float_columns", schema: Schema{Columns: append(slices.Clone(float), histogram...)}, want: HistogramRowShape},
		{name: "mixed", schema: Schema{Columns: mixed}, want: MixedRowShape},
		{name: "opaque_join", schema: Schema{Columns: []Column{{Name: "_hq_L_HistogramCount"}, {Name: "_mvj_R_Value"}}}, want: SampleRowShape},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RowShapeFromSchema(tc.schema); got != tc.want {
				t.Fatalf("RowShapeFromSchema = %s, want %s; schema=%#v", got, tc.want, tc.schema)
			}
		})
	}
}

func TestRowShapeFromSchemaRejectsUnvalidatedContracts(t *testing.T) {
	attributes := Column{Name: "labels", Role: RoleAttributes}
	anchor := Column{Name: "step", Role: RoleAnchor}
	value := Column{Name: "value", Role: RoleValue}
	discriminator := Column{Name: "sample_kind", Role: RoleDiscriminator}
	histogram := HistogramPayloadColumns()
	partialHistogram := slices.Clone(histogram[1:])

	for _, tc := range []struct {
		name   string
		schema Schema
	}{
		{name: "open_reduced", schema: Schema{Open: true, Columns: []Column{attributes, value}}},
		{name: "open_grid", schema: Schema{Open: true, Columns: []Column{attributes, anchor, value}}},
		{name: "open_histogram", schema: Schema{Open: true, Columns: histogram}},
		{name: "partial_histogram", schema: Schema{Columns: partialHistogram}},
		{name: "orphan_discriminator", schema: Schema{Columns: []Column{attributes, value, discriminator}}},
		{name: "duplicate_value_role", schema: Schema{Columns: []Column{attributes, value, {Name: "other_value", Role: RoleValue}}}},
		{name: "unnamed_value", schema: Schema{Columns: []Column{attributes, {Role: RoleValue}}}},
		{name: "ambiguous_value_name", schema: Schema{Columns: []Column{attributes, value, {Name: value.Name}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RowShapeFromSchema(tc.schema); got != SampleRowShape {
				t.Fatalf("RowShapeFromSchema = %s, want sample default; schema=%#v", got, tc.schema)
			}
		})
	}
}

func TestRowShapeOfUsesPhysicalSchemaNotLiveKind(t *testing.T) {
	roles := []Column{
		{Name: "metric", Role: RoleMetricName},
		{Name: "labels", Role: RoleAttributes},
		{Name: "sample_time", Role: RoleTimestamp},
		{Name: "value", Role: RoleValue},
	}
	roles = append(roles, HistogramPayloadColumns()...)
	roles = append(roles, Column{Name: MixedDiscriminatorColumn, Role: RoleDiscriminator})
	names := make([]string, len(roles))
	for i, role := range roles {
		names[i] = role.Name
	}
	mixed := &Scan{Table: "mixed", Columns: names, Roles: roles}
	narrowed := &Filter{
		Input: mixed,
		Predicate: &Binary{
			Op:    OpEq,
			Left:  &ColumnRef{Name: MixedDiscriminatorColumn},
			Right: &LitInt{V: 0},
		},
	}
	if got := LiveSampleKind(narrowed); got != SampleKindFloat {
		t.Fatalf("LiveSampleKind(narrowed) = %s, want float", got)
	}
	if got := RowShapeOf(narrowed); got != MixedRowShape {
		t.Fatalf("RowShapeOf(narrowed) = %s, want physical mixed shape", got)
	}
}

func TestRowShapeOfReconcilesOpaqueAndHistogramJoins(t *testing.T) {
	const (
		metric     = "metric"
		attributes = "labels"
		timestamp  = "sample_time"
		value      = "value"
	)
	for _, tc := range []struct {
		name string
		node Node
		want RowShape
	}{
		{
			name: "histogram_vector_join",
			node: &HistogramVectorJoin{MetricNameColumn: metric, AttributesColumn: attributes, TimestampColumn: timestamp},
			want: SampleRowShape,
		},
		{
			name: "mixed_vector_join",
			node: &MixedVectorJoin{MetricNameColumn: metric, AttributesColumn: attributes, TimestampColumn: timestamp, ValueColumn: value},
			want: SampleRowShape,
		},
		{
			name: "histogram_float_vector_join",
			node: &HistogramFloatVectorJoin{MetricNameColumn: metric, AttributesColumn: attributes, TimestampColumn: timestamp, ValueColumn: value},
			want: HistogramRowShape,
		},
		{name: "empty_union", node: &UnionAll{}, want: SampleRowShape},
		{name: "opaque_default", node: &OneRow{}, want: SampleRowShape},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RowShapeOf(tc.node); got != tc.want {
				t.Fatalf("RowShapeOf(%T) = %s, want %s; schema=%#v", tc.node, got, tc.want, tc.node.RowType())
			}
		})
	}
}

package prom

import (
	"slices"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func schemaProject(columns []chplan.Column) *chplan.Project {
	projections := make([]chplan.Projection, len(columns))
	for i, column := range columns {
		projections[i] = chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: column.Name},
			Alias: column.Name,
		}
	}
	return &chplan.Project{Input: &chplan.OneRow{}, Projections: projections, Roles: columns}
}

func TestWrapWithSampleProjectionOrdersHistogramContractByRoles(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	columns := []chplan.Column{
		{Name: "source_value", Role: chplan.RoleValue},
		{Name: "source_time", Role: chplan.RoleTimestamp},
		{Name: "source_labels", Role: chplan.RoleAttributes},
		{Name: "source_name", Role: chplan.RoleMetricName},
	}
	columns = append(columns, chplan.HistogramPayloadColumns()...)
	input := schemaProject(columns)

	wrapped, err := wrapWithSampleProjection(input, s)
	if err != nil {
		t.Fatalf("wrapWithSampleProjection: %v", err)
	}
	projected, ok := wrapped.(*chplan.Project)
	if !ok || projected == input {
		t.Fatalf("wrapped histogram = %T, want a role-ordered *chplan.Project", wrapped)
	}
	if got := projected.RowType().SampleKind(); got != chplan.SampleKindHistogram {
		t.Fatalf("wrapped kind = %s, want histogram", got)
	}
	want := append(sampleRoleColumns(s), chplan.HistogramPayloadColumns()...)
	if got := projected.RowType(); !got.Equal(chplan.Schema{Columns: want}) {
		t.Fatalf("wrapped schema = %#v, want %#v", got, want)
	}
	for i, source := range []string{"source_name", "source_labels", "source_time", "source_value"} {
		ref, ok := projected.Projections[i].Expr.(*chplan.ColumnRef)
		if !ok || ref.Name != source {
			t.Fatalf("projection[%d] = %#v, want ColumnRef{%q}", i, projected.Projections[i].Expr, source)
		}
	}
}

func TestWrapWithSampleProjectionRealiasesMixedDiscriminator(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	columns := append(slices.Clone(sampleRoleColumns(s)), chplan.HistogramPayloadColumns()...)
	columns = append(columns, chplan.Column{Name: "source_kind", Role: chplan.RoleDiscriminator})

	wrapped, err := wrapWithSampleProjection(schemaProject(columns), s)
	if err != nil {
		t.Fatalf("wrapWithSampleProjection: %v", err)
	}
	projected, ok := wrapped.(*chplan.Project)
	if !ok {
		t.Fatalf("wrapped mixed = %T, want *chplan.Project", wrapped)
	}
	last := projected.Projections[len(projected.Projections)-1]
	ref, ok := last.Expr.(*chplan.ColumnRef)
	if !ok || ref.Name != "source_kind" || last.Alias != chplan.MixedDiscriminatorColumn {
		t.Fatalf("mixed discriminator projection = %#v, want source_kind AS %s", last, chplan.MixedDiscriminatorColumn)
	}
	if got := projected.RowType().SampleKind(); got != chplan.SampleKindMixed {
		t.Fatalf("wrapped kind = %s, want mixed", got)
	}
}

func TestWrapWithSampleProjectionRejectsUntrustedSchemas(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	partialHistogram := append(slices.Clone(sampleRoleColumns(s)), chplan.HistogramPayloadColumns()[:1]...)
	duplicateValue := append(slices.Clone(sampleRoleColumns(s)), chplan.Column{Name: "other_value", Role: chplan.RoleValue})

	for _, tc := range []struct {
		name string
		plan chplan.Node
		kind string
	}{
		{name: "opaque", plan: schemaProject([]chplan.Column{{Name: "private"}}), kind: "opaque"},
		{name: "open", plan: &chplan.Scan{Table: "samples", Roles: sampleRoleColumns(s)}, kind: "opaque"},
		{name: "partial_histogram", plan: schemaProject(partialHistogram), kind: "invalid"},
		{name: "duplicate_value", plan: schemaProject(duplicateValue), kind: "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped, err := wrapWithSampleProjection(tc.plan, s)
			if err == nil || wrapped != nil {
				t.Fatalf("wrapWithSampleProjection = (%T, %v), want nil error result", wrapped, err)
			}
			if !strings.Contains(err.Error(), tc.kind) {
				t.Fatalf("error = %q, want %q schema classification", err, tc.kind)
			}
		})
	}
}

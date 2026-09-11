package promql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestCanonicalizeMixedFloatArmForAggResolvesPhysicalRoles(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	s.MetricNameColumn = "public_name"
	s.AttributesColumn = "public_labels"
	s.TimestampColumn = "public_time"
	s.ValueColumn = "public_value"
	input := sampleForwardTestInput(
		chplan.Column{Name: "physical_name", Role: chplan.RoleMetricName},
		chplan.Column{Name: s.MetricNameColumn, Role: chplan.RoleOpaque},
		chplan.Column{Name: "physical_labels", Role: chplan.RoleAttributes},
		chplan.Column{Name: s.AttributesColumn, Role: chplan.RoleOpaque},
		chplan.Column{Name: "physical_time", Role: chplan.RoleTimestamp},
		chplan.Column{Name: s.TimestampColumn, Role: chplan.RoleOpaque},
		chplan.Column{Name: "physical_value", Role: chplan.RoleValue},
		chplan.Column{Name: s.ValueColumn, Role: chplan.RoleOpaque},
	)

	got, err := canonicalizeMixedFloatArmForAgg(input, s)
	if err != nil {
		t.Fatal(err)
	}
	project, ok := got.(*chplan.Project)
	if !ok {
		t.Fatalf("canonicalized input = %T, want *chplan.Project", got)
	}
	assertTemporalProjectionRef(t, project.Projections[0], "physical_name", s.MetricNameColumn)
	assertTemporalProjectionRef(t, project.Projections[1], "physical_labels", s.AttributesColumn)
	assertTemporalProjectionRef(t, project.Projections[2], "physical_time", s.TimestampColumn)
	assertTemporalProjectionRef(t, project.Projections[3], "physical_value", s.ValueColumn)
	if row := project.RowType(); !row.Equal(chplan.Schema{Columns: metricRoles(s)}) {
		t.Fatalf("canonicalized schema = %#v, want %#v", row, chplan.Schema{Columns: metricRoles(s)})
	}
}

func TestCanonicalizeMixedFloatArmForAggPreservesTemporalEnvelope(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	grid := sampleForwardTestInput(
		chplan.Column{Name: "physical_labels", Role: chplan.RoleAttributes},
		chplan.Column{Name: "physical_anchor", Role: chplan.RoleAnchor},
		chplan.Column{Name: "physical_time", Role: chplan.RoleTimestamp},
		chplan.Column{Name: "physical_value", Role: chplan.RoleValue},
	)
	got, err := canonicalizeMixedFloatArmForAgg(grid, s)
	if err != nil {
		t.Fatal(err)
	}
	project := got.(*chplan.Project)
	if len(project.Projections) != 4 {
		t.Fatalf("grid projection count = %d, want 4", len(project.Projections))
	}
	assertTemporalProjectionRef(t, project.Projections[0], "physical_labels", s.AttributesColumn)
	assertTemporalProjectionRef(t, project.Projections[1], "physical_anchor", chplan.RangeWindowAnchorColumn)
	assertTemporalProjectionRef(t, project.Projections[2], "physical_time", s.TimestampColumn)
	assertTemporalProjectionRef(t, project.Projections[3], "physical_value", s.ValueColumn)

	reduced := sampleForwardTestInput(
		chplan.Column{Name: "physical_labels", Role: chplan.RoleAttributes},
		chplan.Column{Name: s.TimestampColumn, Role: chplan.RoleOpaque},
		chplan.Column{Name: "physical_value", Role: chplan.RoleValue},
	)
	got, err = canonicalizeMixedFloatArmForAgg(reduced, s)
	if err != nil {
		t.Fatal(err)
	}
	project = got.(*chplan.Project)
	if len(project.Projections) != 3 {
		t.Fatalf("reduced projection count = %d, want 3", len(project.Projections))
	}
	assertTemporalProjectionRef(t, project.Projections[0], "physical_labels", s.AttributesColumn)
	if _, ok := project.Projections[1].Expr.(*chplan.Binary); !ok || project.Projections[1].Alias != s.TimestampColumn {
		t.Fatalf("reduced timestamp projection = %#v, want synthesized %q", project.Projections[1], s.TimestampColumn)
	}
	assertTemporalProjectionRef(t, project.Projections[2], "physical_value", s.ValueColumn)

	canonical := sampleForwardTestInput(metricRoles(s)...)
	unchanged, err := canonicalizeMixedFloatArmForAgg(canonical, s)
	if err != nil || unchanged != canonical {
		t.Fatalf("already canonical input changed: plan=%T err=%v", unchanged, err)
	}
}

func TestWidenPlainVectorToMixedShapeResolvesPhysicalRoles(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	s.MetricNameColumn = "public_name"
	s.AttributesColumn = "public_labels"
	s.TimestampColumn = "public_time"
	s.ValueColumn = "public_value"
	input := sampleForwardTestInput(
		chplan.Column{Name: "physical_name", Role: chplan.RoleMetricName},
		chplan.Column{Name: s.MetricNameColumn, Role: chplan.RoleOpaque},
		chplan.Column{Name: "physical_labels", Role: chplan.RoleAttributes},
		chplan.Column{Name: "physical_time", Role: chplan.RoleTimestamp},
		chplan.Column{Name: s.TimestampColumn, Role: chplan.RoleOpaque},
		chplan.Column{Name: "physical_value", Role: chplan.RoleValue},
	)

	got, err := widenPlainVectorToMixedShape(input, s)
	if err != nil {
		t.Fatal(err)
	}
	project := got.(*chplan.Project)
	if len(project.Projections) != 14 {
		t.Fatalf("projection count = %d, want 14", len(project.Projections))
	}
	assertTemporalProjectionRef(t, project.Projections[0], "physical_name", s.MetricNameColumn)
	assertTemporalProjectionRef(t, project.Projections[1], "physical_labels", s.AttributesColumn)
	assertTemporalProjectionRef(t, project.Projections[2], "physical_time", s.TimestampColumn)
	assertTemporalProjectionRef(t, project.Projections[3], "physical_value", s.ValueColumn)
	if kind := project.RowType().SampleKind(); kind != chplan.SampleKindMixed {
		t.Fatalf("widened schema kind = %s, want mixed", kind)
	}
}

func TestWidenPlainVectorToMixedShapeSynthesizesMissingIdentity(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	input := sampleForwardTestInput(
		chplan.Column{Name: "physical_labels", Role: chplan.RoleAttributes},
		chplan.Column{Name: s.MetricNameColumn, Role: chplan.RoleOpaque},
		chplan.Column{Name: s.TimestampColumn, Role: chplan.RoleOpaque},
		chplan.Column{Name: "physical_value", Role: chplan.RoleValue},
	)
	got, err := widenPlainVectorToMixedShape(input, s)
	if err != nil {
		t.Fatal(err)
	}
	project := got.(*chplan.Project)
	name, ok := project.Projections[0].Expr.(*chplan.LitString)
	if !ok || name.V != "" || project.Projections[0].Alias != s.MetricNameColumn {
		t.Fatalf("derived metric-name projection = %#v", project.Projections[0])
	}
	if _, ok := project.Projections[2].Expr.(*chplan.Binary); !ok || project.Projections[2].Alias != s.TimestampColumn {
		t.Fatalf("derived timestamp projection = %#v", project.Projections[2])
	}
}

func TestTemporalConsumersRejectMalformedRoleLayouts(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	valid := metricRoles(s)
	for _, tc := range []struct {
		name  string
		input chplan.Node
	}{
		{name: "open", input: &chplan.Scan{Roles: valid}},
		{name: "duplicate role", input: sampleForwardTestInput(append(append([]chplan.Column(nil), valid...), chplan.Column{Name: "other_value", Role: chplan.RoleValue})...)},
		{name: "unnamed role", input: sampleForwardTestInput(valid[0], valid[1], valid[2], chplan.Column{Role: chplan.RoleValue})},
		{name: "incomplete anchor", input: sampleForwardTestInput(valid[1], chplan.Column{Name: "physical_anchor", Role: chplan.RoleAnchor}, valid[3])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := canonicalizeMixedFloatArmForAgg(tc.input, s); err == nil {
				t.Fatal("aggregate canonicalization accepted malformed roles")
			}
			if _, err := widenPlainVectorToMixedShape(tc.input, s); err == nil {
				t.Fatal("mixed-join widening accepted malformed roles")
			}
		})
	}
}

func assertTemporalProjectionRef(t *testing.T, projection chplan.Projection, source, alias string) {
	t.Helper()
	ref, ok := projection.Expr.(*chplan.ColumnRef)
	if !ok || ref.Name != source || projection.Alias != alias {
		t.Fatalf("projection = %#v, want ColumnRef(%q) AS %q", projection, source, alias)
	}
}

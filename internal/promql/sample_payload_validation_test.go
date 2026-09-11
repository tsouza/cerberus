package promql

import (
	"fmt"
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestSampleForwardHistogramNamesDoNotOverrideSampleRoles(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	for _, field := range chplan.HistogramPayloadColumns() {
		for i, sample := range metricRoles(s) {
			t.Run(fmt.Sprintf("%s/role_%d", field.Name, sample.Role), func(t *testing.T) {
				columns := metricRoles(s)
				columns[i].Name = field.Name
				plan := projectSampleRoles(sampleForwardTestInput(columns...), s,
					sampleProjectionPolicy{name: preserveSampleName, payload: floatSamplePayload},
					sampleProjectionLayout{canonical: true},
					func(refs sampleRoleRefs) sampleRoleRewrite { return sampleRoleRewrite{value: refs.Value} })
				if got := plan.RowType(); !got.Equal(chplan.Schema{Columns: metricRoles(s)}) {
					t.Fatalf("custom sample-role output = %#v", got)
				}
			})
		}
	}
}

func TestSampleForwardRejectsShadowedPublicPayload(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	columns := metricRoles(s)
	for _, field := range chplan.HistogramPayloadColumns() {
		columns = append(columns, chplan.Column{Name: field.Name, Role: chplan.RoleOpaque})
	}
	groupBy := make([]chplan.Expr, len(columns))
	for i, column := range columns {
		groupBy[i] = &chplan.ColumnRef{Name: column.Name}
	}
	// Malformed grouping shadows every appended payload field with an earlier
	// opaque name. Validation must inspect the entire schema, not first matches.
	input := &chplan.HistogramProjection{Input: sampleForwardTestInput(columns...), GroupBy: groupBy}
	row := input.RowType()
	if !row.Has(chplan.RoleHistogramField) || row.HasHistogramPayload() {
		t.Fatalf("test did not construct shadowed histogram roles: %#v", row)
	}
	capturePanic(t, func() {
		projectSampleRoles(input, s,
			sampleProjectionPolicy{name: dropSampleName, payload: floatSamplePayload},
			sampleProjectionLayout{canonical: true},
			func(sampleRoleRefs) sampleRoleRewrite {
				t.Fatal("shadowed public payload reached rewrite callback")
				return sampleRoleRewrite{}
			})
	})
}

func TestSampleForwardRejectsMalformedPublicPayload(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	payload := chplan.HistogramPayloadColumns()
	kind := chplan.Column{Name: "sample_kind", Role: chplan.RoleDiscriminator}
	type payloadCase struct {
		name    string
		columns []chplan.Column
	}
	cases := []payloadCase{
		{name: "orphan_discriminator", columns: []chplan.Column{kind}},
		{name: "duplicate_discriminator", columns: append(slices.Clone(payload), kind, chplan.Column{Name: "other_kind", Role: chplan.RoleDiscriminator})},
		{name: "unnamed_discriminator", columns: append(slices.Clone(payload), chplan.Column{Role: chplan.RoleDiscriminator})},
		{name: "ambiguous_discriminator_name", columns: append(slices.Clone(payload), kind, chplan.Column{Name: kind.Name})},
	}
	for i, field := range payload {
		missing := slices.Delete(slices.Clone(payload), i, i+1)
		wrongRole := slices.Clone(payload)
		wrongRole[i].Role = chplan.RoleOpaque
		cases = append(
			cases,
			payloadCase{name: "missing/" + field.Name, columns: missing},
			payloadCase{name: "missing_mixed/" + field.Name, columns: append(slices.Clone(missing), kind)},
			payloadCase{name: "wrong_role/" + field.Name, columns: append(wrongRole, kind)},
			payloadCase{name: "duplicate_field/" + field.Name, columns: append(slices.Clone(payload), field, kind)},
		)
	}
	for _, tc := range cases {
		for _, narrowed := range []bool{false, true} {
			name := "direct/"
			if narrowed {
				name = "narrowed/"
			}
			t.Run(name+tc.name, func(t *testing.T) {
				var input chplan.Node = sampleForwardTestInput(append(metricRoles(s), tc.columns...)...)
				if narrowed {
					input = &chplan.Filter{Input: input, Predicate: &chplan.Binary{
						Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: kind.Name}, Right: &chplan.LitInt{V: 0},
					}}
				}
				capturePanic(t, func() {
					projectSampleRoles(input, s,
						sampleProjectionPolicy{name: dropSampleName, payload: floatSamplePayload},
						sampleProjectionLayout{canonical: true},
						func(sampleRoleRefs) sampleRoleRewrite {
							t.Fatal("malformed public payload reached rewrite callback")
							return sampleRoleRewrite{}
						})
				})
			})
		}
	}
}

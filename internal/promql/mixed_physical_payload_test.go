package promql

import (
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestMixedRowsNeedPreparationSeparatesPayloadProofAndLegacyShape(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	mixedColumns := append(metricRoles(s), chplan.HistogramPayloadColumns()...)
	mixedColumns = append(mixedColumns, chplan.Column{Name: "source_kind", Role: chplan.RoleDiscriminator})
	mixed := sampleForwardTestInput(mixedColumns...)
	floatRows := &chplan.Filter{Input: mixed, Predicate: &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "source_kind"},
		Right: &chplan.LitInt{V: mixedDiscriminatorFloat},
	}}
	legacyMixedFloat := &chplan.Filter{
		Input:     sampleForwardTestInput(metricRoles(s)...),
		Predicate: &chplan.LitBool{V: true},
		Mixed:     true,
	}
	emptyRankedSelector := &chplan.Filter{Input: floatRows, Predicate: &chplan.LitBool{V: false}}
	nonemptyRankedSelector := &chplan.TopK{Input: floatRows, Columns: topKOutputColumns(floatRows, s)}
	preservingSelector := &chplan.TopK{Input: mixed, Mixed: true}

	for _, tc := range []struct {
		name             string
		input            chplan.Node
		physicalMixed    bool
		floatProven      bool
		legacyShape      chplan.RowShape
		needsPreparation bool
	}{
		{"canonical_float", sampleForwardTestInput(metricRoles(s)...), false, false, chplan.SampleRowShape, false},
		{"pure_histogram", &chplan.HistogramProjection{Input: &chplan.OneRow{}}, false, false, chplan.HistogramRowShape, false},
		{"physical_mixed_legacy_sample", mixed, true, false, chplan.SampleRowShape, true},
		{"float_proof_preserves_physical_mixed", floatRows, true, true, chplan.SampleRowShape, false},
		{"ordered_float_proof", &chplan.OrderBy{Input: floatRows}, true, true, chplan.SampleRowShape, false},
		{"filter_over_float_proof", &chplan.Filter{Input: floatRows, Predicate: &chplan.LitBool{V: true}}, true, true, chplan.SampleRowShape, false},
		{"empty_ranked_selector_preserves_proof", emptyRankedSelector, true, true, chplan.SampleRowShape, false},
		{"nonempty_ranked_selector_projects_float", nonemptyRankedSelector, false, false, chplan.SampleRowShape, false},
		{"preserving_selector_keeps_live_mixed", preservingSelector, true, false, chplan.MixedRowShape, true},
		{"unrelated_filter", &chplan.Filter{Input: mixed, Predicate: &chplan.LitBool{V: true}}, true, false, chplan.SampleRowShape, true},
		{"false_filter", &chplan.Filter{Input: mixed, Predicate: &chplan.LitBool{V: false}}, true, false, chplan.SampleRowShape, true},
		{"project_barrier", &chplan.Project{Input: floatRows}, true, false, chplan.SampleRowShape, true},
		{"legacy_mixed_without_physical_payload", legacyMixedFloat, false, false, chplan.MixedRowShape, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.input.RowType()
			physicalMixed := row.HasHistogramPayload() && row.Has(chplan.RoleDiscriminator)
			if physicalMixed != tc.physicalMixed {
				t.Fatalf("physical mixed=%v, want %v; schema=%#v", physicalMixed, tc.physicalMixed, row)
			}
			if got := mixedFloatRowsProven(tc.input); got != tc.floatProven {
				t.Fatalf("float proof=%v, want %v", got, tc.floatProven)
			}
			if got := chplan.RowShapeOf(tc.input); got != tc.legacyShape {
				t.Fatalf("legacy shape=%s, want %s", got, tc.legacyShape)
			}
			if got := mixedRowsNeedPreparation(tc.input); got != tc.needsPreparation {
				t.Fatalf("needs preparation=%v, want %v", got, tc.needsPreparation)
			}

			prepared := mixedRowsFloatOnly(tc.input)
			if !tc.needsPreparation {
				if prepared != tc.input {
					t.Fatal("input without live mixed rows was narrowed")
				}
				return
			}
			if !prepared.RowType().Equal(row) {
				t.Fatalf("preparation changed physical schema: got %#v, want %#v", prepared.RowType(), row)
			}
			if !chplan.IsMixedFloatNarrowing(prepared) {
				t.Fatalf("prepared plan is not a float discriminator proof: %#v", prepared)
			}
			if mixedRowsFloatOnly(prepared) != prepared {
				t.Fatal("float preparation is not idempotent")
			}
		})
	}
}

func TestMixedRowsNeedPreparationRejectsMalformedPublicPayload(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	payload := chplan.HistogramPayloadColumns()
	kind := chplan.Column{Name: "source_kind", Role: chplan.RoleDiscriminator}

	type payloadCase struct {
		name            string
		columns         []chplan.Column
		preserveUnnamed bool
	}
	cases := []payloadCase{
		{name: "orphan_discriminator", columns: []chplan.Column{kind}},
		{name: "partial_histogram", columns: []chplan.Column{payload[0]}},
		{name: "duplicate_discriminator", columns: append(slices.Clone(payload), kind, chplan.Column{Name: "other_kind", Role: chplan.RoleDiscriminator})},
		{name: "unnamed_discriminator", columns: append(slices.Clone(payload), chplan.Column{Role: chplan.RoleDiscriminator}), preserveUnnamed: true},
		{name: "shadowed_discriminator", columns: append(slices.Clone(payload), kind, chplan.Column{Name: kind.Name, Role: chplan.RoleOpaque})},
	}
	for i, field := range payload {
		missing := slices.Delete(slices.Clone(payload), i, i+1)
		wrongRole := slices.Clone(payload)
		wrongRole[i].Role = chplan.RoleOpaque
		cases = append(
			cases,
			payloadCase{name: "missing/" + field.Name, columns: append(slices.Clone(missing), kind)},
			payloadCase{name: "wrong_role/" + field.Name, columns: append(wrongRole, kind)},
			payloadCase{name: "duplicate/" + field.Name, columns: append(slices.Clone(payload), field, kind)},
		)
	}

	for _, tc := range cases {
		for _, narrowed := range []bool{false, true} {
			name := "direct/" + tc.name
			if narrowed {
				name = "narrowed/" + tc.name
			}
			t.Run(name, func(t *testing.T) {
				columns := append(metricRoles(s), tc.columns...)
				var input chplan.Node = sampleForwardTestInput(columns...)
				if tc.preserveUnnamed {
					input = &chplan.Scan{Table: "samples", Roles: columns}
				}
				if narrowed {
					input = &chplan.Filter{Input: input, Predicate: &chplan.Binary{
						Op:    chplan.OpEq,
						Left:  &chplan.ColumnRef{Name: kind.Name},
						Right: &chplan.LitInt{V: mixedDiscriminatorFloat},
					}}
				}
				capturePanic(t, func() {
					mixedRowsNeedPreparation(input)
				})
			})
		}
	}

	open := &chplan.Scan{Table: "samples", Roles: append(metricRoles(s), kind)}
	capturePanic(t, func() {
		mixedRowsNeedPreparation(open)
	})
}

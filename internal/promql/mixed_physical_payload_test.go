package promql

import (
	"slices"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

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
		name                 string
		input                chplan.Node
		physicalMixed        bool
		floatProven          bool
		legacyShape          chplan.RowShape
		needsPreparation     bool
		mayContainHistograms bool
	}{
		{"canonical_float", sampleForwardTestInput(metricRoles(s)...), false, false, chplan.SampleRowShape, false, false},
		{"pure_histogram", &chplan.HistogramProjection{Input: &chplan.OneRow{}}, false, false, chplan.HistogramRowShape, false, true},
		{"physical_mixed_legacy_sample", mixed, true, false, chplan.SampleRowShape, true, true},
		{"float_proof_preserves_physical_mixed", floatRows, true, true, chplan.SampleRowShape, false, false},
		{"ordered_float_proof", &chplan.OrderBy{Input: floatRows}, true, true, chplan.SampleRowShape, false, false},
		{"filter_over_float_proof", &chplan.Filter{Input: floatRows, Predicate: &chplan.LitBool{V: true}}, true, true, chplan.SampleRowShape, false, false},
		{"empty_ranked_selector_preserves_proof", emptyRankedSelector, true, true, chplan.SampleRowShape, false, false},
		{"nonempty_ranked_selector_projects_float", nonemptyRankedSelector, false, false, chplan.SampleRowShape, false, false},
		{"preserving_selector_keeps_live_mixed", preservingSelector, true, false, chplan.MixedRowShape, true, true},
		{"unrelated_filter", &chplan.Filter{Input: mixed, Predicate: &chplan.LitBool{V: true}}, true, false, chplan.SampleRowShape, true, true},
		{"false_filter", &chplan.Filter{Input: mixed, Predicate: &chplan.LitBool{V: false}}, true, false, chplan.SampleRowShape, true, true},
		{"project_barrier", &chplan.Project{Input: floatRows}, true, false, chplan.SampleRowShape, true, true},
		{"legacy_mixed_without_physical_payload", legacyMixedFloat, false, false, chplan.MixedRowShape, false, false},
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
			if got := rowsMayContainHistograms(tc.input); got != tc.mayContainHistograms {
				t.Fatalf("may contain histograms=%v, want %v", got, tc.mayContainHistograms)
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
				if !rowsMayContainHistograms(input) {
					t.Fatal("malformed payload was routed as float-only")
				}
				capturePanic(t, func() {
					mixedRowsNeedPreparation(input)
				})
			})
		}
	}

	open := &chplan.Scan{Table: "samples", Roles: append(metricRoles(s), kind)}
	if !rowsMayContainHistograms(open) {
		t.Fatal("open mixed schema was routed as float-only")
	}
	capturePanic(t, func() {
		mixedRowsNeedPreparation(open)
	})

	for _, input := range []chplan.Node{
		&chplan.OneRow{},
		&chplan.Scan{Table: "samples", Roles: metricRoles(s)},
		sampleForwardTestInput(chplan.Column{Name: "opaque_value", Role: chplan.RoleOpaque}),
		sampleForwardTestInput(chplan.Column{Role: chplan.RoleValue}),
		sampleForwardTestInput(
			chplan.Column{Name: "value", Role: chplan.RoleValue},
			chplan.Column{Name: "other_value", Role: chplan.RoleValue},
		),
		sampleForwardTestInput(
			chplan.Column{Name: "value", Role: chplan.RoleValue},
			chplan.Column{Name: "value", Role: chplan.RoleOpaque},
		),
		&chplan.Filter{
			Input: sampleForwardTestInput(append(slices.Clone(payload), kind)...),
			Predicate: &chplan.Binary{
				Op:    chplan.OpEq,
				Left:  &chplan.ColumnRef{Name: kind.Name},
				Right: &chplan.LitInt{V: mixedDiscriminatorFloat},
			},
		},
	} {
		if !rowsMayContainHistograms(input) {
			t.Fatalf("unknown schema was routed as float-only: %#v", input.RowType())
		}
	}
}

func TestRowsMayContainHistogramsUsesRolesAcrossSampleEnvelopes(t *testing.T) {
	value := chplan.Column{Name: "source_value", Role: chplan.RoleValue}
	floatRows := sampleForwardTestInput(
		chplan.Column{Name: "source_name", Role: chplan.RoleMetricName},
		chplan.Column{Name: "source_labels", Role: chplan.RoleAttributes},
		chplan.Column{Name: "source_time", Role: chplan.RoleTimestamp},
		value,
	)
	gridRows := sampleForwardTestInput(
		chplan.Column{Name: "source_labels", Role: chplan.RoleAttributes},
		chplan.Column{Name: "source_anchor", Role: chplan.RoleAnchor},
		chplan.Column{Name: "source_time", Role: chplan.RoleTimestamp},
		value,
	)
	reducedRows := sampleForwardTestInput(
		chplan.Column{Name: "group_key", Role: chplan.RoleOpaque},
		value,
	)
	mixedColumns := append(slices.Clone(floatRows.RowType().Columns), chplan.HistogramPayloadColumns()...)
	mixedColumns = append(mixedColumns, chplan.Column{Name: "source_kind", Role: chplan.RoleDiscriminator})
	mixedRows := sampleForwardTestInput(mixedColumns...)
	narrowedRows := &chplan.Filter{Input: mixedRows, Predicate: &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: "source_kind"},
		Right: &chplan.LitInt{V: mixedDiscriminatorFloat},
	}}

	for _, input := range []chplan.Node{floatRows, gridRows, reducedRows, narrowedRows} {
		if rowsMayContainHistograms(input) {
			t.Fatalf("valid float envelope was routed as histogram-capable: %#v", input.RowType())
		}
	}
	for _, input := range []chplan.Node{
		mixedRows,
		&chplan.OrderBy{Input: mixedRows},
		&chplan.Project{Input: narrowedRows},
	} {
		if !rowsMayContainHistograms(input) {
			t.Fatalf("live or unproven mixed envelope was routed as float-only: %#v", input.RowType())
		}
	}

	for _, role := range []chplan.ColumnRole{
		chplan.RoleMetricName,
		chplan.RoleAttributes,
		chplan.RoleTimestamp,
		chplan.RoleAnchor,
	} {
		for _, input := range []chplan.Node{
			sampleForwardTestInput(value, chplan.Column{Role: role}),
			sampleForwardTestInput(
				value,
				chplan.Column{Name: "first", Role: role},
				chplan.Column{Name: "second", Role: role},
			),
			sampleForwardTestInput(
				value,
				chplan.Column{Name: "role_name", Role: role},
				chplan.Column{Name: "role_name", Role: chplan.RoleOpaque},
			),
		} {
			if !rowsMayContainHistograms(input) {
				t.Fatalf("malformed role %d was routed as float-only: %#v", role, input.RowType())
			}
		}
	}
}

func TestHistogramNativeSubqueryInnerRoutesRankedMixedRowsByLiveKind(t *testing.T) {
	standard := schema.DefaultOTelMetrics()
	renamed := standard
	renamed.MetricNameColumn = "source_name"
	renamed.AttributesColumn = "source_labels"
	renamed.TimestampColumn = "source_time"
	renamed.ValueColumn = "source_value"
	at := time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)

	for _, metrics := range []schema.Metrics{standard, renamed} {
		for _, tc := range []struct {
			name  string
			k     string
			empty bool
		}{
			{name: "empty", k: "0", empty: true},
			{name: "nonempty", k: "1"},
		} {
			t.Run(metrics.ValueColumn+"/"+tc.name, func(t *testing.T) {
				expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(
					"topk(" + tc.k + ", latency_exp_hist or num_cpus)[5m:1m]",
				)
				if err != nil {
					t.Fatal(err)
				}
				sub, ok := expr.(*parser.SubqueryExpr)
				if !ok {
					t.Fatalf("parsed expression = %T, want *parser.SubqueryExpr", expr)
				}
				plan, err := lowerSubquery(sub, metrics, lowerCtx{
					start:          at,
					end:            at,
					lowerers:       RangeLowerers{}.withDefaults(),
					resourceBounds: DefaultResourceBounds(),
				})
				if err != nil {
					t.Fatal(err)
				}
				project, ok := plan.(*chplan.Project)
				if !ok {
					t.Fatalf("ranked float subquery = %T, want anchor-shaping *chplan.Project", plan)
				}
				if tc.empty {
					if _, ok := project.Input.(*chplan.Filter); !ok {
						t.Fatalf("empty ranked input = %T, want *chplan.Filter", project.Input)
					}
				} else {
					if _, ok := project.Input.(*chplan.TopK); !ok {
						t.Fatalf("nonempty ranked input = %T, want *chplan.TopK", project.Input)
					}
				}
				if rowsMayContainHistograms(project.Input) {
					t.Fatal("ranked mixed selector retained live histograms")
				}
				row := project.RowType()
				if row.HasHistogramPayload() || row.Has(chplan.RoleDiscriminator) || !row.Has(chplan.RoleAnchor) {
					t.Fatalf("ranked subquery anchor schema = %#v", row)
				}
			})
		}
	}
}

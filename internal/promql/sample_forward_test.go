package promql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func sampleForwardTestInput(columns ...chplan.Column) *chplan.Scan {
	names := make([]string, len(columns))
	for i, column := range columns {
		names[i] = column.Name
	}
	return &chplan.Scan{Table: "samples", Columns: names, Roles: columns}
}

func TestSampleForwardRolesResolveBeforeRewrite(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	input := sampleForwardTestInput(
		chplan.Column{Name: "source_name", Role: chplan.RoleMetricName},
		chplan.Column{Name: "source_attrs", Role: chplan.RoleAttributes},
		chplan.Column{Name: "source_time", Role: chplan.RoleTimestamp},
		chplan.Column{Name: "source_anchor", Role: chplan.RoleAnchor},
		chplan.Column{Name: "source_value", Role: chplan.RoleValue},
	)
	for _, tc := range []struct {
		name   string
		layout sampleProjectionLayout
		want   []chplan.Column
	}{
		{name: "canonical", layout: sampleProjectionLayout{canonical: true}, want: metricRoles(s)},
		{name: "canonical_materialized", layout: sampleProjectionLayout{canonical: true, materializeAliases: true}, want: metricRoles(s)},
		{name: "direct_grid", layout: sampleProjectionLayout{anchored: true}, want: []chplan.Column{
			{Name: s.AttributesColumn, Role: chplan.RoleAttributes},
			{Name: chplan.RangeWindowAnchorColumn, Role: chplan.RoleAnchor},
			{Name: s.TimestampColumn, Role: chplan.RoleTimestamp},
			{Name: s.ValueColumn, Role: chplan.RoleValue},
		}},
		{name: "reduced", want: []chplan.Column{
			{Name: s.AttributesColumn, Role: chplan.RoleAttributes},
			{Name: s.ValueColumn, Role: chplan.RoleValue},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			plan := projectSampleRoles(input, s,
				sampleProjectionPolicy{name: dropSampleName, payload: floatSamplePayload}, tc.layout,
				func(refs sampleRoleRefs) sampleRoleRewrite {
					called = true
					if refs.Value.Name != "source_value" || refs.Attributes.Name != "source_attrs" {
						t.Fatalf("callback captured configured rather than actual names: %#v", refs)
					}
					return sampleRoleRewrite{value: &chplan.FuncCall{Fn: chplan.FnAbs, Args: []chplan.Expr{refs.Value}}}
				})
			if !called {
				t.Fatal("rewrite callback was not invoked")
			}
			if got := plan.RowType(); !got.Equal(chplan.Schema{Columns: tc.want}) {
				t.Fatalf("output = %#v, want %#v", got, tc.want)
			}
			if tc.layout.canonical {
				name, ok := plan.Projections[0].Expr.(*chplan.LitString)
				if !ok || name.V != "" {
					t.Fatalf("name-dropping wrapper forwarded a source name: %#v", plan.Projections[0])
				}
			}
		})
	}
}

func TestSampleForwardPreservesMixedPayload(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	columns := append(metricRoles(s), chplan.HistogramPayloadColumns()...)
	columns = append(columns, chplan.Column{Name: "source_kind", Role: chplan.RoleDiscriminator})
	input := sampleForwardTestInput(columns...)
	plan := projectSampleRoles(input, s,
		sampleProjectionPolicy{name: preserveSampleName, payload: preserveMixedSamplePayload},
		sampleProjectionLayout{canonical: true},
		func(refs sampleRoleRefs) sampleRoleRewrite { return sampleRoleRewrite{attributes: refs.Attributes} })
	want := append(metricRoles(s), chplan.HistogramPayloadColumns()...)
	want = append(want, chplan.Column{Name: mixedDiscriminatorColumn, Role: chplan.RoleDiscriminator})
	if got := plan.RowType(); !got.Equal(chplan.Schema{Columns: want}) {
		t.Fatalf("mixed output = %#v, want %#v", got, want)
	}
	last := plan.Projections[len(plan.Projections)-1]
	ref, ok := last.Expr.(*chplan.ColumnRef)
	if !ok || ref.Name != "source_kind" || last.Alias != mixedDiscriminatorColumn {
		t.Fatalf("discriminator did not use its actual name: %#v", last)
	}
	capturePanic(t, func() {
		projectSampleRoles(input, s,
			sampleProjectionPolicy{name: preserveSampleName, payload: preserveMixedSamplePayload},
			sampleProjectionLayout{canonical: true},
			func(refs sampleRoleRefs) sampleRoleRewrite {
				return sampleRoleRewrite{value: &chplan.FuncCall{Fn: chplan.FnAbs, Args: []chplan.Expr{refs.Value}}}
			})
	})
}

func TestSampleForwardAttributeCallbackUsesActualNames(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	input := sampleForwardTestInput(
		chplan.Column{Name: "input_name", Role: chplan.RoleMetricName},
		chplan.Column{Name: "input_labels", Role: chplan.RoleAttributes},
		chplan.Column{Name: "input_timestamp", Role: chplan.RoleTimestamp},
		chplan.Column{Name: "input_value", Role: chplan.RoleValue},
	)
	plan := projectAttributesOverInner(input, s, func(refs sampleRoleRefs) chplan.Expr {
		return &chplan.LabelJoin{Map: refs.Attributes, Dst: "combined", Separator: "-", Srcs: []string{"job", "instance"}}
	})
	if got := plan.RowType(); !got.Equal(chplan.Schema{Columns: metricRoles(s)}) {
		t.Fatalf("attribute wrapper output = %#v", got)
	}
	for _, projection := range plan.Projections {
		if projection.Alias != s.AttributesColumn {
			continue
		}
		join, ok := projection.Expr.(*chplan.LabelJoin)
		if !ok {
			t.Fatalf("attribute expression = %T", projection.Expr)
		}
		ref, ok := join.Map.(*chplan.ColumnRef)
		if !ok || ref.Name != "input_labels" {
			t.Fatalf("label rewrite source = %#v", join.Map)
		}
		return
	}
	t.Fatal("attribute rewrite projection is absent")
}

func TestSampleForwardExplicitAliasMaterialization(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	columns := append(metricRoles(s), chplan.Column{Name: chplan.RangeWindowAnchorColumn, Role: chplan.RoleAnchor})
	for _, tc := range []struct {
		name    string
		layout  sampleProjectionLayout
		aliases []string
	}{
		{name: "canonical", layout: sampleProjectionLayout{canonical: true}, aliases: []string{s.MetricNameColumn, "", "", s.ValueColumn}},
		{name: "canonical_materialized", layout: sampleProjectionLayout{canonical: true, materializeAliases: true}, aliases: []string{s.MetricNameColumn, s.AttributesColumn, s.TimestampColumn, s.ValueColumn}},
		{name: "direct_grid", layout: sampleProjectionLayout{anchored: true}, aliases: []string{"", "", s.TimestampColumn, s.ValueColumn}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := projectSampleRoles(sampleForwardTestInput(columns...), s,
				sampleProjectionPolicy{name: dropSampleName, payload: floatSamplePayload}, tc.layout,
				func(refs sampleRoleRefs) sampleRoleRewrite { return sampleRoleRewrite{value: refs.Value} })
			if len(plan.Projections) != len(tc.aliases) {
				t.Fatalf("projection count = %d, want %d", len(plan.Projections), len(tc.aliases))
			}
			for i, want := range tc.aliases {
				if got := plan.Projections[i].Alias; got != want {
					t.Errorf("projection %d alias = %q, want %q", i, got, want)
				}
			}
		})
	}
}

func TestSampleForwardFloatNarrowingIsConservative(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	columns := append(metricRoles(s), chplan.HistogramPayloadColumns()...)
	columns = append(columns, chplan.Column{Name: "kind", Role: chplan.RoleDiscriminator})
	input := sampleForwardTestInput(columns...)
	equal := &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "kind"}, Right: &chplan.LitInt{V: 0}}
	narrowed := &chplan.Filter{Input: input, Predicate: equal}
	for _, tc := range []struct {
		name string
		node chplan.Node
		okay bool
	}{
		{name: "unfiltered", node: input},
		{name: "equal", node: narrowed, okay: true},
		{name: "extra_filter", node: &chplan.Filter{Input: narrowed, Predicate: &chplan.LitBool{V: true}}, okay: true},
		{name: "and", node: &chplan.Filter{Input: input, Predicate: &chplan.Binary{Op: chplan.OpAnd, Left: equal, Right: &chplan.LitBool{V: true}}}, okay: true},
		{name: "or", node: &chplan.Filter{Input: input, Predicate: &chplan.Binary{Op: chplan.OpOr, Left: equal, Right: &chplan.LitBool{V: true}}}},
		{name: "project_is_not_proof", node: &chplan.Project{Input: narrowed}},
		{name: "qualified", node: &chplan.Filter{Input: input, Predicate: &chplan.Binary{
			Op:   chplan.OpEq,
			Left: &chplan.ColumnRef{Name: "kind", Qualifier: "other"}, Right: &chplan.LitInt{V: 0},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := func() {
				plan := projectSampleRoles(tc.node, s,
					sampleProjectionPolicy{name: dropSampleName, payload: floatSamplePayload},
					sampleProjectionLayout{canonical: true},
					func(refs sampleRoleRefs) sampleRoleRewrite {
						if !tc.okay {
							t.Fatal("unsafe payload reached rewrite callback")
						}
						return sampleRoleRewrite{value: refs.Value}
					})
				if got := plan.RowType(); !got.Equal(chplan.Schema{Columns: metricRoles(s)}) {
					t.Fatalf("float output = %#v", got)
				}
			}
			if tc.okay {
				call()
			} else {
				capturePanic(t, call)
			}
		})
	}
}

func TestSampleForwardRejectsMissingAndAmbiguousRoles(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	for _, tc := range []struct {
		name    string
		columns []chplan.Column
	}{
		{name: "missing", columns: []chplan.Column{{Name: s.AttributesColumn, Role: chplan.RoleAttributes}}},
		{name: "duplicate_role", columns: append(metricRoles(s), chplan.Column{Name: "another_value", Role: chplan.RoleValue})},
		{name: "duplicate_name", columns: append(metricRoles(s), chplan.Column{Name: s.ValueColumn, Role: chplan.RoleOpaque})},
		{name: "unnamed", columns: []chplan.Column{{Name: s.AttributesColumn, Role: chplan.RoleAttributes}, {Role: chplan.RoleValue}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capturePanic(t, func() {
				projectSampleRoles(sampleForwardTestInput(tc.columns...), s,
					sampleProjectionPolicy{name: dropSampleName, payload: floatSamplePayload},
					sampleProjectionLayout{canonical: true}, func(sampleRoleRefs) sampleRoleRewrite {
						t.Fatal("ambiguous role reached rewrite callback")
						return sampleRoleRewrite{}
					})
			})
		})
	}
}

func TestSampleForwardPreserveNameCompatibility(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	for _, opaque := range []bool{false, true} {
		columns := []chplan.Column{
			{Name: s.AttributesColumn, Role: chplan.RoleAttributes},
			{Name: s.TimestampColumn, Role: chplan.RoleTimestamp},
			{Name: s.ValueColumn, Role: chplan.RoleValue},
		}
		if opaque {
			columns = append(columns, chplan.Column{Name: s.MetricNameColumn, Role: chplan.RoleOpaque})
		}
		plan := projectSampleRoles(sampleForwardTestInput(columns...), s,
			sampleProjectionPolicy{name: preserveSampleName, payload: preserveMixedSamplePayload},
			sampleProjectionLayout{canonical: true},
			func(refs sampleRoleRefs) sampleRoleRewrite { return sampleRoleRewrite{attributes: refs.Attributes} })
		first := plan.Projections[0]
		if opaque {
			ref, ok := first.Expr.(*chplan.ColumnRef)
			if !ok || ref.Name != s.MetricNameColumn || first.Alias != "" {
				t.Fatalf("opaque existing name was reinterpreted: %#v", first)
			}
		} else {
			literal, ok := first.Expr.(*chplan.LitString)
			if !ok || literal.V != "" || first.Alias != s.MetricNameColumn {
				t.Fatalf("missing name was not synthesized canonically: %#v", first)
			}
		}
	}
}

func TestSampleForwardRejectsUnknownPolicy(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	const unknownPolicy = 255
	for _, policy := range []sampleProjectionPolicy{
		{name: unknownPolicy, payload: floatSamplePayload},
		{name: dropSampleName, payload: unknownPolicy},
	} {
		capturePanic(t, func() {
			projectSampleRoles(sampleForwardTestInput(metricRoles(s)...), s, policy,
				sampleProjectionLayout{canonical: true}, func(sampleRoleRefs) sampleRoleRewrite {
					t.Fatal("unknown policy reached the callback")
					return sampleRoleRewrite{}
				})
		})
	}
}

func TestSampleForwardPayloadAdmission(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	for _, tc := range []struct {
		name  string
		extra []chplan.Column
		okay  bool
	}{
		{name: "partial_float_helper", extra: []chplan.Column{{Name: "histogram_working_sum", Role: chplan.RoleHistogramField}}, okay: true},
		{name: "pure_histogram", extra: chplan.HistogramPayloadColumns()},
		{name: "incomplete_mixed", extra: []chplan.Column{{Name: mixedDiscriminatorColumn, Role: chplan.RoleDiscriminator}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := sampleForwardTestInput(append(metricRoles(s), tc.extra...)...)
			call := func() {
				plan := projectAttributesOverInner(input, s, func(refs sampleRoleRefs) chplan.Expr {
					if !tc.okay {
						t.Fatal("unsupported payload reached attribute builder")
					}
					return refs.Attributes
				})
				if got := plan.RowType(); !got.Equal(chplan.Schema{Columns: metricRoles(s)}) {
					t.Fatalf("float helper output = %#v", got)
				}
			}
			if tc.okay {
				call()
			} else {
				capturePanic(t, call)
			}
		})
	}
}

func TestSampleForwardMissingNameAdmissionIsClosedFloatOnly(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	for _, tc := range []struct {
		name  string
		open  bool
		extra []chplan.Column
	}{
		{name: "open", open: true},
		{name: "helper_payload", extra: []chplan.Column{{Name: "helper", Role: chplan.RoleHistogramField}}},
		{name: "conflicting_name_role", extra: []chplan.Column{{Name: s.MetricNameColumn, Role: chplan.RoleAnchor}}},
		{name: "duplicate_opaque_name", extra: []chplan.Column{{Name: s.MetricNameColumn}, {Name: s.MetricNameColumn}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			columns := append([]chplan.Column{
				{Name: s.AttributesColumn, Role: chplan.RoleAttributes},
				{Name: s.TimestampColumn, Role: chplan.RoleTimestamp},
				{Name: s.ValueColumn, Role: chplan.RoleValue},
			}, tc.extra...)
			input := sampleForwardTestInput(columns...)
			if tc.open {
				input.Columns = nil
			}
			capturePanic(t, func() {
				projectAttributesOverInner(input, s, func(sampleRoleRefs) chplan.Expr {
					t.Fatal("unproven missing name reached attribute builder")
					return nil
				})
			})
		})
	}
}

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

func TestLegacySampleProjectionLayoutUsesTemporalRoles(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		columns []chplan.Column
		want    sampleProjectionLayout
	}{
		{
			name: "canonical renamed roles",
			columns: []chplan.Column{
				{Name: "source_name", Role: chplan.RoleMetricName},
				{Name: "source_labels", Role: chplan.RoleAttributes},
				{Name: "source_time", Role: chplan.RoleTimestamp},
				{Name: "source_value", Role: chplan.RoleValue},
			},
			want: sampleProjectionLayout{canonical: true},
		},
		{
			name: "grid renamed roles",
			columns: []chplan.Column{
				{Name: "source_labels", Role: chplan.RoleAttributes},
				{Name: "source_anchor", Role: chplan.RoleAnchor},
				{Name: "source_time", Role: chplan.RoleTimestamp},
				{Name: "source_value", Role: chplan.RoleValue},
			},
			want: sampleProjectionLayout{anchored: true},
		},
		{
			name: "reduced ignores misleading opaque names",
			columns: []chplan.Column{
				{Name: "source_labels", Role: chplan.RoleAttributes},
				{Name: "TimeUnix", Role: chplan.RoleOpaque},
				{Name: "anchor_ts", Role: chplan.RoleOpaque},
				{Name: "source_value", Role: chplan.RoleValue},
			},
			want: sampleProjectionLayout{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := legacySampleProjectionLayout(sampleForwardTestInput(tc.columns...)); got != tc.want {
				t.Fatalf("layout = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestLegacySampleProjectionLayoutDistinguishesCanonicalAndDerivedProjects(t *testing.T) {
	t.Parallel()

	for _, custom := range []bool{false, true} {
		s := schema.DefaultOTelMetrics()
		name := "default"
		if custom {
			name = "custom"
			s.MetricNameColumn = "physical_name"
			s.AttributesColumn = "physical_labels"
			s.TimestampColumn = "physical_time"
			s.ValueColumn = "physical_value"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			grid := &chplan.RangeWindow{
				Input:           sampleForwardTestInput(metricRoles(s)...),
				OuterRange:      1,
				TimestampColumn: s.TimestampColumn,
				ValueColumn:     s.ValueColumn,
				GroupBy: []chplan.Expr{
					&chplan.ColumnRef{Name: s.MetricNameColumn},
					&chplan.ColumnRef{Name: s.AttributesColumn},
				},
			}
			canonicalRoles := append(metricRoles(s), chplan.Column{Name: chplan.RangeWindowAnchorColumn, Role: chplan.RoleAnchor})
			canonical := &chplan.Project{
				Input: grid,
				Projections: []chplan.Projection{
					{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}},
					{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
					{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
					{Expr: &chplan.ColumnRef{Name: s.ValueColumn}},
					{Expr: &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}},
				},
				Roles: canonicalRoles,
			}
			if got := legacySampleProjectionLayout(canonical); got != (sampleProjectionLayout{canonical: true, anchored: true}) {
				t.Fatalf("canonical project layout = %#v, want canonical plus anchor", got)
			}

			derivedRoles := []chplan.Column{
				{Name: s.AttributesColumn, Role: chplan.RoleAttributes},
				{Name: chplan.RangeWindowAnchorColumn, Role: chplan.RoleAnchor},
				{Name: s.TimestampColumn, Role: chplan.RoleTimestamp},
				{Name: s.ValueColumn, Role: chplan.RoleValue},
			}
			projectGrid := func(input chplan.Node) *chplan.Project {
				return &chplan.Project{
					Input: input,
					Projections: []chplan.Projection{
						{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
						{Expr: &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}},
						{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
						{Expr: &chplan.ColumnRef{Name: s.ValueColumn}},
					},
					Roles: derivedRoles,
				}
			}
			transparent := projectGrid(grid)
			nested := projectGrid(transparent)
			for label, project := range map[string]*chplan.Project{"transparent": transparent, "nested": nested} {
				t.Run(label, func(t *testing.T) {
					if got := legacySampleProjectionLayout(project); got != (sampleProjectionLayout{anchored: true}) {
						t.Fatalf("derived project layout = %#v, want anchored", got)
					}
				})
			}
		})
	}
}

func TestLegacySampleProjectionLayoutKeepsDeclaredAnchorWithoutGridSpine(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	columns := append(metricRoles(s), chplan.Column{Name: chplan.RangeWindowAnchorColumn, Role: chplan.RoleAnchor})
	input := sampleForwardTestInput(columns...)
	project := &chplan.Project{Input: input, Roles: columns}
	for _, column := range columns {
		project.Projections = append(project.Projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: column.Name}})
	}
	if got := legacySampleProjectionLayout(project); got != (sampleProjectionLayout{canonical: true, anchored: true}) {
		t.Fatalf("layout = %#v, want canonical plus anchor", got)
	}
}

func TestAnchoredGridLayoutSpineAcceptsEitherCrossJoinInput(t *testing.T) {
	t.Parallel()

	grid := &chplan.RangeWindow{OuterRange: 1}
	plain := sampleForwardTestInput(chplan.Column{Name: "value", Role: chplan.RoleValue})
	for _, join := range []*chplan.CrossJoin{{Left: grid, Right: plain}, {Left: plain, Right: grid}} {
		if !anchoredGridLayoutSpine(join) {
			t.Fatalf("grid spine was not found through %T", join)
		}
	}
}

func TestLegacySampleProjectionLayoutAcceptsReducedWindowSpine(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	window := &chplan.RangeWindow{
		Input:           sampleForwardTestInput(metricRoles(s)...),
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
	project := &chplan.Project{
		Input: window,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}},
		},
		Roles: []chplan.Column{
			{Name: s.AttributesColumn, Role: chplan.RoleAttributes},
			{Name: s.ValueColumn, Role: chplan.RoleValue},
		},
	}
	if got := legacySampleProjectionLayout(project); got != (sampleProjectionLayout{}) {
		t.Fatalf("reduced window projection layout = %#v, want reduced", got)
	}
}

func TestLegacySampleProjectionLayoutRejectsInvalidRoles(t *testing.T) {
	t.Parallel()

	base := []chplan.Column{
		{Name: "source_labels", Role: chplan.RoleAttributes},
		{Name: "source_time", Role: chplan.RoleTimestamp},
		{Name: "source_value", Role: chplan.RoleValue},
	}
	for _, tc := range []struct {
		name    string
		columns []chplan.Column
	}{
		{name: "missing attributes", columns: base[1:]},
		{name: "missing value", columns: base[:2]},
		{name: "duplicate timestamp role", columns: append(append([]chplan.Column(nil), base...), chplan.Column{Name: "other_time", Role: chplan.RoleTimestamp})},
		{name: "ambiguous timestamp name", columns: append(append([]chplan.Column(nil), base...), chplan.Column{Name: "source_time", Role: chplan.RoleOpaque})},
		{name: "anchor without timestamp", columns: []chplan.Column{base[0], {Name: "source_anchor", Role: chplan.RoleAnchor}, base[2]}},
		{name: "metric name without timestamp", columns: []chplan.Column{{Name: "source_name", Role: chplan.RoleMetricName}, base[0], base[2]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			capturePanic(t, func() {
				legacySampleProjectionLayout(sampleForwardTestInput(tc.columns...))
			})
		})
	}
	t.Run("unnamed timestamp", func(t *testing.T) {
		t.Parallel()
		if _, _, err := resolveSampleTemporalLayout(chplan.Schema{Columns: []chplan.Column{base[0], {Role: chplan.RoleTimestamp}, base[2]}}); err == nil {
			t.Fatal("temporal resolver accepted an unnamed role")
		}
	})
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
	plan := mustProjectAttributesOverInner(t, input, s, func(refs sampleRoleRefs) chplan.Expr {
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
	ordered := &chplan.OrderBy{Input: narrowed}
	for _, tc := range []struct {
		name string
		node chplan.Node
		okay bool
	}{
		{name: "unfiltered", node: input},
		{name: "equal", node: narrowed, okay: true},
		{name: "ordered_float_subset", node: ordered, okay: true},
		{name: "filter_over_order", node: &chplan.Filter{Input: ordered, Predicate: &chplan.LitBool{V: true}}, okay: true},
		{name: "order_filter_order", node: &chplan.OrderBy{Input: &chplan.Filter{Input: ordered, Predicate: &chplan.LitBool{V: true}}}, okay: true},
		{name: "ordered_unrestricted_mixed", node: &chplan.OrderBy{Input: input}},
		{name: "ordered_or_is_not_proof", node: &chplan.OrderBy{Input: &chplan.Filter{Input: input, Predicate: &chplan.Binary{Op: chplan.OpOr, Left: equal, Right: &chplan.LitBool{V: true}}}}},
		{name: "ordered_project_barrier", node: &chplan.OrderBy{Input: &chplan.Project{Input: narrowed}}},
		{name: "ordered_qualified_is_not_proof", node: &chplan.OrderBy{Input: &chplan.Filter{Input: input, Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "kind", Qualifier: "other"}, Right: &chplan.LitInt{V: 0}}}}},
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
			if got := mixedFloatRowsProven(tc.node); got != tc.okay {
				t.Fatalf("float subset proof=%v, want %v", got, tc.okay)
			}
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

func TestSampleForwardRolePolicyBoundaries(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	refs := sampleRoleRefs{Value: &chplan.ColumnRef{Name: "actual_value"}}
	if got := refs.sourceMetrics(s); got.AttributesColumn != s.AttributesColumn || got.ValueColumn != "actual_value" {
		t.Fatalf("source metrics = %#v", got)
	}

	row := chplan.Schema{Columns: []chplan.Column{
		{Name: s.MetricNameColumn, Role: chplan.RoleOpaque},
		{Name: s.AttributesColumn, Role: chplan.RoleAttributes},
		{Name: s.TimestampColumn, Role: chplan.RoleTimestamp},
		{Name: s.ValueColumn, Role: chplan.RoleValue},
	}}
	resolveSampleRoleRefs(row, s, sampleProjectionPolicy{name: preserveSampleName}, sampleProjectionLayout{canonical: true})

	conflicting := row
	conflicting.Columns = append([]chplan.Column(nil), row.Columns...)
	conflicting.Columns[0].Role = chplan.RoleAnchor
	resolveSampleRoleRefs(conflicting, s, sampleProjectionPolicy{name: dropSampleName}, sampleProjectionLayout{canonical: true})
	capturePanic(t, func() {
		resolveSampleRoleRefs(conflicting, s, sampleProjectionPolicy{name: preserveSampleName}, sampleProjectionLayout{canonical: true})
	})

	validateConfiguredSampleRole(row, s.MetricNameColumn, chplan.RoleMetricName, true)
	capturePanic(t, func() {
		validateConfiguredSampleRole(row, s.MetricNameColumn, chplan.RoleMetricName, false)
	})
}

func TestSamplePayloadRequiresCompletePublicHistogramWithoutDiscriminator(t *testing.T) {
	t.Parallel()

	columns := chplan.HistogramPayloadColumns()
	complete, discriminated := validateSamplePayload(chplan.Schema{Columns: columns})
	if !complete || discriminated {
		t.Fatalf("payload = complete:%v discriminated:%v", complete, discriminated)
	}
	for _, missing := range columns {
		incomplete := make([]chplan.Column, 0, len(columns)-1)
		for _, column := range columns {
			if column.Name != missing.Name {
				incomplete = append(incomplete, column)
			}
		}
		capturePanic(t, func() { validateSamplePayload(chplan.Schema{Columns: incomplete}) })
	}
	capturePanic(t, func() {
		validateSamplePayload(chplan.Schema{Columns: []chplan.Column{columns[0]}})
	})
}

func TestSampleRoleResolversRejectEachAmbiguousBoundary(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	base := []chplan.Column{
		{Name: s.AttributesColumn, Role: chplan.RoleAttributes},
		{Name: s.TimestampColumn, Role: chplan.RoleTimestamp},
		{Name: s.ValueColumn, Role: chplan.RoleValue},
	}
	for _, extra := range []chplan.Column{
		{Name: "helper", Role: chplan.RoleHistogramField},
		{Name: "kind", Role: chplan.RoleDiscriminator},
	} {
		row := chplan.Schema{Columns: append(append([]chplan.Column(nil), base...), extra)}
		capturePanic(t, func() {
			resolveSampleRoleRefs(row, s, sampleProjectionPolicy{name: preserveSampleName}, sampleProjectionLayout{canonical: true})
		})
	}
	open := chplan.Schema{Columns: append([]chplan.Column(nil), base...), Open: true}
	capturePanic(t, func() {
		resolveSampleRoleRefs(open, s, sampleProjectionPolicy{name: preserveSampleName}, sampleProjectionLayout{canonical: true})
	})

	_, ok, err := resolveOptionalSampleRoleName(chplan.Schema{Columns: []chplan.Column{
		{Name: "first_value", Role: chplan.RoleValue},
		{Name: "second_value", Role: chplan.RoleValue},
	}}, chplan.RoleValue)
	if err == nil || ok {
		t.Fatalf("duplicate value roles resolved: ok=%v err=%v", ok, err)
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
		{name: "private_float_helper", extra: []chplan.Column{{Name: "histogram_working_sum", Role: chplan.RoleOpaque}}, okay: true},
		{name: "pure_histogram", extra: chplan.HistogramPayloadColumns()},
		{name: "incomplete_mixed", extra: []chplan.Column{{Name: mixedDiscriminatorColumn, Role: chplan.RoleDiscriminator}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := sampleForwardTestInput(append(metricRoles(s), tc.extra...)...)
			call := func() {
				plan := mustProjectAttributesOverInner(t, input, s, func(refs sampleRoleRefs) chplan.Expr {
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
				mustProjectAttributesOverInner(t, input, s, func(sampleRoleRefs) chplan.Expr {
					t.Fatal("unproven missing name reached attribute builder")
					return nil
				})
			})
		})
	}
}

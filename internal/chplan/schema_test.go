package chplan

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestRowTypeEveryNode(t *testing.T) {
	roles := []Column{{"name", RoleMetricName}, {"labels", RoleAttributes}, {"time", RoleTimestamp}, {"value", RoleValue}}
	scan := &Scan{Table: "samples", Columns: []string{"name", "labels", "time", "value"}, Roles: roles}
	key := &ColumnRef{Name: "labels"}
	groups := []Expr{key}
	grid := &RangeWindowGridNative{Input: scan, GroupBy: groups, TimestampColumn: "time", ValueColumn: "value"}
	traceRoles := []Column{{"trace", RoleTraceID}, {"span", RoleSpanID}, {"parent", RoleParentSpanID}}
	traces := &Scan{Table: "spans", Columns: []string{"trace", "span", "parent"}, Roles: traceRoles}
	canonical := Schema{Columns: roles}
	groupValue := Schema{Columns: []Column{{"labels", RoleAttributes}, {"value", RoleValue}}}
	hist := []Column{{"HistogramCount", RoleHistogramField}, {"HistogramSum", RoleHistogramField}, {"HistogramScale", RoleHistogramField}, {"HistogramZeroThreshold", RoleHistogramField}, {"HistogramZeroCount", RoleHistogramField}, {"HistogramPositiveOffset", RoleHistogramField}, {"HistogramPositiveBucketCounts", RoleHistogramField}, {"HistogramNegativeOffset", RoleHistogramField}, {"HistogramNegativeBucketCounts", RoleHistogramField}}
	cases := []struct {
		node Node
		want Schema
	}{
		{scan, canonical},
		{&OneRow{}, Schema{Columns: []Column{{Name: "1"}}}},
		{&StepGrid{}, Schema{Columns: []Column{{"anchor_ts", RoleAnchor}}}},
		{&Filter{Input: scan}, canonical},
		{&Limit{Input: scan}, canonical},
		{&OrderBy{Input: scan}, canonical},
		{&SearchTraceLimit{Input: traces}, Schema{Columns: traceRoles}},
		{&MetricsSecondStage{Input: scan}, canonical},
		{&TopK{Input: scan, Columns: []string{"labels", "value"}}, groupValue},
		{&Project{Input: scan, Projections: []Projection{{Expr: key}, {Expr: &LitInt{V: 1}, Alias: "value"}}}, groupValue},
		{&Aggregate{Input: scan, GroupBy: groups, AggFuncs: []AggFunc{{Fn: FnSum, Alias: "value"}}}, groupValue},
		{&CrossJoin{Left: scan, Right: &StepGrid{}}, Schema{Columns: append(slices.Clone(roles), Column{"anchor_ts", RoleAnchor})}},
		{&UnionAll{Inputs: []Node{scan, scan}}, canonical},
		{&SetOperation{Left: traces, Right: traces}, Schema{Columns: traceRoles}},
		{&NestedSetAnnotate{Input: traces}, Schema{Columns: append(slices.Clone(traceRoles), Column{Name: NestedSetLeftColumn}, Column{Name: NestedSetRightColumn}, Column{Name: NestedSetParentColumn})}},
		{&RangeLWR{Input: scan, MetricNameCol: "name", AttributesCol: "labels", TimestampCol: "time", ValueCol: "value"}, canonical},
		{&RangeWindowStaleResample{Input: scan, MetricNameCol: "name", AttributesCol: "labels", TimestampCol: "time", ValueCol: "value"}, canonical},
		{&VectorJoin{Left: scan, Right: scan, MetricNameColumn: "name", AttributesColumn: "labels", TimestampColumn: "time", ValueColumn: "value"}, canonical},
		{&VectorSetOp{Left: scan, Right: scan, MetricNameColumn: "name", AttributesColumn: "labels", TimestampColumn: "time", ValueColumn: "value"}, canonical},
		{&NaryVectorSetOp{Arms: []Node{scan, scan}, MetricNameColumn: "name", AttributesColumn: "labels", TimestampColumn: "time", ValueColumn: "value"}, canonical},
		{&InfoJoin{Input: scan, MetricNameColumn: "name", AttributesColumn: "labels", TimestampColumn: "time", ValueColumn: "value"}, canonical},
		{&HistogramProjection{Input: scan, GroupBy: groups}, Schema{Columns: append([]Column{{"labels", RoleAttributes}}, hist...)}},
		{&HistogramQuantile{Input: scan, GroupBy: groups}, Schema{Columns: []Column{{"labels", RoleAttributes}, {"Value", RoleValue}}}},
		{&HistogramQuantileNative{Input: scan, GroupBy: groups}, Schema{Columns: []Column{{"labels", RoleAttributes}, {"Value", RoleValue}}}},
		{&AbsentOverTime{Input: scan, MetricNameColumn: "name", AttributesColumn: "labels", TimestampColumn: "time", ValueColumn: "value"}, canonical},
		{grid, Schema{Columns: []Column{{"labels", RoleAttributes}, {"anchor_ts", RoleAnchor}, {"time", RoleTimestamp}, {"value", RoleValue}}}},
		{&RangeWindowGridNativeInstant{Input: scan, GroupBy: groups, ValueColumn: "value"}, groupValue},
		{&RangeWindow{Input: scan, GroupBy: groups, ValueColumn: "value"}, groupValue},
		{&RangeBucketFanout{Input: scan, GroupBy: groups, AnchorAlias: "anchor", AggFuncs: []AggFunc{{Alias: "value"}}}, Schema{Columns: []Column{{"anchor", RoleAnchor}, {"labels", RoleAttributes}, {"value", RoleValue}}}},
		{&RangeBucketGridNative{Input: scan, GroupBy: groups, AnchorAlias: "anchor", BucketCountsCol: "counts", ExplicitBoundsCol: "bounds"}, Schema{Columns: []Column{{"anchor", RoleAnchor}, {"labels", RoleAttributes}, {Name: "counts"}, {Name: "bounds"}}}},
		{&RangeWindowGridNativeVectorAgg{Input: grid, GroupBy: groups, GroupByAliases: []string{"labels"}, AnchorAlias: "time"}, Schema{Columns: []Column{{"labels", RoleAttributes}, {"anchor_ts", RoleAnchor}, {"time", RoleTimestamp}, {"value", RoleValue}}}},
		{&MetricsAggregate{Inner: scan, GroupBy: groups, ValueAlias: "value"}, groupValue},
		{&MetricsHistogramOverTime{Inner: scan, GroupBy: groups, ValueAlias: "value"}, Schema{Columns: []Column{{"labels", RoleAttributes}, {Name: "__bucket"}, {"value", RoleValue}}}},
		{&MetricsCompare{Inner: traces}, Schema{Columns: []Column{{Name: "is_selection"}, {Name: "attr"}, {Name: "val"}, {"Value", RoleValue}}}},
		{&StructuralJoin{Left: traces, Right: traces, TraceIDColumn: "trace", SpanIDColumn: "span", ParentSpanIDColumn: "parent"}, Schema{Columns: traceRoles}},
	}
	// Payload joins use deliberately opaque names: their public outputs are
	// qualified intermediate fields, not directly forwardable sample columns.
	hj := &HistogramVectorJoin{Left: scan, Right: scan, MetricNameColumn: "name", AttributesColumn: "labels", TimestampColumn: "time"}
	hjNames := []string{"name", "labels", "time", "HistogramScale", "HistogramCount", "HistogramSum", "HistogramZeroCount", "HistogramZeroThreshold", "HistogramPositiveOffset", "HistogramPositiveBucketCounts", "HistogramNegativeOffset", "HistogramNegativeBucketCounts"}
	wantJoin := func(prefix string, names []string) Schema {
		out := Schema{}
		for _, name := range names {
			for _, side := range []string{"L", "R"} {
				out.Columns = append(out.Columns, Column{Name: prefix + "_" + side + "_" + name})
			}
		}
		return out
	}
	cases = append(cases, struct {
		node Node
		want Schema
	}{hj, wantJoin("_hq", hjNames)})
	mixedNames := []string{"name", "labels", "time", "value"}
	for _, c := range hist {
		mixedNames = append(mixedNames, c.Name)
	}
	mixedNames = append(mixedNames, MixedDiscriminatorColumn)
	cases = append(cases, struct {
		node Node
		want Schema
	}{&MixedVectorJoin{Left: scan, Right: scan, MetricNameColumn: "name", AttributesColumn: "labels", TimestampColumn: "time", ValueColumn: "value"}, wantJoin("_mvj", mixedNames)})
	hf := Schema{Columns: append([]Column(nil), roles[:len(roles)-1]...)}
	for _, name := range hjNames[len(roles)-1:] {
		hf.Columns = append(hf.Columns, Column{name, RoleHistogramField})
	}
	hf.Columns = append(hf.Columns, Column{"value", RoleValue})
	cases = append(cases, struct {
		node Node
		want Schema
	}{&HistogramFloatVectorJoin{MetricNameColumn: "name", AttributesColumn: "labels", TimestampColumn: "time", ValueColumn: "value"}, hf})
	covered := map[string]bool{}
	for _, tc := range cases {
		name := reflect.TypeOf(tc.node).Elem().Name()
		covered[name] = true
		t.Run(name, func(t *testing.T) {
			if got := tc.node.RowType(); !got.Equal(tc.want) {
				t.Fatalf("RowType = %#v; want %#v", got, tc.want)
			}
		})
	}
	assertCoversEverySealedKind(t, nodeMarkerMethod, covered, "RowType cases", "add an output-schema assertion")
}

func TestRowTypeDeclarations(t *testing.T) {
	roles := []Column{{"renamed_value", RoleValue}, {"renamed_labels", RoleAttributes}}
	p := &Project{Input: &OneRow{}, Roles: roles, Projections: []Projection{{Expr: &LitInt{V: 1}, Alias: "renamed_value"}}}
	if got := p.RowType(); !got.Equal(Schema{Columns: roles[:1]}) {
		t.Fatalf("synthesized output: %#v", got)
	}
	clone := CloneNode(p).(*Project)
	clone.Roles[0].Role = RoleOpaque
	if p.Equal(clone) || p.Roles[0].Role != RoleValue {
		t.Fatal("declaration not isolated in clone/equality")
	}
	if got := (&Project{Input: p, Projections: []Projection{{Expr: &ColumnRef{Name: "renamed_value"}, Alias: "private"}}}).RowType(); got.Has(RoleValue) {
		t.Fatal("private alias inherited a public value role")
	}
	open := (&Scan{Table: "samples", Roles: roles}).RowType()
	if !open.Open || !open.Equal(Schema{Columns: roles, Open: true}) {
		t.Fatalf("open scan: %#v", open)
	}
}

func TestProjectRowTypeAlignedDeclarations(t *testing.T) {
	roles := []Column{{Name: "value", Role: RoleValue}, {Name: "opaque", Role: RoleOpaque}}
	aligned := &Project{
		Input: &OneRow{},
		Roles: roles,
		Projections: []Projection{
			{Expr: &LitInt{V: 1}, Alias: "value"},
			{Expr: &LitInt{V: 2}, Alias: "opaque"},
		},
	}
	got := aligned.RowType()
	if !got.Equal(Schema{Columns: roles}) {
		t.Fatalf("aligned declarations: %#v", got)
	}
	got.Columns[0].Role = RoleOpaque
	if aligned.Roles[0].Role != RoleValue {
		t.Fatal("RowType schema aliases the Project role declarations")
	}

	for _, tc := range []struct {
		name        string
		roles       []Column
		projections []Projection
		want        Schema
	}{
		{
			name:        "partial_roles",
			roles:       roles[:1],
			projections: aligned.Projections,
			want:        Schema{Columns: []Column{roles[0], {Name: "opaque"}}},
		},
		{
			name:        "mismatched_order",
			roles:       []Column{roles[1], roles[0]},
			projections: aligned.Projections,
			want:        Schema{Columns: roles},
		},
		{
			name:  "duplicate_names",
			roles: []Column{{Name: "duplicate", Role: RoleValue}, {Name: "duplicate", Role: RoleAttributes}},
			projections: []Projection{
				{Expr: &LitInt{V: 1}, Alias: "duplicate"},
				{Expr: &LitInt{V: 2}, Alias: "duplicate"},
			},
			want: Schema{Columns: []Column{{Name: "duplicate", Role: RoleValue}, {Name: "duplicate", Role: RoleValue}}},
		},
		{
			name:        "unnamed_role",
			roles:       []Column{{Role: RoleDiscriminator}},
			projections: []Projection{{Expr: &LitInt{V: 1}}},
			want:        Schema{Columns: []Column{{}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := &Project{Input: &OneRow{}, Roles: tc.roles, Projections: tc.projections}
			if got := project.RowType(); !got.Equal(tc.want) {
				t.Fatalf("RowType = %#v; want %#v", got, tc.want)
			}
		})
	}
}

func TestRowTypeWindowBranches(t *testing.T) {
	input := &Scan{Columns: []string{"labels", "time", "extra"}, Roles: []Column{{"labels", RoleAttributes}, {"time", RoleTimestamp}}}
	r := &RangeWindow{Input: input, GroupBy: []Expr{&ColumnRef{Name: "labels"}}, ValueColumn: "value", TimestampColumn: "anchor_ts", OuterRange: time.Minute, Identity: true}
	want := Schema{Columns: []Column{{"labels", RoleAttributes}, {"anchor_ts", RoleAnchor}, {"TimeUnix", RoleTimestamp}, {"value", RoleValue}}}
	if got := r.RowType(); !got.Equal(want) {
		t.Fatalf("identity: %#v", got)
	}
	r.Variants = []RangeWindowVariant{{ValueColumn: "value"}}
	r.VariantColumn = "variant"
	want.Columns = []Column{{"labels", RoleAttributes}, {"anchor_ts", RoleAnchor}, {"value", RoleValue}, {Name: "variant"}}
	if got := r.RowType(); !got.Equal(want) {
		t.Fatalf("variants: %#v", got)
	}
	r.Variants = nil
	r.Identity = false
	r.OuterRange = 0
	r.Func = "predict_linear"
	r.PredictLinearSlopeColumn = "slope"
	want.Columns = []Column{{"labels", RoleAttributes}, {"value", RoleValue}, {Name: "slope"}}
	if got := r.RowType(); !got.Equal(want) {
		t.Fatalf("slope: %#v", got)
	}
	r.Func = "rate"
	want.Columns = want.Columns[:len(want.Columns)-1]
	if got := r.RowType(); !got.Equal(want) {
		t.Fatalf("non-predict slope ignored: %#v", got)
	}
}

func TestRowTypeMixedFloatNarrowing(t *testing.T) {
	mixed := &Scan{Columns: []string{MixedDiscriminatorColumn}, Roles: []Column{{MixedDiscriminatorColumn, RoleDiscriminator}}}
	filter := &Filter{Input: mixed, Predicate: &Binary{Op: OpEq, Left: &ColumnRef{Name: MixedDiscriminatorColumn}, Right: &LitInt{V: 0}}}
	if !IsMixedFloatNarrowing(filter) {
		t.Fatal("explicit float partition not recognized")
	}
	filter.Predicate.(*Binary).Right = &LitInt{V: 1}
	if IsMixedFloatNarrowing(filter) {
		t.Fatal("histogram partition accepted as float-only")
	}
	filter.Predicate = &Binary{Op: OpOr, Left: &Binary{Op: OpEq, Left: &ColumnRef{Name: MixedDiscriminatorColumn}, Right: &LitInt{V: 0}}, Right: &LitBool{V: true}}
	if IsMixedFloatNarrowing(filter) {
		t.Fatal("OR does not guarantee float-only rows")
	}
	filter.Predicate.(*Binary).Op = OpAnd
	if !IsMixedFloatNarrowing(filter) {
		t.Fatal("conjunctive float restriction not recognized")
	}
	filter.Input = &OneRow{}
	if IsMixedFloatNarrowing(filter) {
		t.Fatal("input without a discriminator accepted")
	}
	filter.Input = mixed
	filter.Predicate = &Binary{Op: OpEq, Left: &ColumnRef{Name: MixedDiscriminatorColumn, Qualifier: "other"}, Right: &LitInt{V: 0}}
	if IsMixedFloatNarrowing(filter) {
		t.Fatal("qualified foreign column accepted as this input's discriminator")
	}
	filter.Predicate.(*Binary).Left = &ColumnRef{Name: MixedDiscriminatorColumn}
	filter.Input = &Scan{Columns: []string{MixedDiscriminatorColumn, "actual_kind"}, Roles: []Column{{MixedDiscriminatorColumn, RoleOpaque}, {"actual_kind", RoleDiscriminator}}}
	if IsMixedFloatNarrowing(filter) {
		t.Fatal("same-spelled opaque column accepted as discriminator")
	}
	filter.Predicate.(*Binary).Left = &ColumnRef{Name: "actual_kind"}
	if !IsMixedFloatNarrowing(filter) {
		t.Fatal("explicitly declared discriminator name not recognized")
	}
	filter.Input = &CrossJoin{Left: mixed, Right: mixed}
	filter.Predicate.(*Binary).Left = &ColumnRef{Name: MixedDiscriminatorColumn}
	if IsMixedFloatNarrowing(filter) {
		t.Fatal("filtering one of two independent discriminators accepted")
	}
}

func TestRowTypeCanonicalHistogramPayload(t *testing.T) {
	full := Schema{Columns: HistogramPayloadColumns()}
	if !full.HasHistogramPayload() {
		t.Fatal("complete canonical payload missing")
	}
	for i := range full.Columns {
		partial := Schema{Columns: append(slices.Clone(full.Columns[:i]), full.Columns[i+1:]...)}
		if partial.HasHistogramPayload() {
			t.Fatalf("payload missing %s accepted", full.Columns[i].Name)
		}
	}
	raw := Schema{Columns: []Column{{"raw_count", RoleHistogramField}, {"raw_sum", RoleHistogramField}}}
	if raw.HasHistogramPayload() {
		t.Fatal("raw storage fields accepted as native payload")
	}
	full.Columns[0].Role = RoleOpaque
	if full.HasHistogramPayload() {
		t.Fatal("opaque same-named column accepted as histogram field")
	}
	if !(Schema{Columns: HistogramPayloadColumns()}).HasHistogramPayload() {
		t.Fatal("caller mutation changed canonical vocabulary")
	}
}

func TestSchemaSampleKind(t *testing.T) {
	floatRoles := []Column{
		{"source_name", RoleMetricName},
		{"source_labels", RoleAttributes},
		{"source_time", RoleTimestamp},
		{"source_value", RoleValue},
	}
	histogram := HistogramPayloadColumns()
	mixed := append(slices.Clone(floatRoles), histogram...)
	mixed = append(mixed, Column{"source_kind", RoleDiscriminator})

	for _, tc := range []struct {
		name string
		row  Schema
		want SampleKind
	}{
		{name: "float_custom_names", row: Schema{Columns: floatRoles}, want: SampleKindFloat},
		{name: "reduced_float", row: Schema{Columns: []Column{{"source_value", RoleValue}}}, want: SampleKindFloat},
		{name: "pure_histogram", row: Schema{Columns: histogram}, want: SampleKindHistogram},
		{name: "histogram_with_placeholder", row: Schema{Columns: append(slices.Clone(floatRoles), histogram...)}, want: SampleKindHistogram},
		{name: "mixed", row: Schema{Columns: mixed}, want: SampleKindMixed},
		{name: "opaque", row: Schema{Columns: []Column{{Name: "private"}}}, want: SampleKindOpaque},
		{name: "open_float", row: Schema{Columns: floatRoles, Open: true}, want: SampleKindOpaque},
		{name: "trace_roles", row: Schema{Columns: []Column{{"trace", RoleTraceID}, {"span", RoleSpanID}}}, want: SampleKindOpaque},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.SampleKind(); got != tc.want {
				t.Fatalf("SampleKind = %s, want %s; schema=%#v", got, tc.want, tc.row)
			}
		})
	}
}

func TestSchemaSampleKindRejectsMalformedContracts(t *testing.T) {
	floatRoles := []Column{{"labels", RoleAttributes}, {"value", RoleValue}}
	histogram := HistogramPayloadColumns()
	kind := Column{"source_kind", RoleDiscriminator}

	type sampleKindCase struct {
		name    string
		columns []Column
	}
	cases := []sampleKindCase{
		{name: "orphan_discriminator", columns: append(slices.Clone(floatRoles), kind)},
		{name: "mixed_without_value", columns: append(slices.Clone(histogram), kind)},
		{name: "duplicate_value_role", columns: append(slices.Clone(floatRoles), Column{"other_value", RoleValue})},
		{name: "unnamed_value", columns: []Column{{Role: RoleValue}}},
		{name: "shadowed_value", columns: append(slices.Clone(floatRoles), Column{Name: "value"})},
		{name: "duplicate_discriminator", columns: append(append(slices.Clone(floatRoles), histogram...), kind, Column{"other_kind", RoleDiscriminator})},
		{name: "unnamed_discriminator", columns: append(append(slices.Clone(floatRoles), histogram...), Column{Role: RoleDiscriminator})},
		{name: "shadowed_discriminator", columns: append(append(slices.Clone(floatRoles), histogram...), kind, Column{Name: kind.Name})},
		{name: "noncanonical_histogram_role", columns: append(slices.Clone(floatRoles), Column{"raw_count", RoleHistogramField})},
	}
	for i, field := range histogram {
		missing := slices.Delete(slices.Clone(histogram), i, i+1)
		wrongRole := slices.Clone(histogram)
		wrongRole[i].Role = RoleOpaque
		duplicate := append(slices.Clone(histogram), field)
		shadowed := append(slices.Clone(histogram), Column{Name: field.Name})
		cases = append(
			cases,
			sampleKindCase{name: "missing/" + field.Name, columns: missing},
			sampleKindCase{name: "wrong_role/" + field.Name, columns: wrongRole},
			sampleKindCase{name: "duplicate/" + field.Name, columns: duplicate},
			sampleKindCase{name: "shadowed/" + field.Name, columns: shadowed},
		)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Schema{Columns: tc.columns}).SampleKind(); got != SampleKindInvalid {
				t.Fatalf("SampleKind = %s, want %s; schema=%#v", got, SampleKindInvalid, tc.columns)
			}
		})
	}
}

func TestSchemaSampleKindUsesRolesNotSampleNames(t *testing.T) {
	for _, field := range HistogramPayloadColumns() {
		row := Schema{Columns: []Column{{Name: field.Name, Role: RoleValue}}}
		if got := row.SampleKind(); got != SampleKindFloat {
			t.Fatalf("value named %q classified as %s, want %s", field.Name, got, SampleKindFloat)
		}
	}
}

func TestSampleKindString(t *testing.T) {
	for _, tc := range []struct {
		kind SampleKind
		want string
	}{
		{SampleKindOpaque, "opaque"},
		{SampleKindFloat, "float"},
		{SampleKindHistogram, "histogram"},
		{SampleKindMixed, "mixed"},
		{SampleKindInvalid, "invalid"},
		{SampleKindInvalid + 1, "opaque"},
	} {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("SampleKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestRowTypeOpenCrossJoin(t *testing.T) {
	left := &Scan{Roles: []Column{{"known", RoleAttributes}}}
	right := &Scan{Columns: []string{"known", "maybe"}, Roles: []Column{{"known", RoleAttributes}, {"maybe", RoleTimestamp}}}
	got := (&CrossJoin{Left: left, Right: right}).RowType()
	want := Schema{Open: true, Columns: []Column{{"known", RoleAttributes}, {"R.known", RoleAttributes}}}
	if !got.Equal(want) {
		t.Fatalf("uncertain right names advertised: got %#v want %#v", got, want)
	}
}

func TestRowTypeUnionUsesFirstArmNames(t *testing.T) {
	left := &Scan{Columns: []string{"left_value"}, Roles: []Column{{"left_value", RoleValue}}}
	right := &Scan{Columns: []string{"right_value"}, Roles: []Column{{"right_value", RoleValue}}}
	union := &UnionAll{Inputs: []Node{left, right}}
	if got := union.RowType(); !got.Equal(left.RowType()) {
		t.Fatalf("positional union names: %#v", got)
	}
	if left.RowType().Columns[0].Role != right.RowType().Columns[0].Role {
		t.Fatal("valid positional union arms disagree on role")
	}
}

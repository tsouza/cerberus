package ast

import "testing"

// TestReferencesIntrinsic_Name walks a real parsed query for every stage
// shape that can carry an attribute. The `name` intrinsic is the one the
// Tempo response shaper reads (spanSets[].spans[].name), so the false
// rows matter as much as the true ones: a walker that over-reports makes
// cerberus populate a field reference Tempo leaves empty.
func TestReferencesIntrinsic_Name(t *testing.T) {
	t.Parallel()

	cases := []struct {
		query string
		want  bool
		why   string
	}{
		// Plain spanset filters.
		{`{ name = "GET /api/checkout/17" }`, true, "bare intrinsic on the LHS"},
		{`{ "GET /x" = name }`, true, "bare intrinsic on the RHS"},
		{`{ name != nil }`, true, "existence-style comparison"},
		{`{ name =~ "GET.*" }`, true, "regex comparison"},
		{`{ resource.service.name = "checkout" }`, false, "attribute literally spelled service.name is not the intrinsic"},
		{`{ status = error }`, false, "a different intrinsic"},
		{`{ duration > 1s && .a = 1 }`, false, "no name anywhere"},

		// span:name is the scoped spelling of the SAME intrinsic — both
		// cerberus (parser.go scopedIntrinsic) and upstream (expr.y:480,
		// `SPAN_COLON NAME { NewIntrinsic(IntrinsicName) }`) fold it to
		// IntrinsicName, so it must populate the wire field too.
		{`{ span:name = "checkout" }`, true, "scoped spelling folds to IntrinsicName"},

		// Nested / negated field expressions.
		{`{ .a = 1 && (name = "b" || .c = 2) }`, true, "nested binary operation"},
		{`{ !(name = "a") }`, true, "unary operation"},

		// Spanset operations — structural and set. Upstream collects
		// conditions query-wide, so either side counts.
		{`{ name = "a" } && { .b = 1 }`, true, "set op, LHS"},
		{`{ .b = 1 } && { name = "a" }`, true, "set op, RHS"},
		{`{ .a = 1 } >> { name = "b" }`, true, "structural descendant, RHS"},
		{`{ name = "a" } >> { .b = 1 }`, true, "structural descendant, LHS"},
		{`{ .a = 1 } >> { .b = 1 }`, false, "structural with no name"},
		{`({ .a = 1 } &>> { .b = 1 }) || ({ name = "c" })`, true, "nested spanset operations"},

		// Later pipeline stages.
		{`{ .a = 1 } | select(name)`, true, "select list"},
		{`{ .a = 1 } | select(.x, status)`, false, "select list without name"},
		{`{ .a = 1 } | by(name) | count() > 1`, true, "by() grouping"},
		{`{ .a = 1 } | by(.x) | count() > 1`, false, "by() without name"},
		{`{ .a = 1 } | count() > 2`, false, "scalar filter over count()"},
		{`{ .a = 1 } | max(duration) > 1s`, false, "scalar filter over an aggregate"},
		{`{ .a = 1 } | coalesce() | { name = "b" }`, true, "stage after coalesce()"},

		// Metrics first stages. Search never runs these, but upstream's
		// extractConditions covers them, so the walker does too.
		{`{ .a = 1 } | rate() by(name)`, true, "metrics group-by"},
		{`{ .a = 1 } | rate() by(.x)`, false, "metrics group-by without name"},
		{`{ .a = 1 } | avg_over_time(duration) by(name)`, true, "avg_over_time group-by"},
		{`{ .a = 1 } | quantile_over_time(duration, 0.9) by(name)`, true, "quantile group-by"},
		{`{ .a = 1 } | compare({ name = "b" })`, true, "compare() inner filter"},
		{`{ .a = 1 } | compare({ .b = 1 })`, false, "compare() without name"},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			expr, err := Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			if got := expr.ReferencesIntrinsic(IntrinsicName); got != tc.want {
				t.Errorf("ReferencesIntrinsic(IntrinsicName) = %v, want %v (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// TestReferencesIntrinsic_OtherIntrinsics checks the walk discriminates
// between intrinsics rather than reporting "some intrinsic is present".
func TestReferencesIntrinsic_OtherIntrinsics(t *testing.T) {
	t.Parallel()

	expr, err := Parse(`{ status = error && kind = server }`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for in, want := range map[Intrinsic]bool{
		IntrinsicStatus:   true,
		IntrinsicKind:     true,
		IntrinsicName:     false,
		IntrinsicDuration: false,
	} {
		if got := expr.ReferencesIntrinsic(in); got != want {
			t.Errorf("ReferencesIntrinsic(%v) = %v, want %v", in, got, want)
		}
	}
}

// TestReferencesIntrinsic_NoneIsNeverReferenced pins the sentinel case.
// IntrinsicNone is what every ordinary attribute carries, so answering
// "yes" for it would make the shaper populate names on every query.
func TestReferencesIntrinsic_NoneIsNeverReferenced(t *testing.T) {
	t.Parallel()

	expr, err := Parse(`{ .foo = "bar" }`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if expr.ReferencesIntrinsic(IntrinsicNone) {
		t.Error("ReferencesIntrinsic(IntrinsicNone) = true, want false")
	}
	var nilExpr *RootExpr
	if nilExpr.ReferencesIntrinsic(IntrinsicName) {
		t.Error("(*RootExpr)(nil).ReferencesIntrinsic = true, want false")
	}
}

// TestReferencesIntrinsic_MatchesIntrinsicFieldNotName pins that the walk
// keys off Attribute.Intrinsic and not Attribute.Name. An ordinary
// attribute can be spelled "name" while carrying IntrinsicNone, and it
// must not count — that is the shape upstream's NewAttribute produces for
// the dotted form (`.name`), where the attribute is a user key rather
// than the intrinsic.
func TestReferencesIntrinsic_MatchesIntrinsicFieldNotName(t *testing.T) {
	t.Parallel()

	ordinary := Attribute{Name: "name", Intrinsic: IntrinsicNone}
	expr := &RootExpr{Pipeline: Pipeline{Elements: []PipelineElement{
		&SpansetFilter{Expression: &BinaryOperation{
			Op: OpEqual, LHS: ordinary, RHS: NewStaticString("x"),
		}},
	}}}
	if expr.ReferencesIntrinsic(IntrinsicName) {
		t.Error("attribute named \"name\" with IntrinsicNone: got true, want false")
	}
}

// TestReferencesIntrinsic_ValueFormSpansetFilter covers the hand-built
// AST shape the parser never emits. Every SpansetFilter method has a
// value receiver, so a value satisfies PipelineElement just as the
// pointer does; a walk that only matched *SpansetFilter would silently
// return false here instead of failing loudly.
func TestReferencesIntrinsic_ValueFormSpansetFilter(t *testing.T) {
	t.Parallel()

	nameEq := &BinaryOperation{
		Op:  OpEqual,
		LHS: NewIntrinsic(IntrinsicName),
		RHS: NewStaticString("GET /x"),
	}
	valueForm := &RootExpr{Pipeline: Pipeline{Elements: []PipelineElement{
		SpansetFilter{Expression: nameEq},
	}}}
	if !valueForm.ReferencesIntrinsic(IntrinsicName) {
		t.Error("value-form SpansetFilter: got false, want true")
	}
	pointerForm := &RootExpr{Pipeline: Pipeline{Elements: []PipelineElement{
		&SpansetFilter{Expression: nameEq},
	}}}
	if !pointerForm.ReferencesIntrinsic(IntrinsicName) {
		t.Error("pointer-form SpansetFilter: got false, want true")
	}
	// Same via the SpansetExpression branch, where both forms are also
	// reachable.
	nested := &RootExpr{Pipeline: Pipeline{Elements: []PipelineElement{
		SpansetOperation{
			Op:  OpSpansetDescendant,
			LHS: SpansetFilter{Expression: nameEq},
			RHS: &SpansetFilter{Expression: &BinaryOperation{
				Op: OpEqual, LHS: NewAttribute("a"), RHS: NewStaticInt(1),
			}},
		},
	}}}
	if !nested.ReferencesIntrinsic(IntrinsicName) {
		t.Error("value-form SpansetFilter under SpansetOperation: got false, want true")
	}
}

package ast

import (
	"errors"
	"strings"
	"testing"
)

// TestAttributeRendering pins Attribute.String: the qualifiers (`parent`,
// then the scope) joined with the name by dots, a leading dot for an
// unqualified user attribute with a name, an intrinsic or its scoped
// spelling bare, and a name holding a rune a bare attribute cannot carry
// wrapped in double quotes.
func TestAttributeRendering(t *testing.T) {
	t.Parallel()

	cases := []struct {
		attr Attribute
		want string
	}{
		{NewAttribute("foo"), ".foo"},
		{NewAttribute("http.status_code"), ".http.status_code"},
		{NewAttribute(""), ""},
		{NewAttribute("a b"), `."a b"`},
		{NewAttribute("duration"), ".duration"},
		{NewScopedAttribute(AttributeScopeSpan, false, "http.method"), "span.http.method"},
		{NewScopedAttribute(AttributeScopeResource, false, "service.name"), "resource.service.name"},
		{NewScopedAttribute(AttributeScopeNone, true, "x"), "parent.x"},
		{NewScopedAttribute(AttributeScopeResource, true, "x"), "parent.resource.x"},
		{NewScopedAttribute(AttributeScopeSpan, false, ""), "span."},
		{NewScopedAttribute(AttributeScopeSpan, false, "has space"), `span."has space"`},
		{NewScopedAttribute(AttributeScopeNone, false, "duration"), "duration"},
		{NewIntrinsic(IntrinsicStatus), "status"},
		{Attribute{Scope: AttributeScopeSpan, Intrinsic: IntrinsicDuration}, "span.duration"},
		{Attribute{Intrinsic: IntrinsicStatus, Spelling: ScopedIntrinsicSpanStatus}, ScopedIntrinsicSpanStatus.String()},
	}
	for _, c := range cases {
		if got := c.attr.String(); got != c.want {
			t.Errorf("%#v.String() = %q, want %q", c.attr, got, c.want)
		}
	}
}

// TestParseIdentifierVerdicts pins ParseIdentifier: the text is read as the
// sole expression of a spanset filter, and anything but a bare attribute
// reference is rejected with the identifier and the reason.
func TestParseIdentifierVerdicts(t *testing.T) {
	t.Parallel()

	accepted := []struct {
		in   string
		want Attribute
	}{
		{".foo", NewAttribute("foo")},
		{"span.http.status_code", NewScopedAttribute(AttributeScopeSpan, false, "http.status_code")},
		{"duration", NewIntrinsic(IntrinsicDuration)},
		{"parent.resource.x", NewScopedAttribute(AttributeScopeResource, true, "x")},
	}
	for _, c := range accepted {
		got, err := ParseIdentifier(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseIdentifier(%q) = (%#v, %v), want (%#v, nil)", c.in, got, err, c.want)
		}
	}

	rejected := []struct {
		in, want string
	}{
		{".a = 1", "failed to parse identifier .a = 1: expected Attribute but got *ast.BinaryOperation"},
		{"true", "failed to parse identifier true: expected Attribute but got ast.Static"},
		{"} && {", "failed to parse identifier } && {: expected SpansetFilter but got ast.SpansetOperation"},
	}
	for _, c := range rejected {
		got, err := ParseIdentifier(c.in)
		if err == nil || err.Error() != c.want || got != (Attribute{}) {
			t.Errorf("ParseIdentifier(%q) = (%#v, %v), want an empty attribute and %q", c.in, got, err, c.want)
		}
	}

	_, err := ParseIdentifier("(")
	var parseErr *ParseError
	if err == nil || !strings.HasPrefix(err.Error(), "failed to parse identifier (: ") || !errors.As(err, &parseErr) {
		t.Errorf("ParseIdentifier(%q) = %v, want the parse error wrapped under the identifier", "(", err)
	}
}

// TestRootExprRendering pins RootExpr.String: the spanset pipeline, then
// each metrics stage present after ` | `, then the hints.
func TestRootExprRendering(t *testing.T) {
	t.Parallel()

	full, err := Parse(`{ .a = 1 } | rate() by (.b) | topk(2) with(sample=true)`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if full.MetricsPipeline == nil || full.MetricsSecondStage == nil || full.Hints == nil {
		t.Fatalf("the probe query lacks a part: %#v", full)
	}
	pipeline := full.Pipeline.String()
	first := full.MetricsPipeline.String()
	second := full.MetricsSecondStage.String()
	hints := full.Hints.String()
	if hints == "" {
		t.Fatalf("the probe query's hints render empty")
	}

	cases := []struct {
		root RootExpr
		want string
	}{
		{RootExpr{Pipeline: full.Pipeline}, pipeline},
		{RootExpr{Pipeline: full.Pipeline, MetricsPipeline: full.MetricsPipeline}, pipeline + " | " + first},
		{RootExpr{Pipeline: full.Pipeline, MetricsPipeline: full.MetricsPipeline, MetricsSecondStage: full.MetricsSecondStage}, pipeline + " | " + first + " | " + second},
		{RootExpr{Pipeline: full.Pipeline, MetricsSecondStage: full.MetricsSecondStage}, pipeline + " | " + second},
		{RootExpr{Pipeline: full.Pipeline, Hints: full.Hints}, pipeline + hints},
		{RootExpr{Pipeline: full.Pipeline, Hints: &Hints{}}, pipeline},
		{*full, pipeline + " | " + first + " | " + second + hints},
	}
	for _, c := range cases {
		if got := c.root.String(); got != c.want {
			t.Errorf("String() = %q, want %q", got, c.want)
		}
	}
}

// TestStaticElementsBehaviour pins Static.Elements: an array static yields
// each member as its scalar static with its index, in order, and stops
// when the consumer does; any other static yields itself once at index 0.
func TestStaticElementsBehaviour(t *testing.T) {
	t.Parallel()

	cases := []struct {
		static Static
		want   []Static
	}{
		{NewStaticIntArray([]int{3, 1}), []Static{NewStaticInt(3), NewStaticInt(1)}},
		{NewStaticFloatArray([]float64{1.5, -2}), []Static{NewStaticFloat(1.5), NewStaticFloat(-2)}},
		{NewStaticStringArray([]string{"a", "b", "c"}), []Static{NewStaticString("a"), NewStaticString("b"), NewStaticString("c")}},
		{NewStaticBooleanArray([]bool{true, false}), []Static{NewStaticBool(true), NewStaticBool(false)}},
		{NewStaticIntArray(nil), nil},
		{NewStaticInt(7), []Static{NewStaticInt(7)}},
		{NewStaticString("x"), []Static{NewStaticString("x")}},
		{StaticNil, []Static{StaticNil}},
	}
	for _, c := range cases {
		var got []Static
		for i, el := range c.static.Elements() {
			if i != len(got) {
				t.Fatalf("%s: element %d arrived at index %d", c.static, len(got), i)
			}
			got = append(got, el)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%s: yielded %v, want %v", c.static, got, c.want)
		}
		for i := range got {
			if got[i].Type != c.want[i].Type || got[i].String() != c.want[i].String() {
				t.Fatalf("%s: yielded %v, want %v", c.static, got, c.want)
			}
		}

		// Stopping after the first element must end the sequence; a
		// sequence that kept yielding would panic here.
		taken := 0
		for range c.static.Elements() {
			taken++
			break
		}
		if want := min(len(c.want), 1); taken != want {
			t.Fatalf("%s: took %d elements before stopping, want %d", c.static, taken, want)
		}
	}
}

// TestAttrLiteralOperandsBehaviour pins attrLiteralOperands: a comparison
// whose operator is listed yields its attribute and literal from either
// side; the left side is taken as the attribute whenever it is one, so an
// attribute on both sides is no match.
func TestAttrLiteralOperandsBehaviour(t *testing.T) {
	t.Parallel()

	attr := NewAttribute("x")
	other := NewAttribute("y")
	one := NewStaticInt(1)
	bin := func(op Operator, lhs, rhs FieldExpression) FieldExpression {
		return &BinaryOperation{Op: op, LHS: lhs, RHS: rhs}
	}

	cases := []struct {
		name     string
		fe       FieldExpression
		ops      []Operator
		wantOK   bool
		wantAttr Attribute
	}{
		{"attribute on the left", bin(OpEqual, attr, one), []Operator{OpEqual}, true, attr},
		{"attribute on the right", bin(OpEqual, one, attr), []Operator{OpEqual}, true, attr},
		{"any listed operator", bin(OpRegex, attr, NewStaticString("a.*")), []Operator{OpEqual, OpRegex}, true, attr},
		{"an array literal", bin(OpIn, attr, NewStaticIntArray([]int{1, 2})), []Operator{OpIn}, true, attr},
		{"an operator not listed", bin(OpNotEqual, attr, one), []Operator{OpEqual}, false, Attribute{}},
		{"no operators listed", bin(OpEqual, attr, one), nil, false, Attribute{}},
		{"attributes on both sides", bin(OpEqual, attr, other), []Operator{OpEqual}, false, Attribute{}},
		{"literals on both sides", bin(OpEqual, one, one), []Operator{OpEqual}, false, Attribute{}},
		{"a nested operand", bin(OpEqual, attr, bin(OpAdd, one, one)), []Operator{OpEqual}, false, Attribute{}},
		{"not a binary operation", attr, []Operator{OpEqual}, false, Attribute{}},
	}
	for _, c := range cases {
		gotAttr, gotVal, ok := attrLiteralOperands(c.fe, c.ops...)
		if ok != c.wantOK || gotAttr != c.wantAttr {
			t.Errorf("%s: attrLiteralOperands = (%#v, %v, %v), want attribute %#v and %v", c.name, gotAttr, gotVal, ok, c.wantAttr, c.wantOK)
			continue
		}
		if !ok && gotVal.Type != TypeNil {
			t.Errorf("%s: a failed extraction returned the literal %v, want the nil static", c.name, gotVal)
		}
		if ok {
			want := c.fe.(*BinaryOperation).RHS
			if _, rightIsAttr := want.(Attribute); rightIsAttr {
				want = c.fe.(*BinaryOperation).LHS
			}
			if wantVal := want.(Static); gotVal.Type != wantVal.Type || gotVal.String() != wantVal.String() {
				t.Errorf("%s: literal = %v, want %v", c.name, gotVal, wantVal)
			}
		}
	}
}

package ast

import (
	"math"
	"slices"
	"strings"
	"testing"
	"text/scanner"
	"time"
	"unicode"
)

// The rules under test, named once so the tables below read as a
// specification rather than as calls into the implementation.
var (
	operandsComparable    = StaticType.comparableWith
	unaryOperandAccepted  = Operator.acceptsUnaryOperand
	bareAttributeRune     = allowedInBareAttribute
	everyStaticType       = staticTypesThrough(TypeKind)
	everyOperator         = operatorsThrough(OpRegexMatchNone)
	numericStaticTypes    = []StaticType{TypeInt, TypeFloat, TypeDuration}
	wildcardOperandTypes  = []StaticType{TypeNil, TypeAttribute}
	existenceOperators    = []Operator{OpExists, OpNotExists}
	structuralPunctuation = "{}()=~!<>&|^,"
	attributeWhitespace   = " \t\n\v\f\r\u0085         　"
	attributeRuneSweepEnd = rune(0x3100)
)

func staticTypesThrough(last StaticType) []StaticType {
	var out []StaticType
	for t := StaticType(0); t <= last; t++ {
		out = append(out, t)
	}
	return out
}

func operatorsThrough(last Operator) []Operator {
	var out []Operator
	for op := Operator(0); op <= last; op++ {
		out = append(out, op)
	}
	return out
}

func containsType(set []StaticType, t StaticType) bool {
	return slices.Contains(set, t)
}

// TestOperandsComparable_EveryPair pins which two statically-known operand
// types may meet under a binary operator, over every pair of types: nil
// and the query-time-typed attribute meet anything, the numeric family
// (int, float, duration) is inter-comparable, and otherwise only a type
// meets itself.
func TestOperandsComparable_EveryPair(t *testing.T) {
	t.Parallel()

	for _, a := range everyStaticType {
		for _, b := range everyStaticType {
			want := containsType(wildcardOperandTypes, a) || containsType(wildcardOperandTypes, b) ||
				a == b || (containsType(numericStaticTypes, a) && containsType(numericStaticTypes, b))
			if got := operandsComparable(a, b); got != want {
				t.Errorf("operands %v and %v comparable = %v, want %v", a, b, got, want)
			}
		}
	}
}

// TestUnaryOperandAccepted_EveryPair pins which operand type each unary
// operator accepts, over every operator and every type: the attribute
// type is accepted by any operator, `-` takes a numeric operand, `!` a
// boolean one, the two existence operators take anything, and no other
// operator is unary.
func TestUnaryOperandAccepted_EveryPair(t *testing.T) {
	t.Parallel()

	for _, op := range everyOperator {
		for _, ty := range everyStaticType {
			want := ty == TypeAttribute
			switch op {
			case OpSub:
				want = want || containsType(numericStaticTypes, ty)
			case OpNot:
				want = want || ty == TypeBoolean
			case existenceOperators[0], existenceOperators[1]:
				want = true
			}
			if got := unaryOperandAccepted(op, ty); got != want {
				t.Errorf("unary %v accepts %v = %v, want %v", op, ty, got, want)
			}
		}
	}
}

// TestBareAttributeRune_Sweep pins the runes an unquoted attribute name
// may hold: everything except whitespace, the grammar's structural
// punctuation and end of input.
func TestBareAttributeRune_Sweep(t *testing.T) {
	t.Parallel()

	if bareAttributeRune(scanner.EOF) {
		t.Error("end of input accepted in a bare attribute name")
	}
	for _, r := range structuralPunctuation + attributeWhitespace {
		if bareAttributeRune(r) {
			t.Errorf("%q accepted in a bare attribute name", r)
		}
	}
	for r := range attributeRuneSweepEnd {
		want := !unicode.IsSpace(r) && !strings.ContainsRune(structuralPunctuation, r)
		if got := bareAttributeRune(r); got != want {
			t.Errorf("%U (%q) in a bare attribute name = %v, want %v", r, r, got, want)
		}
	}
	for _, r := range "._-:/@#$%*+?[]\"'`;0aZé日\U0001F600" {
		if !bareAttributeRune(r) {
			t.Errorf("%q rejected in a bare attribute name", r)
		}
	}
}

// TestStaticEncodeToString_EveryForm pins the surface rendering of every
// static form, both with and without string quoting: the float forms that
// do and do not gain a `.0`, empty arrays, string arrays (whose elements
// are backticked either way), and a type with no literal form.
func TestStaticEncodeToString_EveryForm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		s              Static
		quoted, unquot string
	}{
		{NewStaticNil(), "nil", "nil"},
		{NewStaticInt(0), "0", "0"},
		{NewStaticInt(-12), "-12", "-12"},
		{NewStaticFloat(0), "0.0", "0.0"},
		{NewStaticFloat(-0.5), "-0.5", "-0.5"},
		{NewStaticFloat(1e21), "1e+21", "1e+21"},
		{NewStaticFloat(1e-7), "1e-07", "1e-07"},
		{NewStaticFloat(123456789), "1.23456789e+08", "1.23456789e+08"},
		{NewStaticFloat(100), "100.0", "100.0"},
		{NewStaticFloat(math.NaN()), "NaN.0", "NaN.0"},
		{NewStaticFloat(math.Inf(1)), "+Inf.0", "+Inf.0"},
		{NewStaticFloat(math.Inf(-1)), "-Inf.0", "-Inf.0"},
		{NewStaticString(""), "``", ""},
		{NewStaticString("a b`c"), "`a b`c`", "a b`c"},
		{NewStaticBool(true), "true", "true"},
		{NewStaticDuration(0), "0s", "0s"},
		{NewStaticDuration(1500 * time.Millisecond), "1.5s", "1.5s"},
		{NewStaticStatus(StatusOk), "ok", "ok"},
		{NewStaticStatus(StatusUnset), "unset", "unset"},
		{NewStaticKind(KindUnspecified), "unspecified", "unspecified"},
		{NewStaticKind(KindConsumer), "consumer", "consumer"},
		{NewStaticIntArray(nil), "[]", "[]"},
		{NewStaticIntArray([]int{-1}), "[-1]", "[-1]"},
		{NewStaticFloatArray([]float64{3, 1e21, 0.25}), "[3.0, 1e+21, 0.25]", "[3.0, 1e+21, 0.25]"},
		{NewStaticStringArray(nil), "[]", "[]"},
		{NewStaticStringArray([]string{"x", ""}), "[`x`, ``]", "[`x`, ``]"},
		{NewStaticBooleanArray([]bool{false}), "[false]", "[false]"},
		{Static{Type: TypeSpanset}, "static(1)", "static(1)"},
		{Static{Type: TypeAttribute}, "static(2)", "static(2)"},
		{Static{Type: TypeKind + 1}, "static(14)", "static(14)"},
	}
	for _, c := range cases {
		if got := c.s.EncodeToString(true); got != c.quoted {
			t.Errorf("%v EncodeToString(true) = %q, want %q", c.s.Type, got, c.quoted)
		}
		if got := c.s.EncodeToString(false); got != c.unquot {
			t.Errorf("%v EncodeToString(false) = %q, want %q", c.s.Type, got, c.unquot)
		}
		if got := c.s.String(); got != c.quoted {
			t.Errorf("%v String() = %q, want %q", c.s.Type, got, c.quoted)
		}
	}
}

package ast

import (
	"iter"
	"strconv"
	"strings"
	"time"
)

// Static is a literal value in a query: an int, float, string, bool,
// duration, status, kind, or an array of one of the scalar kinds.
//
// Unlike the reference parser, which bit-packs every value into a shared
// word to keep span evaluation allocation-free, cerberus only parses and
// lowers — so Static uses a plain, readable layout. The discriminator
// Type is exported (lowering reads it directly); the value storage is
// private and reached through typed accessors that return (value, ok)
// where ok is false when Type does not match the requested kind.
type Static struct {
	Type StaticType

	num    int64   // int, duration (ns), status, kind, bool (0/1)
	flt    float64 // float
	str    string  // string
	ints   []int
	floats []float64
	strs   []string
	bools  []bool
}

// Convenience zero/boolean singletons.
var (
	StaticNil   = NewStaticNil()
	StaticTrue  = NewStaticBool(true)
	StaticFalse = NewStaticBool(false)
)

func NewStaticNil() Static { return Static{Type: TypeNil} }

func NewStaticInt(i int) Static { return Static{Type: TypeInt, num: int64(i)} }

func NewStaticFloat(f float64) Static { return Static{Type: TypeFloat, flt: f} }

func NewStaticString(s string) Static { return Static{Type: TypeString, str: s} }

func NewStaticBool(b bool) Static {
	s := Static{Type: TypeBoolean}
	if b {
		s.num = 1
	}
	return s
}

func NewStaticDuration(d time.Duration) Static {
	return Static{Type: TypeDuration, num: int64(d)}
}

func NewStaticStatus(st Status) Static {
	return Static{Type: TypeStatus, num: int64(st)}
}

func NewStaticKind(k Kind) Static {
	return Static{Type: TypeKind, num: int64(k)}
}

func NewStaticIntArray(v []int) Static {
	return Static{Type: TypeIntArray, ints: v}
}

func NewStaticFloatArray(v []float64) Static {
	return Static{Type: TypeFloatArray, floats: v}
}

func NewStaticStringArray(v []string) Static {
	return Static{Type: TypeStringArray, strs: v}
}

func NewStaticBooleanArray(v []bool) Static {
	return Static{Type: TypeBooleanArray, bools: v}
}

func (Static) isFieldExpression()  {}
func (Static) isScalarExpression() {}

func (s Static) impliedType() StaticType { return s.Type }

// IsNil reports whether the literal is the nil literal.
func (s Static) IsNil() bool { return s.Type == TypeNil }

func (s Static) Int() (int, bool) {
	if s.Type != TypeInt {
		return 0, false
	}
	return int(s.num), true
}

// Float coerces any numeric static to float64; non-numeric statics yield 0.
func (s Static) Float() float64 {
	switch s.Type {
	case TypeFloat:
		return s.flt
	case TypeInt, TypeDuration:
		return float64(s.num)
	default:
		return 0
	}
}

func (s Static) Bool() (bool, bool) {
	if s.Type != TypeBoolean {
		return false, false
	}
	return s.num != 0, true
}

func (s Static) Duration() (time.Duration, bool) {
	if s.Type != TypeDuration {
		return 0, false
	}
	return time.Duration(s.num), true
}

func (s Static) Status() (Status, bool) {
	if s.Type != TypeStatus {
		return 0, false
	}
	return Status(s.num), true
}

func (s Static) Kind() (Kind, bool) {
	if s.Type != TypeKind {
		return 0, false
	}
	return Kind(s.num), true
}

func (s Static) IntArray() ([]int, bool) {
	if s.Type != TypeIntArray {
		return nil, false
	}
	return s.ints, true
}

func (s Static) Int64Array() ([]int64, bool) {
	if s.Type != TypeIntArray {
		return nil, false
	}
	out := make([]int64, len(s.ints))
	for i, v := range s.ints {
		out[i] = int64(v)
	}
	return out, true
}

func (s Static) FloatArray() ([]float64, bool) {
	if s.Type != TypeFloatArray {
		return nil, false
	}
	return s.floats, true
}

func (s Static) StringArray() ([]string, bool) {
	if s.Type != TypeStringArray {
		return nil, false
	}
	return s.strs, true
}

func (s Static) BooleanArray() ([]bool, bool) {
	if s.Type != TypeBooleanArray {
		return nil, false
	}
	return s.bools, true
}

// Elements iterates the members of an array static; a scalar static yields
// itself once at index 0.
func (s Static) Elements() iter.Seq2[int, Static] {
	return func(yield func(int, Static) bool) {
		switch s.Type {
		case TypeIntArray:
			for i, v := range s.ints {
				if !yield(i, NewStaticInt(v)) {
					return
				}
			}
		case TypeFloatArray:
			for i, v := range s.floats {
				if !yield(i, NewStaticFloat(v)) {
					return
				}
			}
		case TypeStringArray:
			for i, v := range s.strs {
				if !yield(i, NewStaticString(v)) {
					return
				}
			}
		case TypeBooleanArray:
			for i, v := range s.bools {
				if !yield(i, NewStaticBool(v)) {
					return
				}
			}
		default:
			yield(0, s)
		}
	}
}

// Equals reports value equality, treating int/float/duration as
// cross-comparable numerics and int as comparable with status/kind, in
// line with the language's loose numeric comparison rules. A nil operand
// on either side is never equal.
func (s Static) Equals(o *Static) bool {
	if s.Type == TypeNil || o.Type == TypeNil {
		return false
	}
	if s.Type.isNumeric() && o.Type.isNumeric() {
		return s.Float() == o.Float()
	}
	switch s.Type {
	case TypeInt:
		if o.Type == TypeStatus || o.Type == TypeKind {
			return s.num == o.num
		}
	case TypeStatus, TypeKind:
		if o.Type == TypeInt {
			return s.num == o.num
		}
	}
	if s.Type != o.Type {
		return false
	}
	switch s.Type {
	case TypeString:
		return s.str == o.str
	case TypeBoolean, TypeStatus, TypeKind:
		return s.num == o.num
	case TypeIntArray:
		return slicesEqual(s.ints, o.ints)
	case TypeFloatArray:
		return slicesEqual(s.floats, o.floats)
	case TypeStringArray:
		return slicesEqual(s.strs, o.strs)
	case TypeBooleanArray:
		return slicesEqual(s.bools, o.bools)
	default:
		return false
	}
}

func slicesEqual[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s Static) String() string { return s.EncodeToString(true) }

// EncodeToString renders the literal in TraceQL surface syntax. When
// quotes is true string values are wrapped in backticks (the form used
// when printing a whole query); when false the bare string is returned
// (used when a literal stands in for an identifier).
func (s Static) EncodeToString(quotes bool) string {
	switch s.Type {
	case TypeNil:
		return "nil"
	case TypeInt:
		return strconv.FormatInt(s.num, 10)
	case TypeFloat:
		f := strconv.FormatFloat(s.flt, 'g', -1, 64)
		if !strings.ContainsAny(f, "e.") {
			f += ".0"
		}
		return f
	case TypeString:
		if quotes {
			return "`" + s.str + "`"
		}
		return s.str
	case TypeBoolean:
		return strconv.FormatBool(s.num != 0)
	case TypeDuration:
		return time.Duration(s.num).String()
	case TypeStatus:
		return Status(s.num).String()
	case TypeKind:
		return Kind(s.num).String()
	case TypeIntArray:
		return encodeArray(s.ints, false)
	case TypeFloatArray:
		return encodeArray(s.floats, false)
	case TypeStringArray:
		return encodeArray(s.strs, true)
	case TypeBooleanArray:
		return encodeArray(s.bools, false)
	default:
		return "static(" + strconv.Itoa(int(s.Type)) + ")"
	}
}

func encodeArray[T any](vals []T, quote bool) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range vals {
		if i > 0 {
			b.WriteString(", ")
		}
		if quote {
			b.WriteByte('`')
		}
		b.WriteString(scalarString(v))
		if quote {
			b.WriteByte('`')
		}
	}
	b.WriteByte(']')
	return b.String()
}

func scalarString(v any) string {
	switch x := v.(type) {
	case int:
		return strconv.Itoa(x)
	case float64:
		f := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(f, "e.") {
			f += ".0"
		}
		return f
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	default:
		return ""
	}
}

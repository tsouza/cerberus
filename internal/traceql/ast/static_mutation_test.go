package ast

import (
	"testing"
	"time"
)

// Mutation-coverage tests for static.go: the typed (value, ok) accessors, the
// numeric coercion, value equality, array iteration, and surface encoding.

// TestStaticAccessorsMatchType pins that each typed accessor returns ok=true
// only for its own StaticType and ok=false otherwise. Negating any of the
// `s.Type != TypeX` guards (e.g. the Status accessor) flips one of these.
func TestStaticAccessorsMatchType(t *testing.T) {
	t.Parallel()

	if v, ok := NewStaticInt(7).Int(); !ok || v != 7 {
		t.Errorf("Int() = (%d,%v); want (7,true)", v, ok)
	}
	if _, ok := NewStaticString("x").Int(); ok {
		t.Error("Int() on string returned ok=true")
	}

	if v, ok := NewStaticBool(true).Bool(); !ok || !v {
		t.Errorf("Bool() = (%v,%v); want (true,true)", v, ok)
	}
	if v, ok := NewStaticBool(false).Bool(); !ok || v {
		t.Errorf("Bool(false) = (%v,%v); want (false,true)", v, ok)
	}
	if _, ok := NewStaticInt(0).Bool(); ok {
		t.Error("Bool() on int returned ok=true")
	}

	if v, ok := NewStaticDuration(5 * time.Second).Duration(); !ok || v != 5*time.Second {
		t.Errorf("Duration() = (%v,%v); want (5s,true)", v, ok)
	}
	if _, ok := NewStaticInt(5).Duration(); ok {
		t.Error("Duration() on int returned ok=true")
	}

	if v, ok := NewStaticStatus(StatusOk).Status(); !ok || v != StatusOk {
		t.Errorf("Status() = (%v,%v); want (ok,true)", v, ok)
	}
	if _, ok := NewStaticInt(1).Status(); ok {
		t.Error("Status() on int returned ok=true")
	}

	if v, ok := NewStaticKind(KindClient).Kind(); !ok || v != KindClient {
		t.Errorf("Kind() = (%v,%v); want (client,true)", v, ok)
	}
	if _, ok := NewStaticInt(1).Kind(); ok {
		t.Error("Kind() on int returned ok=true")
	}
}

// TestStaticArrayAccessors pins the array accessors and the int64 widening.
func TestStaticArrayAccessors(t *testing.T) {
	t.Parallel()

	if v, ok := NewStaticIntArray([]int{1, 2, 3}).IntArray(); !ok || len(v) != 3 || v[2] != 3 {
		t.Errorf("IntArray() = (%v,%v)", v, ok)
	}
	if v, ok := NewStaticIntArray([]int{1, 2, 3}).Int64Array(); !ok || len(v) != 3 || v[2] != int64(3) {
		t.Errorf("Int64Array() = (%v,%v)", v, ok)
	}
	if _, ok := NewStaticFloatArray([]float64{1}).IntArray(); ok {
		t.Error("IntArray() on float array returned ok=true")
	}

	if v, ok := NewStaticFloatArray([]float64{1.5, 2.5}).FloatArray(); !ok || len(v) != 2 || v[1] != 2.5 {
		t.Errorf("FloatArray() = (%v,%v)", v, ok)
	}
	if v, ok := NewStaticStringArray([]string{"a", "b"}).StringArray(); !ok || len(v) != 2 || v[1] != "b" {
		t.Errorf("StringArray() = (%v,%v)", v, ok)
	}
	if v, ok := NewStaticBooleanArray([]bool{true, false}).BooleanArray(); !ok || len(v) != 2 || v[0] != true {
		t.Errorf("BooleanArray() = (%v,%v)", v, ok)
	}
}

// TestStaticFloat pins the numeric coercion: int and duration coerce to float,
// non-numeric statics coerce to 0.
func TestStaticFloat(t *testing.T) {
	t.Parallel()
	if got := NewStaticInt(7).Float(); got != 7 {
		t.Errorf("Int.Float() = %v; want 7", got)
	}
	if got := NewStaticFloat(2.5).Float(); got != 2.5 {
		t.Errorf("Float.Float() = %v; want 2.5", got)
	}
	if got := NewStaticDuration(3 * time.Nanosecond).Float(); got != 3 {
		t.Errorf("Duration.Float() = %v; want 3", got)
	}
	if got := NewStaticString("x").Float(); got != 0 {
		t.Errorf("String.Float() = %v; want 0", got)
	}
}

// TestStaticIsNil pins the nil predicate.
func TestStaticIsNil(t *testing.T) {
	t.Parallel()
	if !NewStaticNil().IsNil() {
		t.Error("NewStaticNil().IsNil() = false; want true")
	}
	if NewStaticInt(0).IsNil() {
		t.Error("NewStaticInt(0).IsNil() = true; want false")
	}
}

// TestStaticEquals pins the value-equality rules: cross-numeric comparison,
// int↔status/kind comparison, nil never equal, and per-type fallbacks.
func TestStaticEquals(t *testing.T) {
	t.Parallel()
	eq := func(a, b Static, want bool) {
		t.Helper()
		if got := a.Equals(&b); got != want {
			t.Errorf("%s.Equals(%s) = %v; want %v", a.String(), b.String(), got, want)
		}
	}
	// Cross-numeric.
	eq(NewStaticInt(5), NewStaticFloat(5), true)
	eq(NewStaticInt(5), NewStaticFloat(6), false)
	eq(NewStaticDuration(5), NewStaticInt(5), true)
	// int ↔ status / kind.
	eq(NewStaticInt(int(StatusOk)), NewStaticStatus(StatusOk), true)
	eq(NewStaticStatus(StatusOk), NewStaticInt(int(StatusOk)), true)
	eq(NewStaticInt(int(KindClient)), NewStaticKind(KindClient), true)
	// nil never equal.
	eq(NewStaticNil(), NewStaticNil(), false)
	eq(NewStaticNil(), NewStaticInt(0), false)
	// strings / bools / arrays.
	eq(NewStaticString("a"), NewStaticString("a"), true)
	eq(NewStaticString("a"), NewStaticString("b"), false)
	eq(NewStaticBool(true), NewStaticBool(true), true)
	eq(NewStaticIntArray([]int{1, 2}), NewStaticIntArray([]int{1, 2}), true)
	eq(NewStaticIntArray([]int{1, 2}), NewStaticIntArray([]int{1, 3}), false)
	eq(NewStaticStringArray([]string{"a"}), NewStaticStringArray([]string{"a"}), true)
	// type mismatch.
	eq(NewStaticString("a"), NewStaticInt(1), false)
	// A numeric vs non-numeric pair must NOT take the numeric branch: int 0 is
	// not equal to a string (whose Float() is 0). Turning the
	// `isNumeric() && isNumeric()` guard into `||` would coerce-compare them
	// and wrongly report equal.
	eq(NewStaticInt(0), NewStaticString("x"), false)
	eq(NewStaticString("x"), NewStaticInt(0), false)
}

// TestStaticElements pins array iteration and the scalar-yields-self-once rule.
func TestStaticElements(t *testing.T) {
	t.Parallel()
	var got []int
	for i, e := range NewStaticIntArray([]int{4, 5, 6}).Elements() {
		v, _ := e.Int()
		if v != []int{4, 5, 6}[i] {
			t.Errorf("Elements[%d] = %d", i, v)
		}
		got = append(got, v)
	}
	if len(got) != 3 {
		t.Errorf("array yielded %d elements; want 3", len(got))
	}

	count := 0
	for i, e := range NewStaticInt(9).Elements() {
		if i != 0 {
			t.Errorf("scalar yielded index %d; want 0", i)
		}
		if v, _ := e.Int(); v != 9 {
			t.Errorf("scalar element = %d; want 9", v)
		}
		count++
	}
	if count != 1 {
		t.Errorf("scalar yielded %d times; want 1", count)
	}
}

// TestStaticEncodeToString pins the surface rendering of every static type,
// including the float `.0` suffixing and the quoted/unquoted string forms.
func TestStaticEncodeToString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    Static
		want string
	}{
		{NewStaticNil(), "nil"},
		{NewStaticInt(42), "42"},
		{NewStaticInt(-7), "-7"},
		{NewStaticFloat(2.5), "2.5"},
		{NewStaticFloat(3), "3.0"},
		{NewStaticBool(true), "true"},
		{NewStaticBool(false), "false"},
		{NewStaticDuration(90 * time.Second), "1m30s"},
		{NewStaticStatus(StatusError), "error"},
		{NewStaticKind(KindServer), "server"},
		{NewStaticIntArray([]int{1, 2, 3}), "[1, 2, 3]"},
		{NewStaticFloatArray([]float64{1, 2.5}), "[1.0, 2.5]"},
		{NewStaticStringArray([]string{"a", "b"}), "[`a`, `b`]"},
		{NewStaticBooleanArray([]bool{true, false}), "[true, false]"},
	}
	for _, c := range cases {
		if got := c.s.EncodeToString(true); got != c.want {
			t.Errorf("EncodeToString(true) = %q; want %q", got, c.want)
		}
	}
	// Quoted vs unquoted string.
	if got := NewStaticString("hi").EncodeToString(true); got != "`hi`" {
		t.Errorf("quoted string = %q; want `hi`", got)
	}
	if got := NewStaticString("hi").EncodeToString(false); got != "hi" {
		t.Errorf("unquoted string = %q; want hi", got)
	}
}

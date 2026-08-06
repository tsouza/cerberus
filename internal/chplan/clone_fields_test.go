package chplan_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestCloneNodeCarriesEveryField is the class guard behind CloneNode: for
// every Node kind, EVERY exported field is filled with a distinguishable
// non-zero value and the clone is compared to the original field by field.
//
// It exists because the shape it catches is silent. `Aggregate.Having` was
// added to the struct but not to CloneNode's copy, whose arm was a composite
// literal enumerating the fields it happened to know about. The clone still
// type-checked, still executed, and merely lost the guard the field carried —
// so the sharded (route B) plan answered a query the single-shot plan
// correctly refused. Nothing failed; the wrong answer just shipped.
//
// TestCloneNodeExhaustive cannot cover this on its own: it compares fixtures
// through Node.Equal, so a field left at its zero value in the fixture is
// equal before and after the drop. Filling by reflection removes the fixture
// from the loop entirely — a newly added field is covered the moment it is
// declared, with no test edit.
func TestCloneNodeCarriesEveryField(t *testing.T) {
	t.Parallel()

	for _, kind := range allNodeKinds() {
		rt := reflect.TypeOf(kind).Elem()
		t.Run(rt.Name(), func(t *testing.T) {
			t.Parallel()

			filled := reflect.New(rt)
			fillStruct(t, filled.Elem(), rt.Name())
			orig, ok := filled.Interface().(chplan.Node)
			if !ok {
				t.Fatalf("%s: filled value is not a chplan.Node", rt.Name())
			}

			clone := reflect.ValueOf(chplan.CloneNode(orig))
			if clone.Type() != filled.Type() {
				t.Fatalf("CloneNode(%s) returned %s", rt.Name(), clone.Type())
			}
			assertFieldsCarried(t, filled.Elem(), clone.Elem(), rt)
		})
	}
}

// assertFieldsCarried compares every exported field of a filled node against
// the same field on its clone. reflect.DeepEqual follows pointers, so a
// deep-copied child compares equal to the child it was copied from — the only
// way a field can differ here is if CloneNode failed to carry it.
func assertFieldsCarried(t *testing.T, orig, clone reflect.Value, rt reflect.Type) {
	t.Helper()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if !reflect.DeepEqual(orig.Field(i).Interface(), clone.Field(i).Interface()) {
			t.Errorf("CloneNode(%s) dropped or altered field %s: got %#v, want %#v — "+
				"the arm in clone.go must copy it (start from `c := *v`)",
				rt.Name(), f.Name, clone.Field(i).Interface(), orig.Field(i).Interface())
		}
	}
}

// Sentinel values are chosen only to be non-zero and self-identifying in a
// failure message; nothing reads them for meaning.
const (
	fillString = "clone-field-sentinel"
	fillInt    = 7
	fillFloat  = 0.5
)

var (
	fillTime  = time.Unix(1_234_567_890, 0).UTC()
	exprIface = reflect.TypeOf((*chplan.Expr)(nil)).Elem()
	nodeIface = reflect.TypeOf((*chplan.Node)(nil)).Elem()
	timeType  = reflect.TypeOf(time.Time{})
)

// fillStruct fills every settable exported field of a struct value.
func fillStruct(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	rt := v.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		fillValue(t, v.Field(i), path+"."+f.Name)
	}
}

// fillValue writes a non-zero value into v, recursing through slices, maps,
// pointers and nested structs. The Expr / Node interface slots take concrete
// leaves — a ColumnRef and a Scan — so the walk terminates.
//
// An unhandled kind fails the test rather than being skipped: a silently
// skipped field is exactly the coverage hole this test exists to close.
func fillValue(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Type() {
	case timeType:
		v.Set(reflect.ValueOf(fillTime))
		return
	case exprIface:
		v.Set(reflect.ValueOf(chplan.Expr(&chplan.ColumnRef{Name: fillString})))
		return
	case nodeIface:
		v.Set(reflect.ValueOf(chplan.Node(&chplan.Scan{Table: fillString})))
		return
	}
	fillByKind(t, v, path)
}

func fillByKind(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString(fillString)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(fillInt)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(fillInt)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(fillFloat)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillValue(t, s.Index(0), path+"[0]")
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fillValue(t, key, path+".key")
		val := reflect.New(v.Type().Elem()).Elem()
		fillValue(t, val, path+".value")
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillValue(t, p.Elem(), path+".*")
		v.Set(p)
	case reflect.Struct:
		fillStruct(t, v, path)
	default:
		t.Fatalf("fillValue: unsupported kind %s at %s — extend the filler", v.Kind(), path)
	}
}

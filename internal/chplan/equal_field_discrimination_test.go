package chplan

import (
	"reflect"
	"testing"
	"time"
)

// This file pins the PER-FIELD DISCRIMINATION contract of every Node's
// Equal method: two nodes of the same type that differ in exactly one
// field must not compare Equal, in both directions.
//
// Why it is generic rather than a per-node list. Equal is a long
// conjunction of field comparisons, and a hand-written negative case per
// node reliably covers the fields someone thought of. The ones nobody
// thought of are invisible: a field left out of the conjunction, or a
// `&&` that should have been `||`, changes nothing a fixture can see —
// the plan still emits, the optimizer still runs, and two structurally
// DIFFERENT plans quietly compare equal. That is a wrong-answer class
// (the fixpoint driver stops early, a rule's "did this change anything?"
// check answers no), not a crash, so it ships silently.
//
// Driving the check off [allNodeCases] — the same registry
// TestRewriteChildren_TableCoversEveryNodeType count-guards — makes the
// contract total by construction: a new Node type has to be added to that
// table, and every field it declares is discriminated here from the
// moment it exists. There is no per-field opt-in to forget.
//
// Field values are synthesised by [synthValue], which produces two
// distinct values for every type the IR's node structs use. A field whose
// type it cannot synthesise fails the test rather than being skipped, so
// widening the IR's type vocabulary is a visible event.

// discriminationVariants is how many distinct values [synthValue]
// produces per type: a baseline and one that must be observably different
// from it.
const discriminationVariants = 2

// The per-kind value pairs. Index 0 builds the baseline node; index 1 is
// the single differing field under test. The values carry no meaning
// beyond "these two are not equal".
var (
	stringVariants = [discriminationVariants]string{"discriminantA", "discriminantB"}
	intVariants    = [discriminationVariants]int64{1, 2}
	uintVariants   = [discriminationVariants]uint64{1, 2}
	floatVariants  = [discriminationVariants]float64{1.5, 2.5}
	boolVariants   = [discriminationVariants]bool{false, true}
	timeVariants   = [discriminationVariants]time.Time{
		time.Unix(1_700_000_000, 0).UTC(),
		time.Unix(1_700_000_060, 0).UTC(),
	}
)

var (
	nodeInterface = reflect.TypeOf((*Node)(nil)).Elem()
	exprInterface = reflect.TypeOf((*Expr)(nil)).Elem()
	timeType      = reflect.TypeOf(time.Time{})
)

// synthValue returns a value of typ for the given variant. Two calls with
// different variants must produce values that any correct Equal reports as
// different; two calls with the same variant must produce equal values.
func synthValue(t *testing.T, typ reflect.Type, variant int) reflect.Value {
	t.Helper()

	if typ == timeType {
		return reflect.ValueOf(timeVariants[variant])
	}
	if typ == nodeInterface {
		return reflect.ValueOf(Node(&Scan{Table: stringVariants[variant]}))
	}
	if typ == exprInterface {
		return reflect.ValueOf(Expr(&ColumnRef{Name: stringVariants[variant]}))
	}

	v := reflect.New(typ).Elem()
	switch typ.Kind() {
	case reflect.String:
		v.SetString(stringVariants[variant])
	case reflect.Bool:
		v.SetBool(boolVariants[variant])
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(intVariants[variant])
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uintVariants[variant])
	case reflect.Float32, reflect.Float64:
		v.SetFloat(floatVariants[variant])
	case reflect.Slice:
		return synthSlice(t, typ, variant, 1)
	case reflect.Pointer:
		p := reflect.New(typ.Elem())
		p.Elem().Set(synthValue(t, typ.Elem(), variant))
		return p
	case reflect.Map:
		m := reflect.MakeMap(typ)
		m.SetMapIndex(synthValue(t, typ.Key(), variant), synthValue(t, typ.Elem(), variant))
		return m
	case reflect.Struct:
		for i := range typ.NumField() {
			f := typ.Field(i)
			if !f.IsExported() {
				t.Fatalf("field %s.%s is unexported, so no value can be synthesised for it; "+
					"give the type an exported shape or teach synthValue about it", typ, f.Name)
			}
			v.Field(i).Set(synthValue(t, f.Type, variant))
		}
	default:
		t.Fatalf("synthValue has no case for %s (kind %s); teach it one rather than "+
			"leaving the field undiscriminated", typ, typ.Kind())
	}
	return v
}

// synthSlice builds a length-n slice of typ's element type. Element i takes
// variant (variant+i) so a longer slice is also element-wise different from
// the baseline, which is what makes the length case fail for the right
// reason when a length check is missing.
func synthSlice(t *testing.T, typ reflect.Type, variant, n int) reflect.Value {
	t.Helper()
	s := reflect.MakeSlice(typ, n, n)
	for i := range n {
		s.Index(i).Set(synthValue(t, typ.Elem(), (variant+i)%discriminationVariants))
	}
	return s
}

// synthNode builds a *T (typ is the struct type) with every field set to
// the given variant's value.
func synthNode(t *testing.T, typ reflect.Type, variant int) Node {
	t.Helper()
	p := reflect.New(typ)
	p.Elem().Set(synthValue(t, typ, variant))
	n, ok := p.Interface().(Node)
	if !ok {
		t.Fatalf("*%s does not implement Node", typ)
	}
	return n
}

func TestNodeEqual_DiscriminatesEveryField(t *testing.T) {
	t.Parallel()

	for _, c := range allNodeCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			typ := reflect.TypeOf(c.node).Elem()
			base := synthNode(t, typ, 0)
			if twin := synthNode(t, typ, 0); !base.Equal(twin) {
				t.Fatalf("two independently built, field-identical %s nodes must be Equal; "+
					"an Equal that answers false here makes every discrimination case below vacuous", typ)
			}

			for i := range typ.NumField() {
				field := typ.Field(i)
				t.Run(field.Name, func(t *testing.T) {
					assertDiscriminates(t, base, typ, i, synthValue(t, field.Type, 1), "value")

					// A slice field also has to be discriminated by LENGTH:
					// an Equal that compares elements pairwise but forgets
					// the length check reports a prefix as equal to the
					// whole.
					if field.Type.Kind() == reflect.Slice {
						longer := synthSlice(t, field.Type, 0, discriminationVariants)
						assertDiscriminates(t, base, typ, i, longer, "length")
					}
				})
			}
		})
	}
}

// assertDiscriminates builds a copy of base with exactly field i replaced by
// value and asserts Equal reports the two as different, symmetrically.
func assertDiscriminates(t *testing.T, base Node, typ reflect.Type, i int, value reflect.Value, why string) {
	t.Helper()

	mutated := synthNode(t, typ, 0)
	reflect.ValueOf(mutated).Elem().Field(i).Set(value)

	name := typ.Field(i).Name
	if base.Equal(mutated) {
		t.Errorf("%s.Equal ignores the %s of field %s: nodes differing only there compare Equal, "+
			"so the optimizer treats two different plans as one", typ, why, name)
	}
	if mutated.Equal(base) {
		t.Errorf("%s.Equal is not symmetric for the %s of field %s: the reversed comparison reports Equal",
			typ, why, name)
	}
}

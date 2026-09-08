package chplan_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// reanchorGridFields names the fields ReanchorRange is SUPPOSED to rewrite.
// Everything else on every node kind must survive re-anchoring untouched, so
// this set is the whole exemption list for the field-carrying guard below.
//
// Start / End / Step are the eval grid itself; OuterRange, Range, Lookback
// and Offset are the window geometry the widening arithmetic derives from the
// new grid; Anchor is the instant-shape single point. A field added to a node
// that legitimately moves with the grid has to be named here explicitly,
// which is the point: the addition becomes a decision rather than a silent
// omission.
var reanchorGridFields = map[string]bool{
	"Start":      true,
	"End":        true,
	"Step":       true,
	"Anchor":     true,
	"OuterRange": true,
	"Range":      true,
	"Lookback":   true,
	"Offset":     true,
	"Input":      true, // rebuilt by recursion; its own fields are checked when that child is the subject.
	"Inputs":     true,
	"LHS":        true,
	"RHS":        true,
}

// TestReanchorRangeCarriesEveryNonGridField is the class guard behind
// ReanchorRange, mirroring TestCloneNodeCarriesEveryField for the other
// tree-rebuilding walk in this package.
//
// It exists because the failure is silent and has already happened twice in
// the same switch. `Project.Replacements` was dropped by a composite-literal
// arm, so a sharded (route B) classic-histogram plan lost its `le` and
// finite-bounds restrictions and evaluated the quantile over the unrestricted
// bucket ladder — a different answer from route A, with nothing failing.
// `Filter.Histogram` / `Filter.Mixed` were dropped the same way, so RowShapeOf
// read a re-anchored histogram-valued Filter as plain float rows. clone.go's
// doc states the rule both arms broke: start from `c := *v`, never from a
// literal enumerating the fields the author happened to know about.
//
// Filling by reflection is what makes this a class guard rather than a
// fixture: a newly added field is covered the moment it is declared, with no
// test edit, and a new arm written as a literal fails here immediately.
func TestReanchorRangeCarriesEveryNonGridField(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC)

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

			got, err := chplan.ReanchorRange(orig, start, end)
			if err != nil {
				// A kind this walk refuses to re-anchor is a legitimate
				// outcome (it is not slice-invariant); there is simply no
				// rebuilt node whose fields could have been dropped.
				return
			}
			rv := reflect.ValueOf(got)
			if rv.Type() != filled.Type() {
				// Off the windowed spine ReanchorRange shares the original
				// subtree verbatim, and a kind may legitimately re-anchor
				// into a different shape. Neither can drop a field.
				return
			}
			if rv.Pointer() == filled.Pointer() {
				// Shared verbatim: nothing was rebuilt, nothing can be lost.
				return
			}

			assertNonGridFieldsCarried(t, filled.Elem(), rv.Elem(), rt)
		})
	}
}

// assertNonGridFieldsCarried compares every exported field that ReanchorRange
// must not touch. reflect.DeepEqual follows pointers, so a shared child
// compares equal to the child it was shared from — the only way a field can
// differ here is if the arm failed to carry it.
func assertNonGridFieldsCarried(t *testing.T, orig, got reflect.Value, rt reflect.Type) {
	t.Helper()

	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() || reanchorGridFields[f.Name] {
			continue
		}
		o := orig.Field(i)
		g := got.Field(i)
		if reflect.DeepEqual(o.Interface(), g.Interface()) {
			continue
		}
		t.Errorf(
			"ReanchorRange(%s) dropped or altered non-grid field %s: original %#v, re-anchored %#v",
			rt.Name(), f.Name, o.Interface(), g.Interface(),
		)
	}
}

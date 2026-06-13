package chplan_test

import (
	"reflect"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestIsSliceInvariant_RegisteredKinds asserts exactly the phase-1 node
// kinds are registered slice-invariant, and that the registry is driven by
// node kind (not instance state).
func TestIsSliceInvariant_RegisteredKinds(t *testing.T) {
	t.Parallel()

	registered := []chplan.Node{
		&chplan.Scan{},
		&chplan.Filter{},
		&chplan.Project{},
		&chplan.Aggregate{},
		&chplan.RangeWindow{},
		&chplan.RangeLWR{},
		&chplan.RangeBucketFanout{},
		&chplan.StepGrid{},
		&chplan.UnionAll{},
	}
	for _, n := range registered {
		if !chplan.IsSliceInvariant(n) {
			t.Errorf("%T should be registered slice-invariant", n)
		}
	}
}

// TestIsSliceInvariant_UnregisteredKinds asserts every node kind NOT in the
// phase-1 set returns false — the default-deny posture. This list is the
// complement of the registered set over allNodeKinds (defined in
// clone_test.go); together they cover all 26 node kinds, so adding a node
// type without a deliberate registry decision fails the count guard below.
func TestIsSliceInvariant_UnregisteredKinds(t *testing.T) {
	t.Parallel()

	registered := map[reflect.Type]bool{
		reflect.TypeOf(&chplan.Scan{}):              true,
		reflect.TypeOf(&chplan.Filter{}):            true,
		reflect.TypeOf(&chplan.Project{}):           true,
		reflect.TypeOf(&chplan.Aggregate{}):         true,
		reflect.TypeOf(&chplan.RangeWindow{}):       true,
		reflect.TypeOf(&chplan.RangeLWR{}):          true,
		reflect.TypeOf(&chplan.RangeBucketFanout{}): true,
		reflect.TypeOf(&chplan.StepGrid{}):          true,
		reflect.TypeOf(&chplan.UnionAll{}):          true,
	}

	var unregisteredSeen int
	for _, n := range allNodeKinds() {
		want := registered[reflect.TypeOf(n)]
		got := chplan.IsSliceInvariant(n)
		if got != want {
			t.Errorf("IsSliceInvariant(%T) = %v, want %v", n, got, want)
		}
		if !want {
			unregisteredSeen++
		}
	}

	// 27 total node kinds, 9 registered → 18 must be default-denied. If this
	// drifts, a node kind was added: decide explicitly whether it is
	// slice-invariant (extend sliceInvariantKinds + the registered set here)
	// or not (it falls into the default-deny count).
	//
	// NaryVectorSetOp is deliberately default-denied: like VectorSetOp it is
	// a set-op family node, absent from sliceInvariantKinds until its own
	// slice-invariance proof + §Parity lanes land.
	const wantUnregistered = 27 - 9
	if unregisteredSeen != wantUnregistered {
		t.Fatalf("expected %d default-denied node kinds, saw %d — a node kind was added; "+
			"make an explicit slice-invariance decision", wantUnregistered, unregisteredSeen)
	}
}

// TestIsSliceInvariant_Nil returns false for a nil node.
func TestIsSliceInvariant_Nil(t *testing.T) {
	t.Parallel()
	if chplan.IsSliceInvariant(nil) {
		t.Fatal("IsSliceInvariant(nil) should be false")
	}
}

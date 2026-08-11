package chplan_test

import (
	"reflect"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// sliceInvariantRegisteredKinds is the phase-1 registered set: the node
// kinds IsSliceInvariant answers true for. It is a policy declaration,
// not a restatement of anything derivable — which kinds have a proof of
// slice-invariance is a decision, and the default-deny complement below
// is computed from it rather than written down a second time.
func sliceInvariantRegisteredKinds() []chplan.Node {
	return []chplan.Node{
		&chplan.Scan{},
		&chplan.Filter{},
		&chplan.Project{},
		&chplan.Aggregate{},
		&chplan.RangeWindow{},
		&chplan.RangeLWR{},
		&chplan.RangeBucketFanout{},
		&chplan.StepGrid{},
		&chplan.UnionAll{},
		&chplan.VectorJoin{},
	}
}

// TestIsSliceInvariant_RegisteredKinds asserts exactly the phase-1 node
// kinds are registered slice-invariant, and that the registry is driven by
// node kind (not instance state).
func TestIsSliceInvariant_RegisteredKinds(t *testing.T) {
	t.Parallel()

	for _, n := range sliceInvariantRegisteredKinds() {
		if !chplan.IsSliceInvariant(n) {
			t.Errorf("%T should be registered slice-invariant", n)
		}
	}
}

// TestIsSliceInvariant_UnregisteredKinds asserts every node kind NOT in the
// phase-1 set returns false — the default-deny posture. The unregistered
// set is the complement of sliceInvariantRegisteredKinds over
// allNodeKinds (defined in clone_test.go), so adding a node type without
// a deliberate registry decision fails the guard below.
func TestIsSliceInvariant_UnregisteredKinds(t *testing.T) {
	t.Parallel()

	registered := map[reflect.Type]bool{}
	for _, n := range sliceInvariantRegisteredKinds() {
		registered[reflect.TypeOf(n)] = true
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

	// Every node kind the registry does not name must be default-denied.
	// The expected number is DERIVED — the live planNode() implementer
	// set minus the registered set — rather than pinned, because a
	// pinned total is a second copy of the Node set that drifts silently
	// the moment a node kind lands. If this fails, a node kind was
	// added: decide explicitly whether it is slice-invariant (extend
	// sliceInvariantKinds + sliceInvariantRegisteredKinds) or not (it
	// falls into the default-deny count).
	//
	// VectorJoin is now REGISTERED (the step-aligned vector-vector join —
	// each output row reduces one anchor's window on each arm because the
	// anchor timestamp is in the join key). Its instant-mode shape is held on
	// route A not by this registry but by the solver's sawInstantVectorJoin
	// fail-closed guard. VectorSetOp / NaryVectorSetOp are deliberately
	// default-denied: set-op family nodes (and/or/unless), absent from
	// sliceInvariantKinds until their own slice-invariance proof + §Parity
	// lanes land. RangeWindowNative is also default-denied: the experimental
	// native-rate node is never routed by the solver (ReanchorRange does not
	// re-grid it), so it fails closed to route A — exactly the safe default for
	// an opt-in node. InfoJoin is a join-family node whose Info arm is a
	// point-in-time label lookup, not a sliced row stream, so it stays
	// default-denied. RangeWindowResample (a re-gridding range node) and
	// SearchTraceLimit (a per-trace cap) are likewise default-denied — neither
	// is a simple sliced row stream. HistogramProjection is default-denied
	// too: like HistogramQuantileNative, it aggregates bucket columns per
	// group from its input rather than passing a sliced row stream through
	// unchanged, and no lowering builds it yet regardless.
	wantUnregistered := len(planNodeImplementers(t)) - len(registered)
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

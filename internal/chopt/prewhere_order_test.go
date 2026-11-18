package chopt

import "testing"

// TestPrewhereKeepsWhereOrder pins the 24.9 / 24.10 boundary: from 24.10,
// which reorders the conditions it moves into PREWHERE
// (allow_reorder_prewhere_conditions), a build no longer keeps WHERE order.
func TestPrewhereKeepsWhereOrder(t *testing.T) {
	for _, tc := range []struct {
		server Version
		want   bool
	}{
		{Version{Major: 24, Minor: 8}, true},
		{Version{Major: 24, Minor: 8, Patch: 14, Build: 39}, true},
		{Version{Major: 24, Minor: 9, Patch: 3}, true},
		{Version{Major: 24, Minor: 10}, false},
		{Version{Major: 25, Minor: 3, Patch: 14, Build: 14}, false},
		{Version{Major: 26, Minor: 8, Patch: 36, Build: 3}, false},
	} {
		if got := PrewhereKeepsWhereOrder(tc.server); got != tc.want {
			t.Errorf("PrewhereKeepsWhereOrder(%s) = %v; want %v", tc.server, got, tc.want)
		}
	}
}

// TestResolve_LeavesPrewhereInWhereOrderUnset pins that Resolve never sets
// PrewhereInWhereOrder, even against a build that keeps WHERE order: only the
// caller that read the whole fleet does (WithPrewhereInWhereOrder). It also
// pins that Equal sees the fact.
func TestResolve_LeavesPrewhereInWhereOrderUnset(t *testing.T) {
	set, _, err := Resolve(Config{Optimizations: SelectionAuto}, Version{Major: 24, Minor: 8})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if set.PrewhereInWhereOrder() {
		t.Fatal("Resolve set PrewhereInWhereOrder from one build")
	}
	inOrder := set.WithPrewhereInWhereOrder(true)
	if !inOrder.PrewhereInWhereOrder() || set.PrewhereInWhereOrder() {
		t.Fatal("WithPrewhereInWhereOrder did not return a copy carrying the fact")
	}
	if inOrder.Equal(set) {
		t.Error("Equal reports sets that differ only in PrewhereInWhereOrder as equal")
	}
}

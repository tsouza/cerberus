package chplan

import (
	"reflect"
	"testing"
)

// TestInspectExprExhaustive guards that InspectExpr's type switch covers
// every concrete Expr implementation. If a new Expr type is added without
// extending inspectExpr's switch, this test fails — forcing the author to
// decide how the new node's children participate in traversal (a missing
// case would otherwise silently hide a ScalarSubquery nested inside it,
// blinding the fan-out linter).
//
// The set under test is allExprKinds(), whose completeness against the
// exprNode() implementer set is asserted below rather than pinned as a
// number. `go vet` cannot flag the missing inspectExpr case — the switch
// has no default-panic — so this derivation is the only thing standing
// between a new Expr type and a silently unvisited subtree.
func TestInspectExprExhaustive(t *testing.T) {
	t.Parallel()

	all := allExprKinds()

	// Each listed type must be reached as a root visit without panicking,
	// and the set must be free of accidental duplicates.
	seenType := map[reflect.Type]bool{}
	for _, e := range all {
		rt := reflect.TypeOf(e)
		if seenType[rt] {
			t.Errorf("duplicate Expr type in exhaustiveness set: %v", rt)
		}
		seenType[rt] = true

		var rooted bool
		InspectExpr(e, func(x Expr) bool {
			if reflect.TypeOf(x) == rt {
				rooted = true
			}
			return true
		})
		if !rooted {
			t.Errorf("InspectExpr did not visit root of %T", e)
		}
	}

	assertCoversEverySealedKind(t, exprMarkerMethod, sealedKindNames(all), "allExprKinds",
		"add a zero-value instance to allExprKinds and an inspectExpr case in walk_expr.go")
}

// TestCloneExprExhaustive guards cloneExpr's exhaustive switch (which has a
// default-panic) against a new Expr type silently aliasing into a re-anchored
// shard plan. Every concrete Expr type must clone without panicking and the
// clone must be Equal to the original. It shares allExprKinds() with
// TestInspectExprExhaustive — one list, derived against the exprNode()
// implementers — so a new type forces the author to extend BOTH switches
// (inspectExpr AND cloneExpr).
func TestCloneExprExhaustive(t *testing.T) {
	t.Parallel()

	all := allExprKinds()

	assertCoversEverySealedKind(t, exprMarkerMethod, sealedKindNames(all), "allExprKinds",
		"add a zero-value instance to allExprKinds and a cloneExpr case in clone.go")

	// Zero-value Exprs (nil children) are fine for this check: we only assert
	// cloneExpr handles each concrete type (no default-panic) and returns the
	// same concrete type — NOT structural equality, which would deref the nil
	// children. Deep-copy isolation is covered by clone_test.go's Node tests.
	for _, e := range all {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("cloneExpr(%T) panicked: %v — extend cloneExpr's switch in clone.go", e, r)
				}
			}()
			got := cloneExpr(e)
			if reflect.TypeOf(got) != reflect.TypeOf(e) {
				t.Errorf("cloneExpr(%T) returned %T — type not preserved", e, got)
			}
		}()
	}
}

// TestInspectExprReachesScalarSubquery proves a ScalarSubquery nested
// inside a Binary/FuncCall chain is reached, and that InspectExprNodes
// surfaces its embedded plan Node.
func TestInspectExprReachesScalarSubquery(t *testing.T) {
	t.Parallel()

	inner := &Scan{Table: "t"}
	sub := &ScalarSubquery{Input: inner}
	expr := &Binary{
		Op:   OpGt,
		Left: &ColumnRef{Name: "Value"},
		Right: &FuncCall{
			Fn:   FnAbs,
			Args: []Expr{sub},
		},
	}

	var sawSub bool
	InspectExpr(expr, func(x Expr) bool {
		if x == Expr(sub) {
			sawSub = true
		}
		return true
	})
	if !sawSub {
		t.Fatal("InspectExpr did not reach the nested ScalarSubquery")
	}

	var sawNode bool
	InspectExprNodes(expr, func(Expr) bool { return true }, func(n Node) {
		if n == Node(inner) {
			sawNode = true
		}
	})
	if !sawNode {
		t.Fatal("InspectExprNodes did not surface the ScalarSubquery's embedded plan Node")
	}
}

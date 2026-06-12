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
// The set below must list one zero-value instance of every type that
// implements Expr. Discovery is manual-by-design: there is no registry of
// Expr types, so the test pins the closed set explicitly.
func TestInspectExprExhaustive(t *testing.T) {
	t.Parallel()

	all := []Expr{
		&ColumnRef{},
		&LitString{},
		&LitInt{},
		&LitFloat{},
		&LitBool{},
		&BareIdent{},
		&Binary{},
		&FuncCall{},
		&InList{},
		&FieldAccess{},
		&MapAccess{},
		&Subscript{},
		&LineContent{},
		&LabelJoin{},
		&LabelReplace{},
		&Lambda{},
		&MapWithoutKeys{},
		&MapWithoutEmptyValues{},
		&NestedArrayExists{},
		&ScalarSubquery{},
	}

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

	// The hardcoded count is the real guard: if a new Expr type is added,
	// `go vet` / compilation won't flag the missing inspectExpr case (the
	// switch has no default-panic), so this count forces the author to
	// revisit both the switch AND this list. Keep it in lock-step with the
	// number of exprNode() implementers under internal/chplan.
	const wantExprTypes = 20
	if len(all) != wantExprTypes {
		t.Fatalf("expected %d Expr types in the exhaustiveness set, listed %d — "+
			"a new Expr type was added: extend inspectExpr's switch in walk_expr.go AND this list",
			wantExprTypes, len(all))
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
			Name: "abs",
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

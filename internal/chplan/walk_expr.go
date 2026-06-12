package chplan

// InspectExpr visits e and every sub-expression reachable from it in
// depth-first pre-order. The visit function returns false to skip an
// expression's children. It mirrors [Walk] (which traverses Node trees)
// for the Expr side of the IR.
//
// Unlike [Walk], InspectExpr deliberately DOES descend into the embedded
// plan subtree of a [ScalarSubquery] via the exprNodes argument: callers
// that want to reach Nodes nested inside an Expr (e.g. a fan-out / cross-
// join linter that must see the subquery's plan) pass a non-nil
// nodeVisit; passing nil keeps the traversal Expr-only.
//
// The switch is exhaustive over every concrete Expr type. Adding a new
// Expr implementation without extending this switch is caught by
// TestInspectExprExhaustive in walk_expr_test.go.
func InspectExpr(e Expr, visit func(Expr) bool) {
	inspectExpr(e, visit, nil)
}

// InspectExprNodes is the Node-aware variant of [InspectExpr]: in
// addition to visiting every sub-expression, it invokes nodeVisit on the
// plan subtree embedded inside any [ScalarSubquery] it encounters, so a
// caller can reach plan Nodes that are only reachable through an Expr
// slot. nodeVisit receives the ScalarSubquery's Input Node; returning is
// the caller's responsibility (e.g. recurse with [Walk]).
func InspectExprNodes(e Expr, visit func(Expr) bool, nodeVisit func(Node)) {
	inspectExpr(e, visit, nodeVisit)
}

func inspectExpr(e Expr, visit func(Expr) bool, nodeVisit func(Node)) {
	if e == nil || !visit(e) {
		return
	}
	switch v := e.(type) {
	case *ColumnRef, *LitString, *LitInt, *LitFloat, *LitBool, *BareIdent:
		// Leaf expressions: no sub-expressions.
	case *Binary:
		inspectExpr(v.Left, visit, nodeVisit)
		inspectExpr(v.Right, visit, nodeVisit)
	case *FuncCall:
		for _, a := range v.Args {
			inspectExpr(a, visit, nodeVisit)
		}
	case *InList:
		inspectExpr(v.Left, visit, nodeVisit)
		for _, x := range v.List {
			inspectExpr(x, visit, nodeVisit)
		}
	case *FieldAccess:
		inspectExpr(v.Source, visit, nodeVisit)
	case *MapAccess:
		inspectExpr(v.Map, visit, nodeVisit)
		inspectExpr(v.Key, visit, nodeVisit)
	case *Subscript:
		inspectExpr(v.Container, visit, nodeVisit)
		inspectExpr(v.Key, visit, nodeVisit)
	case *LineContent:
		inspectExpr(v.Source, visit, nodeVisit)
	case *LabelJoin:
		inspectExpr(v.Map, visit, nodeVisit)
	case *LabelReplace:
		inspectExpr(v.Map, visit, nodeVisit)
	case *Lambda:
		inspectExpr(v.Body, visit, nodeVisit)
	case *MapWithoutKeys:
		inspectExpr(v.Map, visit, nodeVisit)
	case *MapWithoutEmptyValues:
		inspectExpr(v.Map, visit, nodeVisit)
	case *NestedArrayExists:
		inspectExpr(v.Value, visit, nodeVisit)
	case *ScalarSubquery:
		if nodeVisit != nil && v.Input != nil {
			nodeVisit(v.Input)
		}
	}
}

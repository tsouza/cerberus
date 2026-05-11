package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// ConstantFold simplifies the predicate of every Filter and the expressions
// inside every Project/Aggregate AggFunc by folding compile-time-known
// values: Binary over two Lits collapses, and the identity rules for AND/OR
// with bool literals apply (`true AND X` → `X`, `false OR X` → `X`, etc.).
//
// Folding runs recursively across each expression tree before the result is
// compared with the original — so a single Apply call can collapse nested
// constants in one shot.
type ConstantFold struct{}

func (ConstantFold) Name() string { return "constant-fold" }

func (ConstantFold) Apply(n chplan.Node) (chplan.Node, bool) {
	switch v := n.(type) {
	case *chplan.Filter:
		newPred, changed := foldExpr(v.Predicate)
		if !changed {
			return n, false
		}
		cp := *v
		cp.Predicate = newPred
		return &cp, true
	case *chplan.Project:
		anyChange := false
		newProjs := make([]chplan.Projection, len(v.Projections))
		for i, p := range v.Projections {
			ne, ch := foldExpr(p.Expr)
			newProjs[i] = chplan.Projection{Expr: ne, Alias: p.Alias}
			if ch {
				anyChange = true
			}
		}
		if !anyChange {
			return n, false
		}
		cp := *v
		cp.Projections = newProjs
		return &cp, true
	case *chplan.Aggregate:
		anyChange := false
		newGroup := make([]chplan.Expr, len(v.GroupBy))
		for i, g := range v.GroupBy {
			ne, ch := foldExpr(g)
			newGroup[i] = ne
			if ch {
				anyChange = true
			}
		}
		newFuncs := make([]chplan.AggFunc, len(v.AggFuncs))
		for i, af := range v.AggFuncs {
			newArgs := make([]chplan.Expr, len(af.Args))
			for j, a := range af.Args {
				ne, ch := foldExpr(a)
				newArgs[j] = ne
				if ch {
					anyChange = true
				}
			}
			newFuncs[i] = chplan.AggFunc{Name: af.Name, Args: newArgs, Alias: af.Alias}
		}
		if !anyChange {
			return n, false
		}
		cp := *v
		cp.GroupBy = newGroup
		cp.AggFuncs = newFuncs
		return &cp, true
	}
	return n, false
}

// foldExpr recursively folds e and reports whether the result differs.
func foldExpr(e chplan.Expr) (chplan.Expr, bool) {
	switch v := e.(type) {
	case *chplan.Binary:
		left, lc := foldExpr(v.Left)
		right, rc := foldExpr(v.Right)
		current := v
		if lc || rc {
			current = &chplan.Binary{Op: v.Op, Left: left, Right: right}
		}
		folded, fc := foldBinary(current)
		if fc {
			return folded, true
		}
		if lc || rc {
			return current, true
		}
		return v, false
	case *chplan.MapAccess:
		nm, mc := foldExpr(v.Map)
		nk, kc := foldExpr(v.Key)
		if !mc && !kc {
			return v, false
		}
		return &chplan.MapAccess{Map: nm, Key: nk}, true
	case *chplan.FuncCall:
		newArgs := make([]chplan.Expr, len(v.Args))
		anyChange := false
		for i, a := range v.Args {
			na, ch := foldExpr(a)
			newArgs[i] = na
			if ch {
				anyChange = true
			}
		}
		if !anyChange {
			return v, false
		}
		return &chplan.FuncCall{Name: v.Name, Args: newArgs}, true
	}
	return e, false
}

// foldBinary attempts a constant-fold on a single Binary node whose children
// are already folded. Returns the replacement and true iff a fold applied.
func foldBinary(b *chplan.Binary) (chplan.Expr, bool) {
	// Boolean identity rules (independent of literal types on the other side).
	if lit, ok := b.Left.(*chplan.LitBool); ok {
		if r, ok := identityForBool(b.Op, lit.V, b.Right); ok {
			return r, true
		}
	}
	if lit, ok := b.Right.(*chplan.LitBool); ok {
		if r, ok := identityForBool(b.Op, lit.V, b.Left); ok {
			return r, true
		}
	}

	// Pure numeric folds: both sides are int or both sides are float.
	if li, ok := b.Left.(*chplan.LitInt); ok {
		if ri, ok := b.Right.(*chplan.LitInt); ok {
			if r, ok := foldIntInt(b.Op, li.V, ri.V); ok {
				return r, true
			}
		}
	}
	if lf, ok := b.Left.(*chplan.LitFloat); ok {
		if rf, ok := b.Right.(*chplan.LitFloat); ok {
			if r, ok := foldFloatFloat(b.Op, lf.V, rf.V); ok {
				return r, true
			}
		}
	}
	return b, false
}

// identityForBool collapses Bool-vs-anything boolean ops:
//
//	true  AND X  → X        false AND X  → false
//	false OR  X  → X        true  OR  X  → true
//
// It returns (nil, false) for ops where the literal doesn't carry an
// algebraic identity.
func identityForBool(op chplan.BinaryOp, lit bool, other chplan.Expr) (chplan.Expr, bool) {
	switch op {
	case chplan.OpAnd:
		if lit {
			return other, true
		}
		return &chplan.LitBool{V: false}, true
	case chplan.OpOr:
		if lit {
			return &chplan.LitBool{V: true}, true
		}
		return other, true
	}
	return nil, false
}

func foldIntInt(op chplan.BinaryOp, l, r int64) (chplan.Expr, bool) {
	switch op {
	case chplan.OpAdd:
		return &chplan.LitInt{V: l + r}, true
	case chplan.OpSub:
		return &chplan.LitInt{V: l - r}, true
	case chplan.OpMul:
		return &chplan.LitInt{V: l * r}, true
	case chplan.OpDiv:
		if r == 0 {
			return nil, false
		}
		return &chplan.LitInt{V: l / r}, true
	case chplan.OpEq:
		return &chplan.LitBool{V: l == r}, true
	case chplan.OpNe:
		return &chplan.LitBool{V: l != r}, true
	case chplan.OpLt:
		return &chplan.LitBool{V: l < r}, true
	case chplan.OpLe:
		return &chplan.LitBool{V: l <= r}, true
	case chplan.OpGt:
		return &chplan.LitBool{V: l > r}, true
	case chplan.OpGe:
		return &chplan.LitBool{V: l >= r}, true
	}
	return nil, false
}

func foldFloatFloat(op chplan.BinaryOp, l, r float64) (chplan.Expr, bool) {
	switch op {
	case chplan.OpAdd:
		return &chplan.LitFloat{V: l + r}, true
	case chplan.OpSub:
		return &chplan.LitFloat{V: l - r}, true
	case chplan.OpMul:
		return &chplan.LitFloat{V: l * r}, true
	case chplan.OpDiv:
		if r == 0 {
			return nil, false
		}
		return &chplan.LitFloat{V: l / r}, true
	case chplan.OpEq:
		return &chplan.LitBool{V: l == r}, true
	case chplan.OpNe:
		return &chplan.LitBool{V: l != r}, true
	case chplan.OpLt:
		return &chplan.LitBool{V: l < r}, true
	case chplan.OpLe:
		return &chplan.LitBool{V: l <= r}, true
	case chplan.OpGt:
		return &chplan.LitBool{V: l > r}, true
	case chplan.OpGe:
		return &chplan.LitBool{V: l >= r}, true
	}
	return nil, false
}

package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// ConstantFoldSemantic reduces pure-literal Binary subtrees inside every
// Filter predicate and every Project/Aggregate expression: a Binary
// whose left and right are both LitInt (or both LitFloat) collapses to
// the computed Lit (`1+2 → 3`, `1=0 → false`).
//
// This is the **semantic / must-run** flavour of constant folding
// (DataFusion `AnalyzerRule` shape — see analyzer.go + § 4 of
// docs/optimizer-research.md). Downstream rules assume that
// pure-literal subtrees have already collapsed to a single Lit: a
// PREWHERE-promotion rule that distinguishes `WHERE false` from
// `WHERE 1=0` would silently miss the latter without this pass.
//
// Folding runs recursively across each expression tree before the
// result is compared with the original — so a single Apply call can
// collapse nested literal subtrees in one shot. Idempotent by
// construction: after one bottom-up sweep no further pure-literal
// Binary remains.
type ConstantFoldSemantic struct{}

// ConstantFoldSemantic satisfies AnalyzerRule.
func (ConstantFoldSemantic) Name() string   { return "constant-fold-semantic" }
func (ConstantFoldSemantic) isAnalyzerRule() {}

func (ConstantFoldSemantic) Apply(n chplan.Node) (chplan.Node, bool) {
	return foldNode(n, foldExprSemantic)
}

// ConstantFoldHeuristic applies boolean algebraic identities at every
// Binary in the same expression positions ConstantFoldSemantic
// inspects:
//
//	true  AND X → X        false AND X → false
//	false OR  X → X        true  OR  X → true
//
// This is the **heuristic / optional** flavour of constant folding —
// the identities make the emitted SQL cleaner but the result is
// correct either way. ClickHouse will short-circuit `true AND X` at
// execution time; we apply the identity at plan time to shrink the
// tree and unblock downstream rules (e.g. FilterFusion sees a single
// non-trivial predicate instead of a Binary{AND, LitBool(true), real}).
//
// Heuristic rules live in Once / FixedPoint batches (not Analyzer)
// because they are not part of the language contract: skipping them
// would still produce a correct plan, just a noisier one.
type ConstantFoldHeuristic struct{}

func (ConstantFoldHeuristic) Name() string { return "constant-fold-heuristic" }

func (ConstantFoldHeuristic) Apply(n chplan.Node) (chplan.Node, bool) {
	return foldNode(n, foldExprHeuristic)
}

// foldNode is the shared Node-level walker for the two constant-fold
// flavours. It visits Filter predicates, Project expressions, and
// Aggregate group-by / arg expressions, applying foldFn to each
// expression tree.
func foldNode(n chplan.Node, foldFn func(chplan.Expr) (chplan.Expr, bool)) (chplan.Node, bool) {
	switch v := n.(type) {
	case *chplan.Filter:
		newPred, changed := foldFn(v.Predicate)
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
			ne, ch := foldFn(p.Expr)
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
			ne, ch := foldFn(g)
			newGroup[i] = ne
			if ch {
				anyChange = true
			}
		}
		newFuncs := make([]chplan.AggFunc, len(v.AggFuncs))
		for i, af := range v.AggFuncs {
			newArgs := make([]chplan.Expr, len(af.Args))
			for j, a := range af.Args {
				ne, ch := foldFn(a)
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

// foldExprSemantic recursively reduces pure-literal Binary subtrees
// (both sides LitInt or both LitFloat) — the semantic flavour. It does
// NOT apply boolean identity rules; those are the heuristic's job.
func foldExprSemantic(e chplan.Expr) (chplan.Expr, bool) {
	switch v := e.(type) {
	case *chplan.Binary:
		left, lc := foldExprSemantic(v.Left)
		right, rc := foldExprSemantic(v.Right)
		current := v
		if lc || rc {
			current = &chplan.Binary{Op: v.Op, Left: left, Right: right}
		}
		folded, fc := foldBinaryLiterals(current)
		if fc {
			return folded, true
		}
		if lc || rc {
			return current, true
		}
		return v, false
	case *chplan.MapAccess:
		nm, mc := foldExprSemantic(v.Map)
		nk, kc := foldExprSemantic(v.Key)
		if !mc && !kc {
			return v, false
		}
		return &chplan.MapAccess{Map: nm, Key: nk}, true
	case *chplan.FuncCall:
		newArgs := make([]chplan.Expr, len(v.Args))
		anyChange := false
		for i, a := range v.Args {
			na, ch := foldExprSemantic(a)
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

// foldExprHeuristic recursively applies boolean algebraic identities
// at every Binary — the heuristic flavour. It does NOT touch
// pure-literal arithmetic; the semantic pass has already canonicalised
// those.
func foldExprHeuristic(e chplan.Expr) (chplan.Expr, bool) {
	switch v := e.(type) {
	case *chplan.Binary:
		left, lc := foldExprHeuristic(v.Left)
		right, rc := foldExprHeuristic(v.Right)
		current := v
		if lc || rc {
			current = &chplan.Binary{Op: v.Op, Left: left, Right: right}
		}
		folded, fc := foldBinaryBoolIdentity(current)
		if fc {
			return folded, true
		}
		if lc || rc {
			return current, true
		}
		return v, false
	case *chplan.MapAccess:
		nm, mc := foldExprHeuristic(v.Map)
		nk, kc := foldExprHeuristic(v.Key)
		if !mc && !kc {
			return v, false
		}
		return &chplan.MapAccess{Map: nm, Key: nk}, true
	case *chplan.FuncCall:
		newArgs := make([]chplan.Expr, len(v.Args))
		anyChange := false
		for i, a := range v.Args {
			na, ch := foldExprHeuristic(a)
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

// foldBinaryLiterals attempts a pure-literal numeric / comparison fold
// on a single Binary whose children are already folded. Returns the
// replacement and true iff the fold applied. Boolean identity rules
// live in foldBinaryBoolIdentity (the heuristic flavour).
func foldBinaryLiterals(b *chplan.Binary) (chplan.Expr, bool) {
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

// foldBinaryBoolIdentity attempts a boolean identity collapse on a
// single Binary:
//
//	true  AND X → X        false AND X → false
//	false OR  X → X        true  OR  X → true
//
// Returns the replacement and true iff an identity applied. Pure-
// literal numeric folding lives in foldBinaryLiterals (the semantic
// flavour).
func foldBinaryBoolIdentity(b *chplan.Binary) (chplan.Expr, bool) {
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

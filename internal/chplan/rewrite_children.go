package chplan

// This file hosts the single generic "recurse into a node's children and
// rebuild on change" tree operation. It lives in chplan (not optimizer) because
// it is a pure plan-tree rewrite that more than one layer needs: the optimizer's
// applyToTree drives every rule through it, and the TraceQL search-window fold
// (pushLeafPredicate) delegates to it so NO node type can silently swallow the
// window. TestRewriteChildren_Exhaustive is the lock-step guard.

// rewriteSingleInput is the shared clone-on-change shape for nodes
// with exactly one Input child: apply fn to input; when unchanged,
// hand back the original node, otherwise clone via `clone(newInput)`.
func rewriteSingleInput(
	orig Node,
	input Node,
	fn func(Node) (Node, bool),
	clone func(Node) Node,
) (Node, bool) {
	newInput, ch := fn(input)
	if !ch {
		return orig, false
	}
	return clone(newInput), true
}

// rewriteLeftRight is the shared clone-on-change shape for binary nodes
// with exactly two children (Left / Right): apply fn to each; when
// neither changed, hand back the original node, otherwise clone via
// `clone(newLeft, newRight)`.
func rewriteLeftRight(
	orig Node,
	left, right Node,
	fn func(Node) (Node, bool),
	clone func(newLeft, newRight Node) Node,
) (Node, bool) {
	newLeft, lch := fn(left)
	newRight, rch := fn(right)
	if !lch && !rch {
		return orig, false
	}
	return clone(newLeft, newRight), true
}

// RewriteChildren clones n with each child replaced by `fn(child)`. Returns
// the new (or same) node and whether any child changed.
//
// EXHAUSTIVENESS CONTRACT: every concrete Node implementation MUST
// be handled by exactly one of the category dispatchers below. A node that
// recurses into its `Children()` lets every optimizer rule (predicate
// pushdown, PREWHERE, projection-pushdown, MV, constant-fold) fire beneath
// it; a node that falls through to the final `return n, false` becomes an
// OPAQUE LEAF that silently DISABLES all rules on its subtree. The only
// legitimate no-recursion arms are genuine leaves whose `Children()` is
// always nil (Scan, OneRow, StepGrid), handled by rewriteLeafNode. The
// compile-time test TestRewriteChildren_Exhaustive enumerates every Node
// type and fails if a node with non-nil children is returned unchanged, so
// a future new node type can't silently reintroduce the gap.
//
// The dispatch is split into per-shape helpers (unary-Input, binary
// Left/Right, leaf, and the irregular shapes) purely to keep each function
// under the cyclomatic-complexity gate; the behaviour is a single flat
// "match the concrete type, recurse into its children" rule.
func RewriteChildren(n Node, fn func(Node) (Node, bool)) (Node, bool) {
	if out, changed, handled := rewriteLeafNode(n); handled {
		return out, changed
	}
	if out, changed, handled := rewriteUnaryNode(n, fn); handled {
		return out, changed
	}
	if out, changed, handled := rewriteBinaryNode(n, fn); handled {
		return out, changed
	}
	if out, changed, handled := rewriteIrregularNode(n, fn); handled {
		return out, changed
	}
	return n, false
}

// rewriteLeafNode handles the genuine leaves — nodes whose `Children()` is
// always nil. They are returned unchanged (recursing would be a no-op).
func rewriteLeafNode(n Node) (out Node, changed, handled bool) {
	switch v := n.(type) {
	case *Scan:
		return v, false, true
	case *OneRow:
		return v, false, true
	case *StepGrid:
		return v, false, true
	}
	return n, false, false
}

// rewriteUnaryNode handles every node with exactly one Node-typed child.
// Most carry it as `Input`; the Metrics* aggregation nodes carry it as
// `Inner`. Each rebuilds via the shared clone-on-change rewriteSingleInput
// shape.
func rewriteUnaryNode(n Node, fn func(Node) (Node, bool)) (out Node, changed, handled bool) {
	switch v := n.(type) {
	case *Filter:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *Project:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *NestedSetAnnotate:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *SearchTraceLimit:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *Aggregate:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *RangeWindow:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *RangeWindowNative:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *AbsentOverTime:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *Limit:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *OrderBy:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *RangeLWR:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *RangeWindowResample:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *RangeBucketFanout:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *HistogramQuantile:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *HistogramQuantileNative:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *MetricsAggregate:
		out, changed = rewriteSingleInput(v, v.Inner, fn, func(in Node) Node {
			cp := *v
			cp.Inner = in
			return &cp
		})
	case *MetricsHistogramOverTime:
		out, changed = rewriteSingleInput(v, v.Inner, fn, func(in Node) Node {
			cp := *v
			cp.Inner = in
			return &cp
		})
	case *MetricsSecondStage:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in Node) Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	default:
		return n, false, false
	}
	return out, changed, true
}

// rewriteBinaryNode handles every node with exactly two Node-typed
// children carried as `Left` / `Right`. Each rebuilds via the shared
// rewriteLeftRight shape.
func rewriteBinaryNode(n Node, fn func(Node) (Node, bool)) (out Node, changed, handled bool) {
	switch v := n.(type) {
	case *CrossJoin:
		out, changed = rewriteLeftRight(v, v.Left, v.Right, fn, func(l, r Node) Node {
			cp := *v
			cp.Left = l
			cp.Right = r
			return &cp
		})
	case *StructuralJoin:
		out, changed = rewriteLeftRight(v, v.Left, v.Right, fn, func(l, r Node) Node {
			cp := *v
			cp.Left = l
			cp.Right = r
			return &cp
		})
	case *VectorJoin:
		out, changed = rewriteLeftRight(v, v.Left, v.Right, fn, func(l, r Node) Node {
			cp := *v
			cp.Left = l
			cp.Right = r
			return &cp
		})
	case *VectorSetOp:
		out, changed = rewriteLeftRight(v, v.Left, v.Right, fn, func(l, r Node) Node {
			cp := *v
			cp.Left = l
			cp.Right = r
			return &cp
		})
	case *SetOperation:
		out, changed = rewriteLeftRight(v, v.Left, v.Right, fn, func(l, r Node) Node {
			cp := *v
			cp.Left = l
			cp.Right = r
			return &cp
		})
	default:
		return n, false, false
	}
	return out, changed, true
}

// rewriteIrregularNode handles the nodes whose child shape doesn't fit the
// unary / binary helpers: TopK (Input + optional KExpr), UnionAll
// (N inputs), and MetricsCompare (Inner + optional RootLookup).
func rewriteIrregularNode(n Node, fn func(Node) (Node, bool)) (out Node, changed, handled bool) {
	switch v := n.(type) {
	case *TopK:
		newInput, ch := fn(v.Input)
		var newKExpr Node
		kCh := false
		if v.KExpr != nil {
			newKExpr, kCh = fn(v.KExpr)
		}
		if !ch && !kCh {
			return v, false, true
		}
		cp := *v
		if ch {
			cp.Input = newInput
		}
		if kCh {
			cp.KExpr = newKExpr
		}
		return &cp, true, true
	case *UnionAll:
		// Recurse into each arm so existing optimizer rules
		// (constant-fold, PREWHERE promotion, etc.) can rewrite the
		// per-arm Project(Filter(Scan)) subtrees the PromQL
		// classic-histogram companion-suffix lowering emits.
		newInputs := make([]Node, len(v.Inputs))
		ch := false
		for i, in := range v.Inputs {
			newIn, c := fn(in)
			if c {
				ch = true
			}
			newInputs[i] = newIn
		}
		if !ch {
			return v, false, true
		}
		cp := *v
		cp.Inputs = newInputs
		return &cp, true, true
	case *NaryVectorSetOp:
		// Recurse into each arm so rules already applied to the binary
		// VectorSetOp's Left / Right children keep firing once the
		// flatten rule has linearised the chain into N arms.
		newArms := make([]Node, len(v.Arms))
		ch := false
		for i, arm := range v.Arms {
			newArm, c := fn(arm)
			if c {
				ch = true
			}
			newArms[i] = newArm
		}
		if !ch {
			return v, false, true
		}
		cp := *v
		cp.Arms = newArms
		return &cp, true, true
	case *InfoJoin:
		// Two always-present children carried as Input (the base vector)
		// + Info (the info-metric scan). Recurse into both so the per-
		// side LWR / PREWHERE-promotable Filter(Scan) subtrees stay
		// reachable to the optimizer. Field names differ from the
		// Left/Right binary shape, so this lands in the irregular arm.
		newInput, inCh := fn(v.Input)
		newInfo, infoCh := fn(v.Info)
		if !inCh && !infoCh {
			return v, false, true
		}
		cp := *v
		if inCh {
			cp.Input = newInput
		}
		if infoCh {
			cp.Info = newInfo
		}
		return &cp, true, true
	case *MetricsCompare:
		// Inner is the always-present child; RootLookup is an optional
		// second child (the service-graph root-name join side). Recurse
		// into both, rebuilding only when one changed.
		newInner, innerCh := fn(v.Inner)
		var newRootLookup Node
		rootCh := false
		if v.RootLookup != nil {
			newRootLookup, rootCh = fn(v.RootLookup)
		}
		if !innerCh && !rootCh {
			return v, false, true
		}
		cp := *v
		if innerCh {
			cp.Inner = newInner
		}
		if rootCh {
			cp.RootLookup = newRootLookup
		}
		return &cp, true, true
	}
	return n, false, false
}

// Package optimizer rewrites a chplan tree to an equivalent, cheaper one by
// running registered rules to a fixpoint.
//
// Each Rule implements `Apply(n) (n', changed bool)`. The Driver visits
// every node in the tree bottom-up, gives each rule a chance to rewrite,
// and re-runs the whole pass until no rule reports a change (or the
// iteration cap is reached).
//
// Rules ship in their own files (filter_fusion.go, constant_fold.go,
// projection_pushdown.go); the default rule set is wired in Default().
package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// Rule is one rewrite pass over the plan IR.
type Rule interface {
	// Name returns the rule's identifier (used in debug + test fixtures).
	Name() string
	// Apply rewrites n if a pattern matches and returns the new node + a
	// changed-flag. When no pattern matches it returns n unchanged.
	Apply(n chplan.Node) (chplan.Node, bool)
}

// Driver runs a set of Rules to a fixpoint over a chplan tree.
type Driver struct {
	rules         []Rule
	maxIterations int
}

// New builds a Driver with the supplied rule set, in the order they'll run
// during each iteration. The default iteration cap (100) is generous; rules
// that don't converge typically signal a bug rather than a configuration
// concern.
func New(rules ...Rule) *Driver {
	return &Driver{rules: rules, maxIterations: 100}
}

// Default returns a Driver configured with all the seed v0.1 rules in a
// sensible order. The order matters: constant folding may unlock filter
// fusion (e.g. by collapsing `true AND X` → `X`), which may unlock projection
// pushdown.
func Default() *Driver {
	return New(
		ConstantFold{},
		FilterFusion{},
		ProjectionPushdown{},
	)
}

// Run rewrites plan to a fixpoint, returning the optimized tree. Run never
// mutates plan; rules construct fresh nodes when they rewrite.
func (d *Driver) Run(plan chplan.Node) chplan.Node {
	for i := 0; i < d.maxIterations; i++ {
		var iterationChanged bool
		for _, rule := range d.rules {
			rewritten, changed := applyToTree(plan, rule)
			plan = rewritten
			iterationChanged = iterationChanged || changed
		}
		if !iterationChanged {
			return plan
		}
	}
	return plan
}

// applyToTree walks n bottom-up, applying rule at each node. Returns the
// rewritten tree plus whether any node changed.
func applyToTree(n chplan.Node, rule Rule) (chplan.Node, bool) {
	if n == nil {
		return nil, false
	}
	rewritten, childrenChanged := rewriteChildren(n, func(c chplan.Node) (chplan.Node, bool) {
		return applyToTree(c, rule)
	})
	result, here := rule.Apply(rewritten)
	return result, childrenChanged || here
}

// rewriteChildren clones n with each child replaced by `fn(child)`. Returns
// the new (or same) node and whether any child changed.
func rewriteChildren(n chplan.Node, fn func(chplan.Node) (chplan.Node, bool)) (chplan.Node, bool) {
	switch v := n.(type) {
	case *chplan.Scan:
		return v, false
	case *chplan.Filter:
		newInput, ch := fn(v.Input)
		if !ch {
			return v, false
		}
		cp := *v
		cp.Input = newInput
		return &cp, true
	case *chplan.Project:
		newInput, ch := fn(v.Input)
		if !ch {
			return v, false
		}
		cp := *v
		cp.Input = newInput
		return &cp, true
	case *chplan.Aggregate:
		newInput, ch := fn(v.Input)
		if !ch {
			return v, false
		}
		cp := *v
		cp.Input = newInput
		return &cp, true
	case *chplan.RangeWindow:
		newInput, ch := fn(v.Input)
		if !ch {
			return v, false
		}
		cp := *v
		cp.Input = newInput
		return &cp, true
	case *chplan.Limit:
		newInput, ch := fn(v.Input)
		if !ch {
			return v, false
		}
		cp := *v
		cp.Input = newInput
		return &cp, true
	case *chplan.OrderBy:
		newInput, ch := fn(v.Input)
		if !ch {
			return v, false
		}
		cp := *v
		cp.Input = newInput
		return &cp, true
	}
	return n, false
}

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
//
// Rules are grouped into Batches (Catalyst-style). Each Batch carries a
// Strategy (`Once` or `FixedPoint(n)`) that controls how its rules
// iterate. Batches run sequentially in the order Default() returns
// them; within a batch, rules run in declared order. See batch.go.
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

// defaultMaxIterations is the fixpoint cap used by Default()'s
// FixedPoint batches and by the New() back-compat wrapper. Generous;
// rules that don't converge typically signal a bug rather than a
// tuning concern.
const defaultMaxIterations = 100

// Driver runs a sequence of Batches over a chplan tree.
type Driver struct {
	batches []Batch
}

// New builds a Driver with the supplied rule set wrapped in a single
// `FixedPoint(100)` batch named "default". Preserved for back-compat
// with callers that pre-date Batch grouping; new code should call
// NewWithBatches.
func New(rules ...Rule) *Driver {
	return NewWithBatches(Batch{
		Name:     "default",
		Strategy: FixedPoint(defaultMaxIterations),
		Rules:    rules,
	})
}

// NewWithBatches builds a Driver that runs the given batches in order.
// Each batch iterates per its Strategy; later batches see the output of
// earlier ones.
func NewWithBatches(batches ...Batch) *Driver {
	return &Driver{batches: batches}
}

// Default returns a Driver configured with all the seed v0.1 rules
// grouped into Catalyst-style batches. The split:
//
//   - "constant-folding" (Once) — ConstantFold is idempotent; one pass
//     reaches its fixpoint by construction (folding is a single
//     bottom-up sweep that consumes every literal subtree it can).
//     Re-iterating wastes work.
//   - "predicate-pushdown" (FixedPoint) — FilterFusion + the two R3.2
//     transpose rules can unlock each other: fuse adjacent filters,
//     transpose the fused filter through Project / Aggregate, then
//     possibly fuse again as new neighbours appear. Iteration is
//     load-bearing here.
//   - "projection" (FixedPoint) — ProjectionPushdown may iterate as
//     pushdown unused-column elimination cascades through nested
//     Projects. Today only one pass changes anything, but the
//     strategy leaves room for follow-up rules (R3.4 / R3.7) to land
//     in this batch without changing wiring.
//
// Order matters across batches: constant folding feeds the predicate-
// pushdown batch (true AND X → X means a filter that was previously
// composite becomes single, which the transpose rules can then push).
func Default() *Driver {
	return NewWithBatches(
		Batch{
			Name:     "constant-folding",
			Strategy: Once(),
			Rules:    []Rule{ConstantFold{}},
		},
		Batch{
			Name:     "predicate-pushdown",
			Strategy: FixedPoint(defaultMaxIterations),
			Rules: []Rule{
				FilterFusion{},
				FilterProjectTranspose(),
				FilterAggregateTranspose(),
				FilterRangeWindowTranspose(),
			},
		},
		Batch{
			Name:     "projection",
			Strategy: FixedPoint(defaultMaxIterations),
			Rules:    []Rule{ProjectionPushdown{}},
		},
	)
}

// Run rewrites plan by applying each batch in order, returning the
// optimized tree. Run never mutates plan; rules construct fresh nodes
// when they rewrite.
func (d *Driver) Run(plan chplan.Node) chplan.Node {
	for _, batch := range d.batches {
		plan = runBatch(plan, batch)
	}
	return plan
}

// runBatch applies batch.Rules to plan per batch.Strategy.
func runBatch(plan chplan.Node, batch Batch) chplan.Node {
	maxIter := batch.Strategy.maxIterations()
	for i := 0; i < maxIter; i++ {
		var iterationChanged bool
		for _, rule := range batch.Rules {
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

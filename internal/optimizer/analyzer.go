package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// AnalyzerRule is a Rule that is *semantically required* — running it is
// part of the language contract, not a heuristic improvement. Examples:
// compile-time arithmetic reduction (`1+2 → 3`, `1=0 → false`) so that
// downstream rules can rely on canonical literal forms; type coercion;
// schema-resolution invariants.
//
// AnalyzerRule borrows the DataFusion `AnalyzerRule` vs `OptimizerRule`
// distinction: analyzer rules are **must-run** and **idempotent** —
// exactly one pass produces a canonical
// tree that subsequent (heuristic) optimizer rules consume. The Driver
// enforces both contracts:
//
//   - must-run: analyzer batches run unconditionally at the top of the
//     pipeline, before any OptimizerRule sees the plan.
//   - idempotent: after the single Once pass, a verification pass must
//     report no further change. Non-idempotent analyzer rules signal a
//     bug; the Driver panics with the offending rule name.
//
// The sealed `isAnalyzerRule()` marker prevents external types from
// claiming the contract without going through this package; declare your
// rule's type here (or alongside an existing analyzer rule) so the
// must-run / idempotent invariants live next to the implementation.
type AnalyzerRule interface {
	Rule
	isAnalyzerRule()
}

// AnalyzerBatch wraps a list of AnalyzerRules into a Batch suitable for
// the Driver. The Strategy is implicitly Analyzer() — Once semantics
// plus an idempotence check on a verification pass.
//
// Use this constructor rather than building a Batch by hand: it enforces
// at compile time that every rule satisfies the AnalyzerRule contract.
// Mixing analyzer + optimizer rules in one batch defeats the split.
func AnalyzerBatch(name string, rules ...AnalyzerRule) Batch {
	r := make([]Rule, len(rules))
	for i, rule := range rules {
		r[i] = rule
	}
	return Batch{
		Name:     name,
		Strategy: Analyzer(),
		Rules:    r,
	}
}

// applyAnalyzerRule is the verification helper used by runBatch when the
// Strategy is Analyzer: it runs the rule once over the tree (the
// "production" pass) and then a second time (the "verification" pass)
// to confirm idempotence. A non-idempotent analyzer rule is a bug — the
// Driver panics with the offending rule's name so the failure surfaces
// at test time, not in production.
//
// Cost: 2x tree walks per analyzer rule. Analyzer batches are small by
// construction (semantic invariants only) so the overhead is bounded.
func applyAnalyzerRule(plan chplan.Node, rule Rule) (chplan.Node, bool) {
	out, changed := applyToTree(plan, rule)
	// Verification pass — must report no further change.
	_, secondChanged := applyToTree(out, rule)
	if secondChanged {
		panic("optimizer: analyzer rule " + rule.Name() + " is not idempotent (verification pass produced a further change)")
	}
	return out, changed
}

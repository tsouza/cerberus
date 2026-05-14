package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// PatternRule is a `Rule` defined declaratively as `match → transform`.
//
// `Match` is a `Pattern` (see pattern.go) that the driver tests against
// every node in the plan tree. When the pattern matches, `Apply` is
// called with the resulting `Bindings`; `Apply` returns the rewritten
// node, or `nil` to signal "no change" (the original node is preserved
// and the rule reports `changed=false`).
//
// PatternRule satisfies the existing `Rule` interface, so the existing
// fixpoint driver (`Driver.Run`) picks it up with no modification — the
// driver's bottom-up tree walk visits every node, and PatternRule does
// its own per-node match. This mirrors Calcite's `RelOptRule`
// (`onMatch`) and Spark Catalyst's `Rule[LogicalPlan]` (`apply(plan)`):
// both engines schedule rules and let each rule decide whether the
// candidate node is a match.
//
// The PatternRule type plus the driver wiring is the canonical shape
// for new rules. The transpose family (FilterProjectTranspose,
// FilterAggregateTranspose, FilterRangeWindowTranspose, and MVSubstitution)
// is built on top.
type PatternRule struct {
	// RuleName is the rule's identifier, surfaced via `Rule.Name()` for
	// debug logging and test fixtures.
	RuleName string

	// Match is the pattern the driver tests against each node.
	Match Pattern

	// Transform is invoked when `Match` succeeds. It returns the
	// rewritten subtree (replacing the matched node) or `nil` to
	// indicate "no change at this node".
	//
	// The supplied `Bindings` map is owned by the caller; rules must
	// not retain it past the call.
	//
	// Field name (`Transform`, not `Apply`) sidesteps the name clash
	// with the `Rule.Apply` method below — Calcite calls the analogous
	// hook `onMatch`, Catalyst calls it `apply`; we go with `Transform`
	// because it reads naturally next to `Match`.
	Transform func(b Bindings) chplan.Node
}

// Name satisfies `Rule`.
func (r *PatternRule) Name() string { return r.RuleName }

// Apply satisfies `Rule`. The existing driver walks the tree bottom-up
// and calls Apply on each node; this method therefore only needs to
// match the single supplied node, not recurse.
func (r *PatternRule) Apply(n chplan.Node) (chplan.Node, bool) {
	if n == nil || r == nil || r.Match == nil || r.Transform == nil {
		return n, false
	}
	b, ok := r.Match.Match(n)
	if !ok {
		return n, false
	}
	rewritten := r.Transform(b)
	if rewritten == nil {
		return n, false
	}
	return rewritten, true
}

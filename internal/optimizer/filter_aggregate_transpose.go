package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// FilterAggregateTranspose rewrites `Filter(Aggregate(X), p)` →
// `Aggregate(Filter(X, p))` when the Filter's predicate references only
// columns that appear, unchanged, as group-by keys.
//
// SPECULATIVE: 0 fires on the current test/spec corpus (measured with
// the total optimizer walk from #812). No live lowering emits a
// group-key label filter stacked *above* an Aggregate — PromQL
// `sum by (job) (m{job="x"})` lowers the `job="x"` matcher into the
// scan PREWHERE, never above the aggregate. The rule is kept as cheap
// correctness insurance: a future lowering that *does* surface such a
// shape is then handled without re-derivation. Do not count it as an
// active optimization. See doc.go for the corpus-wide fire census.
//
// Lineage: Calcite's `FilterAggregateTransposeRule` — see
// https://calcite.apache.org/javadocAggregate/org/apache/calcite/rel/rules/FilterAggregateTransposeRule.html.
// The motivating case is `sum by (job) (m{job="x"})`: the `job="x"`
// predicate over the Aggregate result is equivalent to the same
// predicate applied to the rows feeding the Aggregate. Pushing it
// underneath dramatically shrinks the rows the GROUP BY has to chew
// through, and exposes the predicate to PREWHERE promotion once it
// reaches the Scan.
//
// Safety. The Filter sees two flavours of output column on the
// Aggregate: the group-by keys (`Aggregate.GroupBy`, optionally renamed
// by `GroupByAliases`) and the aggregate output columns
// (`Aggregate.AggFuncs[i].Alias`). Only the first flavour exists in the
// Aggregate's input. We allow the rewrite only when every `ColumnRef`
// in the predicate names a bare group-by key — i.e. there is a
// `GroupBy[i]` that is itself a `*ColumnRef{Name: N}` (no qualifier),
// and the matching `GroupByAliases[i]` (if any) is empty or equal to
// `N`. Predicates referencing aggregate outputs (sum_value, count, …)
// or renamed group keys are left alone.
//
// Conservative cases the rule leaves alone:
//
//   - Empty `GroupBy` — nothing is passthrough.
//   - `GroupBy[i]` that is not a bare `ColumnRef` (e.g. a function
//     call or arithmetic) — could be substituted into the predicate,
//     but that's a more involved rewrite.
//   - `GroupByAliases[i]` that renames the key.
//   - `ColumnRef` with a non-empty Qualifier in the predicate.
//
// Built on the `PatternRule` scaffold.
func FilterAggregateTranspose() Rule {
	return &PatternRule{
		RuleName: "filter-aggregate-transpose",
		Match: WithChildren(
			Capture("filter", Kind(KindFilter)),
			Capture("aggregate", Kind(KindAggregate)),
		),
		Transform: transposeFilterAggregate,
	}
}

func transposeFilterAggregate(b Bindings) chplan.Node {
	fNode, ok := b.Get("filter")
	if !ok {
		return nil
	}
	aNode, ok := b.Get("aggregate")
	if !ok {
		return nil
	}
	f, ok := fNode.(*chplan.Filter)
	if !ok {
		return nil
	}
	a, ok := aNode.(*chplan.Aggregate)
	if !ok {
		return nil
	}

	passthrough := passthroughGroupKeys(a)
	if passthrough == nil {
		return nil
	}
	if !onlyReferencesPassthrough(f.Predicate, passthrough) {
		return nil
	}

	newFilter := &chplan.Filter{
		Input:     a.Input,
		Predicate: f.Predicate,
	}
	newAgg := *a
	newAgg.Input = newFilter
	return &newAgg
}

// passthroughGroupKeys returns the set of bare-column group keys whose
// names survive into the Aggregate's output unchanged, or nil to signal
// "this Aggregate has no passthrough keys — decline the rewrite". An
// alias that renames a key removes it from the passthrough set; the
// emitter would expose the key only under the alias, so the original
// name doesn't exist in the Aggregate's output anyway.
func passthroughGroupKeys(a *chplan.Aggregate) map[string]struct{} {
	if len(a.GroupBy) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(a.GroupBy))
	for i, g := range a.GroupBy {
		cr, ok := g.(*chplan.ColumnRef)
		if !ok || cr.Qualifier != "" {
			continue
		}
		if i < len(a.GroupByAliases) {
			alias := a.GroupByAliases[i]
			if alias != "" && alias != cr.Name {
				continue
			}
		}
		out[cr.Name] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

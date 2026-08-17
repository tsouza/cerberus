package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// onlyReferencesPassthrough reports whether every `*ColumnRef` reachable
// from e is bare (empty Qualifier) and names a column in the supplied
// passthrough set.
//
// Shared by the filter-transpose rules that push a Filter under a
// row-shape-preserving parent (FilterAggregateTranspose,
// FilterRangeWindowTranspose): each computes its own passthrough set
// (group-by keys / series-identity columns) and then asks this helper
// whether the Filter's predicate is safe to push through that boundary.
// Hoisted out of filter_project_transpose.go when FilterProjectTranspose
// (the original definer) was retired, so the live consumers keep a single
// shared implementation.
func onlyReferencesPassthrough(e chplan.Expr, passthrough map[string]struct{}) bool {
	ok := true
	walkExpr(e, func(sub chplan.Expr) {
		cr, isCol := sub.(*chplan.ColumnRef)
		if !isCol {
			return
		}
		if cr.Qualifier != "" {
			ok = false
			return
		}
		if _, found := passthrough[cr.Name]; !found {
			ok = false
		}
	})
	return ok
}

// bareGroupByColumnNames returns the names of every entry in groupBy that is
// a bare, unqualified *chplan.ColumnRef — the entries a Filter above a
// grouping operator (Aggregate, RangeWindow) can reference without any
// rewrite, because they pass through the operator's output unchanged. An
// entry that is not a bare unqualified ColumnRef (a computed expression, or
// a qualified reference) is skipped — it simply contributes no name — and
// does NOT disqualify the other entries.
//
// Skipping (rather than aborting the whole GroupBy) is sound regardless of
// what else sits in the list, computed or not. Every GroupBy entry, bare or
// computed, is a per-row expression evaluated once per the operator's Input
// row; none of them aggregate across rows. So a predicate that names only a
// bare key commutes with the GROUP BY no matter the company: filtering
// Input rows ahead of the grouping can only remove rows, and it removes
// exactly the rows whose group the same predicate would have discarded had
// it run on the grouped output instead — because the bare key's value for a
// surviving row is identical on both sides of the boundary, independent of
// how any other (possibly computed) key in the same row evaluates. There is
// nothing to gain, and no correctness reason, to decline every bare key
// just because one sibling entry needs a substitution this rule doesn't
// perform.
//
// Shared by FilterAggregateTranspose's passthroughGroupKeys (which layers
// GroupByAliases renaming on top — RangeWindow has no alias list) and
// FilterRangeWindowTranspose's seriesIdentifyingColumns (which uses this
// directly). See #2285 for the review that established the two rules
// should use the same policy here.
func bareGroupByColumnNames(groupBy []chplan.Expr) map[string]struct{} {
	if len(groupBy) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(groupBy))
	for _, g := range groupBy {
		cr, ok := g.(*chplan.ColumnRef)
		if !ok || cr.Qualifier != "" {
			continue
		}
		out[cr.Name] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

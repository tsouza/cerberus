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

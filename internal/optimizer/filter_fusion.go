package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// FilterFusion merges `Filter(Filter(X, p1), p2)` into a single
// `Filter(X, p1 AND p2)`. The chsql emitter wraps each Filter in its own
// subquery, so fusion directly drops a SELECT level from the emitted SQL.
type FilterFusion struct{}

func (FilterFusion) Name() string { return "filter-fusion" }

func (FilterFusion) Apply(n chplan.Node) (chplan.Node, bool) {
	outer, ok := n.(*chplan.Filter)
	if !ok {
		return n, false
	}
	inner, ok := outer.Input.(*chplan.Filter)
	if !ok {
		return n, false
	}
	return &chplan.Filter{
		Input:     inner.Input,
		Predicate: &chplan.Binary{Op: chplan.OpAnd, Left: inner.Predicate, Right: outer.Predicate},
	}, true
}

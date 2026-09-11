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
	// Copy-on-write from the outer filter so future fields are preserved while
	// only the input and predicate change, matching chplan.CloneNode's rule.
	fused := *outer
	fused.Input = inner.Input
	fused.Predicate = &chplan.Binary{Op: chplan.OpAnd, Left: inner.Predicate, Right: outer.Predicate}
	return &fused, true
}

package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// FilterRangeWindowTranspose rewrites `Filter(RangeWindow(X), p)` →
// `RangeWindow(Filter(X, p))` when the Filter's predicate references
// only the series-identifying columns the RangeWindow exposes
// unchanged from X.
//
// Fires on shapes like `max_over_time(topk(0, up)[5m:1m])`, whose
// `topk(0, …)` lowers to a `Filter(false)` sitting directly above a
// RangeWindow; this rule pushes that filter under the window.
//
// Lineage: VictoriaMetrics' `metricsql/optimizer.go` pushdown shape —
// see https://github.com/VictoriaMetrics/metricsql/blob/master/optimizer.go.
// The motivating case is `rate(m[5m])` with a downstream label-filter
// like `{job="api"}`: the `job="api"` predicate over the matrix
// (one row per series + step) is equivalent to the same predicate
// applied to the per-sample rows feeding the window. Pushing it
// underneath shrinks the rows the windowed-array idiom has to
// `groupArray` / `arraySort` / `arrayFilter` through, and exposes the
// predicate to PREWHERE promotion once it reaches the Scan.
//
// Safety. The Filter sees three flavours of column on the
// RangeWindow's output:
//
//  1. Series-identifying columns — the bare `ColumnRef` entries of
//     `RangeWindow.GroupBy` (typically `[ColumnRef("Attributes")]`
//     for OTel-CH metrics). These survive unchanged from X.
//  2. The per-step grid timestamp — derived inside the window;
//     does not exist in X with the same semantics.
//  3. The windowed value (`rate`, `*_over_time` output, etc.) —
//     synthesized by the window; does not exist in X at all.
//
// Only flavour (1) is push-safe. We allow the rewrite only when every
// `ColumnRef` in the predicate names a bare group key — i.e. there is a
// `GroupBy[i]` that is itself a `*ColumnRef{Name: N}` with no qualifier.
// Predicates that touch the windowed value, the `TimestampColumn`, or
// the `ValueColumn` are left alone.
//
// Conservative cases the rule leaves alone:
//
//   - Empty `GroupBy` — no per-series identity is passthrough.
//   - `GroupBy[i]` that is not a bare `ColumnRef` (e.g. a function
//     call or arithmetic) — could be substituted into the predicate,
//     but that's a more involved rewrite (mirrors the
//     `FilterAggregateTranspose` policy).
//   - `ColumnRef` with a non-empty `Qualifier` in the predicate.
//   - Mixed predicates (one safe AND one unsafe sub-clause): we keep
//     the *entire* Filter above the RangeWindow. Splitting an AND
//     into push-safe / hold-back halves is conceptually simple but
//     interacts with `FilterFusion` in non-obvious ways (the pushed
//     half would resurface after re-fusion if anything below
//     re-introduces it). FilterFusion + ConstantFold in the
//     predicate-pushdown batch typically pre-flatten composite
//     predicates that are *all* safe; the unsafe-mixed case is rare
//     enough in practice that the conservative policy buys safety
//     without losing much.
//
// Built on the `PatternRule` scaffold.
func FilterRangeWindowTranspose() Rule {
	return &PatternRule{
		RuleName: "filter-range-window-transpose",
		Match: WithChildren(
			Capture("filter", Kind(KindFilter)),
			Capture("range_window", Kind(KindRangeWindow)),
		),
		Transform: transposeFilterRangeWindow,
	}
}

func transposeFilterRangeWindow(b Bindings) chplan.Node {
	fNode, ok := b.Get("filter")
	if !ok {
		return nil
	}
	rNode, ok := b.Get("range_window")
	if !ok {
		return nil
	}
	f, ok := fNode.(*chplan.Filter)
	if !ok {
		return nil
	}
	r, ok := rNode.(*chplan.RangeWindow)
	if !ok {
		return nil
	}

	passthrough := seriesIdentifyingColumns(r)
	if passthrough == nil {
		return nil
	}
	if !onlyReferencesPassthrough(f.Predicate, passthrough) {
		return nil
	}

	newFilter := &chplan.Filter{
		Input:     r.Input,
		Predicate: f.Predicate,
	}
	newRW := *r
	newRW.Input = newFilter
	return &newRW
}

// seriesIdentifyingColumns returns the set of bare-column series-identity
// keys exposed unchanged by r, or nil to signal "this RangeWindow has no
// passthrough keys — decline the rewrite". Computed-key entries (anything
// other than a bare `*chplan.ColumnRef`) cause the entire set to be
// rejected: the caller would have to substitute the computed expression
// into the predicate to keep semantics, and the seed rule stays
// conservative.
func seriesIdentifyingColumns(r *chplan.RangeWindow) map[string]struct{} {
	if len(r.GroupBy) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(r.GroupBy))
	for _, g := range r.GroupBy {
		cr, ok := g.(*chplan.ColumnRef)
		if !ok || cr.Qualifier != "" {
			return nil
		}
		out[cr.Name] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

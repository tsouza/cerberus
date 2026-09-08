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
// predicate to the chsql emitter's PREWHERE promotion once it reaches
// the Scan.
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
//     call or arithmetic) — that entry contributes no passthrough name
//     (see `seriesIdentifyingColumns`, which shares its policy with
//     `FilterAggregateTranspose`'s `passthroughGroupKeys` via
//     `bareGroupByColumnNames`): it is skipped, not substituted, and it
//     does not disqualify the OTHER bare entries in the same `GroupBy`.
//     Skipping a computed key never risks correctness here — a pushed
//     predicate can only ever reference the bare keys still in the
//     passthrough set (`onlyReferencesPassthrough` rejects anything
//     else), so the rule never needs to reason about the computed key's
//     shape at all.
//   - `ColumnRef` with a non-empty `Qualifier` in the predicate.
//   - A `DownsampleTier` RangeWindow. See
//     `rangeWindowReadsInput` for why that arm has to be declined
//     rather than merely rewired, and why the OTHER optional side-scan,
//     `DeltaPrefixAggregateInput`, needs no guard.
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

	if !rangeWindowReadsInput(r) {
		return nil
	}

	passthrough := seriesIdentifyingColumns(r)
	if passthrough == nil {
		return nil
	}
	if !onlyReferencesPassthrough(f.Predicate, passthrough) {
		return nil
	}

	// Copy-on-write from f so Histogram / Mixed ride down with the
	// predicate, matching the newAgg := *a / newRW := *r discipline just
	// below and chplan/clone.go's rule.
	newFilter := *f
	newFilter.Input = r.Input
	newRW := *r
	newRW.Input = &newFilter
	return &newRW
}

// rangeWindowReadsInput reports whether the emitter renders r by reading
// r.Input at all.
//
// The transpose moves the Filter from above r to underneath it, into
// r.Input. That relocation only preserves the answer if the emitter goes
// on to read r.Input — and for one RangeWindow mode it does not.
// `emitRangeWindow` (internal/chsql/range_window.go) checks
// `r.DownsampleTier` before any other dispatch and hands the node to
// `emitRangeWindowDownsampleTier`, which answers the query entirely from
// r.DownsampleTierInput's bucketed aggregate state; its own doc states
// "r.Input is UNUSED here". A predicate transposed into r.Input in that
// mode is therefore not pushed down, it is DELETED: the emitted SQL loses
// the WHERE clause and its bound argument outright, so a query that must
// return no rows returns every row instead.
//
// r.DownsampleTier is the right thing to test rather than
// r.DownsampleTierInput being non-nil: lowering populates the side-scan
// whenever the schema offers a tier table, and the node is only read from
// it when the flag is also set (internal/chplan/range_window.go).
// Declining on the populated-but-inert shape would give up a sound
// pushdown for nothing.
//
// The other optional side-scan, r.DeltaPrefixAggregateInput, needs no
// guard, and the reason is worth stating because the transform's silence
// about it reads like an oversight. The emitter drives that join from the
// r.Input-derived side with a LEFT JOIN keyed on the group columns
// (internal/chsql/range_window.go's instant and matrix arms both build
// `FROM <window> LEFT JOIN <agg> ON <group cols>`). This rule only ever
// pushes a predicate over bare `GroupBy` ColumnRefs — exactly those join
// keys. So the rows an unfiltered aggregate side still contributes are
// rows whose key the predicate rejects, and those keys are by
// construction absent from the filtered driving side: they find no
// partner and reach no output. The answer is unchanged; only some
// needless scan work on the side-scan survives.
func rangeWindowReadsInput(r *chplan.RangeWindow) bool {
	return !r.DownsampleTier
}

// seriesIdentifyingColumns returns the set of bare-column series-identity
// keys exposed unchanged by r, or nil to signal "this RangeWindow has no
// passthrough keys — decline the rewrite". A computed-key entry (anything
// other than a bare `*chplan.ColumnRef`) simply contributes no name to the
// set; it does not disqualify the RangeWindow's other, bare keys. See
// `bareGroupByColumnNames` for why that's sound: `RangeWindow.GroupBy` has
// the same per-row, series-identity semantics as `Aggregate.GroupBy` (the
// doc comment above), so the two rules share one policy here.
func seriesIdentifyingColumns(r *chplan.RangeWindow) map[string]struct{} {
	return bareGroupByColumnNames(r.GroupBy)
}

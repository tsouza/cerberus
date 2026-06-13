package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitRangeBucketFanout renders a chplan.RangeBucketFanout — the
// single-pass, bounded sample-side fan-out that supersedes the StepGrid
// CROSS JOIN + per-anchor lookback Filter + per-(series, anchor)
// Aggregate shape for the array-valued histogram-quantile /
// histogram-value-function range lowerings. It is the array-aggregate
// sibling of emitRangeLWR and reuses lwrAnchorFanoutFrag verbatim for the
// bounded anchor fan-out.
//
// SQL skeleton (N = (End-Start)/Step + 1 grid anchors, but the
// intermediate cardinality is rows × (Lookback/Step + 1), constant in N):
//
//	SELECT <user-key_1> AS <alias_1>, …, anchor_ts AS <AnchorAlias>,
//	       <AggFuncs[i].Name>(<args>) AS <AggFuncs[i].Alias>, …
//	FROM (
//	  SELECT *,
//	         arrayJoin(arrayMap(i -> <grid_base> - toIntervalNanosecond(i * <stepNS>),
//	                   range(greatest(0, floorIdx(dist - lookback)),
//	                         least(<N>, floorIdx(dist))))) AS anchor_ts
//	  FROM (<Input>)
//	)
//	GROUP BY <user-key_1>, …, anchor_ts
//
// where `dist = dateDiff('nanosecond', TimeUnix, <shift_base>)` is the
// sample's distance behind the newest OFFSET-SHIFTED anchor (identical to
// emitRangeLWR's window math). Each sample fans to only the
// ≤ Lookback/Step + 1 anchors whose half-open staleness window
// `(anchor - Offset - Lookback, anchor - Offset]` contains it; the
// `GROUP BY (<user-keys>, anchor_ts)` then collapses each (series,
// anchor) bucket with the configured AggFuncs. An anchor with no sample
// in its window receives no fanned row and so produces no GROUP BY row —
// preserving Prom's staleness gap, exactly as the old CROSS JOIN +
// lookback Filter did.
//
// The fanout SELECT projects `*` so the inner Input's columns (the
// AggFunc source columns + the group-key source columns + TimeUnix) flow
// through unchanged to the collapse SELECT; the only added column is the
// computed `anchor_ts`. The collapse SELECT keeps the anchor under its
// own `anchor_ts` alias (no re-alias to TimestampCol) so an
// `argMax(<col>, TimeUnix)` AggFunc resolves its TimeUnix argument to the
// inner per-sample source column rather than a same-SELECT output alias —
// the same alias-shadowing trap emitRangeLWR documents. Because no
// AggFunc output alias collides with TimestampCol, no outer re-alias
// Project is needed; the wrapping chplan Project (added by the lowering)
// re-aliases anchor_ts → TimeUnix downstream.
func (e *emitter) emitRangeBucketFanout(r *chplan.RangeBucketFanout) error {
	if r.Step <= 0 {
		return fmt.Errorf("%w: RangeBucketFanout requires Step > 0", ErrUnsupported)
	}
	if r.Input == nil {
		return fmt.Errorf("%w: RangeBucketFanout.Input is nil", ErrUnsupported)
	}
	if r.TimestampCol == "" {
		return fmt.Errorf("%w: RangeBucketFanout requires TimestampCol", ErrUnsupported)
	}
	if r.AnchorAlias == "" {
		return fmt.Errorf("%w: RangeBucketFanout requires AnchorAlias", ErrUnsupported)
	}
	if len(r.AggFuncs) == 0 {
		return fmt.Errorf("%w: RangeBucketFanout requires at least one AggFunc", ErrUnsupported)
	}

	// Pre-flight group-key + aggregate expressions so chplan errors
	// surface synchronously (mirrors emitAggregate).
	for _, g := range r.GroupBy {
		if err := (&Builder{}).Expr(g); err != nil {
			return err
		}
	}
	for _, af := range r.AggFuncs {
		for _, p := range af.Params {
			if err := (&Builder{}).Expr(p); err != nil {
				return err
			}
		}
		for _, a := range af.Args {
			if err := (&Builder{}).Expr(a); err != nil {
				return err
			}
		}
	}

	stepNS := r.Step.Nanoseconds()
	lookbackNS := r.Lookback.Nanoseconds()

	// End-inclusive anchor count across the [Start, End] grid. When the
	// grid bounds are absent (the now64(9) fixture shape) a single anchor
	// is the only deterministic choice; the bounded fanout still applies.
	var numAnchors int64 = 1
	if !r.Start.IsZero() && !r.End.IsZero() {
		span := r.End.Sub(r.Start).Nanoseconds()
		if span < 0 {
			return fmt.Errorf("%w: RangeBucketFanout.Start > End", ErrUnsupported)
		}
		numAnchors = span/stepNS + 1
	}

	// Membership base (offset-shifted newest anchor) and value base
	// (unshifted grid anchor). Offset folds onto the membership base only.
	shiftBase := offsetShiftedBaseFrag(timeOrNowFrag(r.End), r.Offset)
	gridBase := timeOrNowFrag(r.End)

	inner, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}

	tsIdent := func(b *Builder) { b.Ident(r.TimestampCol) }

	// Sample-fanout SELECT: pass through every Input column (`*`) and add
	// the bounded grid anchor. `*` is required so the AggFunc source
	// columns + group-key source columns + TimeUnix all reach the collapse
	// SELECT without enumerating the (schema-dependent) column set here.
	fanout := NewQuery().From(inner)
	fanout.Select(Star())
	fanout.Select(rawAs(
		lwrAnchorFanoutFrag(gridBase, shiftBase, tsIdent, stepNS, lookbackNS, numAnchors),
		r.AnchorAlias,
	))

	// Collapse SELECT: GROUP BY (<user-keys>, anchor) with the configured
	// AggFuncs. The user group keys are projected first (under their
	// aliases) then the anchor, matching the column order the replaced
	// Aggregate node emitted (anchor_ts came first there, but the
	// downstream reshape Project references every column by name, not
	// position, so the surface order is observationally identical).
	collapse := NewQuery().From(fanout.Frag())
	collapse.Select(As(verbatim(r.AnchorAlias), r.AnchorAlias))
	for i, g := range r.GroupBy {
		expr := g
		alias := ""
		if i < len(r.GroupByAliases) {
			alias = r.GroupByAliases[i]
		}
		collapse.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	for _, af := range r.AggFuncs {
		af := af
		collapse.Select(aggFuncFrag(af))
	}

	// GROUP BY (<user-key exprs>, anchor_ts). CH groups by the underlying
	// expression for the user keys (not the alias) and by the anchor's
	// bare name. The anchor is referenced verbatim because it is the
	// fanout SELECT's output column, not a base-table column.
	groupFrags := make([]Frag, 0, len(r.GroupBy)+1)
	groupFrags = append(groupFrags, verbatim(r.AnchorAlias))
	for _, g := range r.GroupBy {
		expr := g
		groupFrags = append(groupFrags, func(b *Builder) { _ = b.Expr(expr) })
	}
	collapse.GroupBy(groupFrags...)

	e.emitSelect(collapse)
	return nil
}

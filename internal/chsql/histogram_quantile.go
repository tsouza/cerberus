package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitHistogramQuantile renders a chplan.HistogramQuantile against the
// OTel-CH classic histogram schema (parallel BucketCounts × ExplicitBounds
// arrays per row).
//
// The CH expression chain — for each row out of the inner subquery —
// computes the interpolated quantile:
//
//  1. cum = arrayCumSum(BucketCounts) — running totals across buckets.
//  2. total = cum[length(cum)] — total observations (last bucket is +Inf,
//     so this captures every observation).
//  3. target = phi * total — the desired cumulative-count cutoff.
//  4. idx = arrayFirstIndex(c -> c >= target, cum) — the 1-based bucket
//     index whose cumulative first crosses target. Zero means no bucket
//     reaches target (only possible when phi <= 0 + total > 0; the phi
//     guard handles it).
//  5. Linear interpolation between the bucket's lower and upper bounds
//     using the cumulative counts at each edge. The lower edge of bucket
//     1 is (bound=0, cum=0); subsequent buckets read from ExplicitBounds
//     and cum at idx-1. The trailing +Inf bucket (idx == length(cum))
//     returns the highest explicit bound — matching upstream Prometheus.
//
// Prom edge cases mirrored:
//
//   - total = 0 (empty histogram) → NaN.
//   - phi >= 1 → highest explicit bound (so p1.0 reads from the last
//     finite edge, not +Inf).
//   - phi <= 0 → lowest explicit bound (Prom's convention for
//     non-negative observations).
//   - Any other phi → linear interpolation per the steps above.
//
// The outer QueryBuilder projects the GroupBy columns aliased per
// GroupByAliases, then the interpolated quantile as the `Value` column,
// matching the Sample contract the lowering's wrapping Project consumes.
func (e *emitter) emitHistogramQuantile(h *chplan.HistogramQuantile) error {
	if h.Input == nil {
		return fmt.Errorf("%w: HistogramQuantile.Input is nil", ErrUnsupported)
	}
	if h.BucketCountsColumn == "" || h.ExplicitBoundsColumn == "" {
		return fmt.Errorf("%w: HistogramQuantile requires BucketCountsColumn and ExplicitBoundsColumn", ErrUnsupported)
	}
	// Pre-flight every GroupBy expression so chplan errors surface
	// synchronously rather than from inside a Frag callback.
	for _, g := range h.GroupBy {
		if err := (&Builder{}).Expr(g); err != nil {
			return err
		}
	}

	sub, err := e.subqueryFrag(h.Input)
	if err != nil {
		return err
	}

	sb := NewQuery().From(sub)
	for i, g := range h.GroupBy {
		expr := g
		alias := ""
		if i < len(h.GroupByAliases) {
			alias = h.GroupByAliases[i]
		}
		sb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	sb.SelectAs(histogramQuantileValueFrag(h), "Value")
	e.emitSelect(sb)
	return nil
}

// histogramQuantileValueFrag returns the Frag that renders the per-row
// quantile-interpolation expression. A literal phi is rendered as an
// inline float literal (query-shape parameter, mirrors
// holtWintersValueExpr's sf / tf treatment); a computed phi
// (h.PhiExpr != nil) renders the expression — typically a scalar
// subquery, which CH evaluates once and folds as a constant — at every
// phi position, wrapped in a leading `isNaN(phi) → nan` branch
// because a runtime phi can be NaN (PromQL `scalar()` over zero / many
// series) and Prom's bucketQuantile returns NaN there. Bound user data
// lives on the wrapping Filter / Project, not on the per-row
// arithmetic.
//
// The expression is structured as nested if(...) clauses so the
// edge-case branches stay inlined (no CASE WHEN, no CTEs) — keeps the
// query shape stable for CH's planner.
func histogramQuantileValueFrag(h *chplan.HistogramQuantile) Frag {
	bc := h.BucketCountsColumn
	eb := h.ExplicitBoundsColumn
	// BucketCounts is Array(UInt64) in the OTel-CH schema; arraySum /
	// arrayCumSum on it return UInt64. The downstream linear-interpolation
	// arithmetic mixes those with Float64 ExplicitBounds and the `0.0`
	// edge-case literals, which CH rejects with NO_COMMON_TYPE
	// ("some are integers and some are floating point"). Cast BucketCounts
	// to Array(Float64) once at the entry so every sum / cumsum derives
	// Float64 and the interpolation arithmetic stays in a single numeric
	// domain. CSE folds the cast across the many references.
	bcFloat := Call("arrayMap", Lambda1("x", Call("toFloat64", BareIdent("x"))), Col(bc))
	lengthBC := Call("length", Col(bc))
	lengthEB := Call("length", Col(eb))
	arraySumBC := Call("arraySum", bcFloat)
	arrayCumSumBC := Call("arrayCumSum", bcFloat)

	// phi renders the phi parameter: the computed expression when PhiExpr
	// is set, the inline float literal (query-shape param, mirrors
	// holtWintersValueExpr's sf / tf) otherwise. Re-invoked at each phi
	// position so each carries its own `?` placeholder for the PhiExpr
	// case — matching the legacy emitter's per-position re-emission.
	phi := func() Frag {
		if h.PhiExpr != nil {
			return func(b *Builder) { _ = b.Expr(h.PhiExpr) }
		}
		return InlineLit(h.Phi)
	}
	// `nan` / `0.0` are CH-portable shape tokens, not data: InlineLit
	// would render `nan` as the quoted string `'nan'` and `0.0` as the
	// canonicalised `0`, so they ride verbatim (the same posture as
	// IfNonZero's `0.0` fallback in builder.go).
	nan := verbatim("nan")
	zeroF := verbatim("0.0")
	highestBound := Subscript(Col(eb), lengthEB) // ExplicitBounds[length(ExplicitBounds)]
	target := Paren(Mul(phi(), arraySumBC))      // (phi * arraySum(bc))

	// idx = arrayFirstIndex(c -> c >= target, cum). Computed phi:
	// ClickHouse 24.8 rejects a scalar subquery anywhere in
	// arrayFirstIndex's argument tree with ILLEGAL_COLUMN ("Unexpected
	// type of filter column") — the lambda's comparison result stops
	// being the plain UInt8 filter column the higher-order filter
	// machinery expects (newer CH accepts it). Wrapping the predicate as
	// `(if(<cmp>, 1, 0) = 1)` restores the constant-folded UInt8 the 24.8
	// filter path requires. The literal path keeps the bare comparison
	// (byte-stable fixtures). idx is re-evaluated at each use site rather
	// than CTE- d; CH's CSE folds it.
	idx := func() Frag {
		cmp := Gte(BareIdent("c"), target)
		pred := cmp
		if h.PhiExpr != nil {
			pred = Paren(Eq(If(cmp, InlineLit(1), InlineLit(0)), InlineLit(1)))
		}
		return Call("arrayFirstIndex", Lambda1("c", pred), arrayCumSumBC)
	}
	// idxAtOffset renders `<idx>` or `<idx> - 1` (the `idx - 1` lower-edge
	// lookups). offsetMinusOne selects the `- 1` form.
	idxAtOffset := func(offsetMinusOne bool) Frag {
		if offsetMinusOne {
			return Sub(idx(), InlineLit(1))
		}
		return idx()
	}
	cumAt := func(offsetMinusOne bool) Frag {
		return Subscript(arrayCumSumBC, idxAtOffset(offsetMinusOne))
	}
	boundAt := func(offsetMinusOne bool) Frag {
		return Subscript(Col(eb), idxAtOffset(offsetMinusOne))
	}
	// Lower-edge selectors branch on idx = 1 → 0.0, else the [idx-1]
	// lookup. bound_lo / cum_lo per the interpolation below.
	boundLo := If(Eq(idx(), InlineLit(1)), zeroF, boundAt(true))
	cumLo := If(Eq(idx(), InlineLit(1)), zeroF, cumAt(true))

	// Interpolation:
	//   bound_lo + (bound_hi - bound_lo) * (target - cum_lo) / (cum_hi - cum_lo)
	// bound_hi = ExplicitBounds[idx]; cum_hi = cum[idx]; target = phi *
	// arraySum(bc). The grouping parens match the legacy emitter exactly:
	//   (bound_lo + (bound_hi - bound_lo) * ((target) - cum_lo) / (cum_hi - cum_lo))
	interp := Paren(
		Add(
			boundLo,
			Div(
				Mul(
					Paren(Sub(boundAt(false), boundLo)),
					Paren(Sub(target, cumLo)),
				),
				Paren(Sub(cumAt(false), cumLo)),
			),
		),
	)

	// if(idx = length(cum), highest_bound, <interp>) — the +Inf bucket
	// (only the trailing bucket crosses target) returns the highest
	// explicit bound.
	idxBranch := If(Eq(idx(), Call("length", arrayCumSumBC)), highestBound, interp)

	// Nested edge-case chain, outermost first:
	//   if(length(bc) = 0, nan,
	//      if(arraySum(bc) = 0, nan,
	//         if(phi <= 0, 0.0,
	//            if(phi >= 1, highest_bound, idxBranch))))
	core := If(Eq(lengthBC, InlineLit(0)), nan,
		If(Eq(arraySumBC, InlineLit(0)), nan,
			If(Lte(phi(), InlineLit(0)), zeroF,
				If(Gte(phi(), InlineLit(1)), highestBound, idxBranch))))

	if h.PhiExpr == nil {
		return core
	}
	// Computed phi can be NaN at runtime; every comparison branch above
	// evaluates false on NaN and the interpolation would index cum[0] —
	// guard with a leading isNaN → nan branch (Prom's bucketQuantile
	// NaN-phi contract). The literal path skips the wrapper so existing
	// fixtures stay byte-stable.
	return If(Call("isNaN", phi()), nan, core)
}

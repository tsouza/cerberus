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
// Prom edge cases mirrored (quantile.go:114-119):
//
//   - total = 0 (empty histogram) → NaN.
//   - phi < 0 → -Inf (out of domain).
//   - phi > 1 → +Inf (out of domain).
//   - phi == 1 → highest explicit bound (in domain; the idx == length(cum)
//     guard reads the last finite edge, not +Inf).
//   - phi == 0 → lowest explicit bound (in domain; idx == 1 lower edge).
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
	return e.emitSelect(sb)
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
	//
	// Monotonicity invariant — why there is no counterpart to Prometheus's
	// ensureMonotonicAndIgnoreSmallDeltas here. Prometheus repairs its
	// ladder before interpolating because its input is already CUMULATIVE:
	// one independently-computed float per `le`, so a later `le` can carry
	// a smaller count than an earlier one. Cerberus's input is the opposite
	// shape — BucketCountsColumn is a PER-BUCKET, non-negative array
	// (Array(UInt64) in the OTel-CH schema, element-wise summed by
	// sumForEach when the lowering aggregates) — and the ladder is built
	// HERE by arrayCumSum. IEEE-754 addition is correctly rounded and
	// monotone, so cum[i+1] = fl(cum[i] + x[i]) >= cum[i] for every
	// x[i] >= 0: the ladder is non-decreasing by construction and
	// upstream's `curr < prev` branch has no reachable work to do. A
	// zero-count bucket yields a flat run — precisely the shape the
	// upstream repair outputs (pinned by the
	// histogram_quantile_classic_*_plateau fixtures).
	//
	// h.BucketCountsCumulative is the path that breaks that precondition:
	// the bucket-layout merge hands this node an ALREADY-cumulative,
	// independently-derived per-`le` ladder (one rung per bound in the
	// union of the group's layouts, folded across rows in the cumulative
	// domain). There arrayCumSum must NOT run — the array is the ladder —
	// and the producer owes Prometheus's ensureMonotonicAndIgnoreSmallDeltas
	// repair before this node sees it (chplan.HistogramQuantile documents
	// that debt). `total` below is then read off the ladder's top rung
	// rather than summed, or target and ladder disagree exactly when the
	// repair fires.
	bcFloat := Call("arrayMap", Lambda1("x", Call("toFloat64", BareIdent("x"))), Col(bc))
	lengthBC := Call("length", Col(bc))

	// Coalesce buckets that share an upper bound, mirroring upstream's
	// coalesceBuckets (promql/quantile.go), which runs before every
	// interpolation. A repeated entry in ExplicitBounds describes a bucket
	// whose interval is empty, so it carries no observation of its own; left
	// in place it still splits the ladder, and arrayFirstIndex then stops at
	// the FIRST rung of the flat run it creates. The lower edge read off
	// that rung is the repeated bound rather than the true start of the
	// bucket holding the rank, and the interpolation collapses onto the
	// bound instead of interpolating across it — bounds [1, 1, 5] with
	// counts [2, 3, 5, 0] answer phi=0.3 with 1 where the ladder
	// (5 observations at or below 1) puts the rank at 0.6.
	//
	// Upstream merges by ADDING the duplicate rungs' counts because its
	// input is one already-cumulative sample per `le`, and two entries with
	// equal bounds are two independent series. Here the row is a single
	// per-bucket array whose running total already absorbed the run, so the
	// equivalent merge is to keep the LAST index of each run of equal
	// bounds: its cumulative entry is exactly the total at that bound.
	// The trailing +Inf rung is appended unconditionally — it has no
	// ExplicitBounds entry to be duplicated, and its cumulative value is
	// the total, which coalescing never changes.
	//
	// Duplicate-free input (every real producer) keeps every index, so the
	// coalesced arrays are element-wise identical to the raw ones and the
	// quantile is unchanged.
	rawCumSum := Call("arrayCumSum", bcFloat)
	if h.BucketCountsCumulative {
		rawCumSum = bcFloat
	}
	boundCount := Call("length", Col(eb))
	cumCount := Call("length", bcFloat)
	// Rungs the ladder keeps: the last index of every run of equal bounds,
	// then every cumulative entry past the bounds array untouched. That
	// tail is the overflow rung the schema carries when BucketCounts runs
	// one longer than ExplicitBounds; rebuilding it from arraySum instead
	// would ADD a rung whenever the two arrays are the same length, so it
	// is carried across verbatim and the ladder's height is preserved
	// exactly.
	keptBoundIdx := Call(
		"arrayFilter",
		Lambda1("i", Or(
			Eq(BareIdent("i"), boundCount),
			Neq(Subscript(Col(eb), BareIdent("i")), Subscript(Col(eb), Add(BareIdent("i"), InlineLit(1)))),
		)),
		Call("range", InlineLit(1), Add(boundCount, InlineLit(1))),
	)
	keptCumIdx := Call(
		"arrayConcat",
		keptBoundIdx,
		Call("range", Add(boundCount, InlineLit(1)), Add(cumCount, InlineLit(1))),
	)
	coalescedEB := Call("arrayMap", Lambda1("i", Subscript(Col(eb), BareIdent("i"))), keptBoundIdx)
	coalescedCumSum := Call("arrayMap", Lambda1("i", Subscript(rawCumSum, BareIdent("i"))), keptCumIdx)

	lengthEB := Call("length", coalescedEB)
	arrayCumSumBC := coalescedCumSum

	// observations is Prometheus's `observations` (quantile.go): the
	// ladder's top rung. Per-bucket input reaches it by summing the array;
	// cumulative input already carries it as the last coalesced entry, and
	// summing there would total the ladder instead of reading it. Both
	// forms are the same value in the per-bucket mode, so the sum form
	// stays there and existing fixtures are byte-stable.
	observations := Call("arraySum", bcFloat)
	if h.BucketCountsCumulative {
		observations = Subscript(arrayCumSumBC, Call("length", arrayCumSumBC))
	}

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
	negInf := verbatim("-inf")
	posInf := verbatim("inf")
	zeroF := verbatim("0.0")
	highestBound := Subscript(coalescedEB, lengthEB) // coalesced ExplicitBounds' last entry
	target := Paren(Mul(phi(), observations))        // (phi * observations)

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
		return Subscript(coalescedEB, idxAtOffset(offsetMinusOne))
	}
	// Lower-edge selectors branch on idx = 1 → 0.0, else the [idx-1]
	// lookup. bound_lo / cum_lo per the interpolation below.
	boundLo := If(Eq(idx(), InlineLit(1)), zeroF, boundAt(true))
	cumLo := If(Eq(idx(), InlineLit(1)), zeroF, cumAt(true))

	// Interpolation:
	//   bound_lo + (bound_hi - bound_lo) * (target - cum_lo) / (cum_hi - cum_lo)
	// bound_hi = ExplicitBounds[idx]; cum_hi = cum[idx]; target = phi *
	// observations. The grouping parens match the legacy emitter exactly:
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
	//
	// Nested inside the else: upstream's `b == 0 && buckets[0].UpperBound
	// <= 0` short-circuit. The interpolation below assumes the first
	// bucket spans `(0, ExplicitBounds[1]]`, which is only a bucket when
	// that bound is positive. OTel permits negative and zero explicit
	// bounds, and there the assumed lower edge sits ABOVE the upper one:
	// interpolating across `[0, -10]` walks a negative-width interval and
	// answers with a number no observation could have produced. Upstream
	// returns the bound itself, so every rank landing in that bucket
	// reports its upper edge. The branch order matches BucketQuantile's
	// switch exactly — a lone +Inf bucket satisfies both predicates and
	// upstream resolves it as the trailing bucket.
	lowestBound := Subscript(coalescedEB, InlineLit(1))
	firstBucketNonPositive := And(Eq(idx(), InlineLit(1)), Lte(lowestBound, InlineLit(0)))
	idxBranch := If(
		Eq(idx(), Call("length", arrayCumSumBC)),
		highestBound,
		If(firstBucketNonPositive, lowestBound, interp),
	)

	// Nested edge-case chain, outermost first:
	//   if(length(bc) = 0, nan,
	//      if(observations = 0, nan,
	//         if(phi < 0, -inf,
	//            if(phi > 1, inf, idxBranch))))
	//
	// Prometheus semantics (quantile.go:114-119): phi < 0 → -Inf, phi > 1
	// → +Inf are OUT of domain. phi == 0 and phi == 1 are IN domain and
	// fall through to idxBranch: phi == 0 → target 0 → idx 1 → lower edge
	// 0.0; phi == 1 → target observations → idx == length(cum) → highest finite
	// bound. ClickHouse parses the bare `inf` / `-inf` tokens as Float64.
	core := If(Eq(lengthBC, InlineLit(0)), nan,
		If(Eq(observations, InlineLit(0)), nan,
			If(Lt(phi(), InlineLit(0)), negInf,
				If(Gt(phi(), InlineLit(1)), posInf, idxBranch))))

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

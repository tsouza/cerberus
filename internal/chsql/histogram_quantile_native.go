// histogram_quantile_native.go emits the SQL for
// chplan.HistogramQuantileNative against the OTel-CH exponential
// (native) histogram table.
//
// Algorithm (full walk — negative + zero + positive):
//
//  1. base = pow(2, pow(2, -Scale)). Higher scale = finer buckets.
//  2. cum = arrayCumSum(arrayConcat(
//     arrayReverse(NegativeBucketCounts),
//     [ZeroCount],
//     PositiveBucketCounts)).
//     The NegativeBucketCounts array is reversed so that the cum-sum
//     walks from the most-negative bucket (largest |value|, last in
//     the original array) toward the least-negative, then through the
//     zero bucket, then through PositiveBucketCounts in natural
//     order. arrayReverse([]) = [], so distributions with no negative
//     observations collapse to the positive-only walk
//     (`arrayConcat([ZeroCount], PositiveBucketCounts)`).
//  3. revCum = arrayReverse(arrayCumSum(arrayReverse(buckets))) — the
//     same bucket array accumulated from the TOP instead of the bottom,
//     so revCum[i] is the count at or above position i. It is the
//     backward arm's running count (step 4). The RANK is taken against
//     the histogram's own stored Count column (rankBase), matching
//     reference Prometheus's `HistogramQuantile`, which ranks against
//     `h.Count` and never re-derives a total from the bucket array. The
//     two agree everywhere except when the histogram counted NaN
//     observations: those inflate stored Count without landing in any
//     bucket, so Sum comes out NaN (the only way Count can exceed the
//     bucket-derived total) and rankBase > revCum[1].
//     target = phi * rankBase.
//  4. idx = the 1-based position the rank walk stops on. Reference
//     Prometheus walks the buckets in one of TWO directions depending
//     on phi AND on whether Sum is NaN (quantile.go:243-262), so
//     cerberus does too:
//     - phi < reverseWalkPhi, OR Sum is NaN: forward, rank =
//     phi * rankBase, stopping at
//     idx = arrayFirstIndex(c -> c >= target, cum).
//     Reference forces the forward arm whenever Sum is NaN regardless
//     of phi, because a reverse walk cannot find a percentile whose
//     mass sits outside every bucket.
//     - phi >= reverseWalkPhi AND Sum is not NaN: backward from the
//     top, rank = (1 - phi) * rankBase. Reference accumulates the
//     bucket counts downward from the highest bucket and stops on the
//     first one whose running total reaches the rank; revCum IS that
//     running total, and since it is non-increasing in i the bucket it
//     stops on is the LAST position still reaching the rank:
//     idx = arrayLastIndex(c -> c >= rank, revCum).
//     The running count is accumulated rather than derived as
//     `total - cum[i-1]`: the two are equal in exact arithmetic, but
//     the subtraction cancels away low-order bits, and once a
//     rate()/increase() window fold has scaled every bucket by one
//     extrapolation factor the counts are Float64 rather than exact
//     integers. A few ULPs there move an EXACT rank tie one bucket, and
//     across the zero bucket that flips the answer's sign. Accumulating
//     in reference's own order and direction reproduces its running
//     count bit for bit, so the tie resolves the same way it does
//     upstream.
//     The two directions agree everywhere except at an exact rank tie
//     (target lands exactly on a cum boundary): forward stops on the
//     bucket whose cumulative count ENDS at the target and reports its
//     upper edge, backward stops on the next POPULATED bucket and
//     reports its lower edge. Those are the same number unless an empty
//     bucket — or a region boundary — separates the two, which is why a
//     histogram with equal negative and positive mass either side of an
//     empty zero bucket answered phi = 0.5 with the wrong SIGN before
//     the backward arm existed.
//     Both arms skip empty buckets for free, exactly as reference's own
//     `if bucket.Count == 0 { continue }` does. cum is non-decreasing, so
//     an empty bucket repeats its predecessor's value and can never be
//     the FIRST index to reach a strictly positive rank; revCum is
//     non-increasing, so an empty bucket repeats its SUCCESSOR's value
//     and can never be the LAST index still reaching one. Only rank = 0
//     could stop on an empty bucket, and that is exactly the phi = 0 /
//     phi = 1 pair the phiLow / phiHigh edges handle explicitly (step 6).
//     A rank ABOVE every bucket's reach — rankBase > the bucket-derived
//     total, which only happens when Sum is NaN and the walk is forced
//     forward — leaves arrayFirstIndex with nothing to stop on; it
//     returns 0, and the
//     outer chain (step 6a) answers NaN, matching reference's own
//     "walk exhausted every bucket without reaching the rank" result.
//     Regions: idx ∈ [1, nlen] is the negative-side walk;
//     idx = nlen+1 is the zero bucket; idx > nlen+1 is the positive
//     walk. (nlen = length(NegativeBucketCounts).)
//  5. fraction = (target - cum[idx-1]) / bucketCount(valueIdx), where
//     valueIdx normally equals idx. For a NaN Sum, reference Prometheus
//     continues the forward iterator to its end to detect NaN skew after
//     rebasing the rank in idx. That annotation scan leaves `bucket` on
//     the final iterated bucket, and the subsequent interpolation uses
//     that bucket's geometry and count. valueIdx reproduces that observable
//     behavior by selecting the last positive bucket, otherwise the
//     populated zero bucket, otherwise the last negative bucket.
//     ClickHouse returns the array element's default (0) for
//     `cum[0]`, which matches the "no bucket consumed yet" semantics
//     when idx = 1, so the formula needs no explicit guard.
//     One formula serves both walk directions. Reference Prometheus
//     rebases the remaining rank per direction (`rank -= count -
//     bucket.Count` forward, `rank = count - rank` backward) and then
//     divides by the bucket's own count; substituting count =
//     total - cum[idx-1] for the backward arm reduces its expression to
//     this same (target - cum[idx-1]) / bucketCount, so only the STOP
//     INDEX differs between the arms, never the interpolation.
//  6. Edge cases (Prometheus quantile.go:114-119), tested in THIS order —
//     out-of-domain phi is checked before the empty-histogram case, so a
//     `phi` outside [0, 1] answers ±Inf even for an empty histogram
//     (cerberus issue #2067; reference quantile.go:222-232 checks
//     `q < 0` / `q > 1` before `h.Count == 0`):
//     - phi < 0 → -Inf (out of domain).
//     - phi > 1 → +Inf (out of domain).
//     - rankBase (stored Count) = 0 → NaN (empty histogram).
//     - phi == 0 → lower edge of the lowest POPULATED bucket (in
//     domain), and phi == 1 → upper edge of the highest populated
//     one. Both saturate to a bucket some observation fell in,
//     because the reference rank walk skips zero-count buckets
//     (quantile.go: `if bucket.Count == 0 { continue }`) and a
//     bucket array may carry zeros anywhere — the offsets describe
//     only the leading gap. Each is the step-7 interpolation for
//     whichever region that bucket lies in, evaluated at
//     fraction = 0 (lower) / 1 (upper), with idx replaced by
//     `arrayFirstIndex` / `arrayLastIndex` of a non-zero count over
//     the same concatenated walk order.
//     - idx = 0 → NaN. Both index functions return 0 when no element of
//     the running-count array satisfies the walk's comparison — the
//     "walk exhausted every bucket without reaching the rank" case from
//     step 4, reachable only when Sum is NaN and rankBase (Count)
//     exceeds every bucket's reach. Checked after phi == 0 / phi == 1
//     (step 6a) since those saturate rather than walk.
//  7. Interpolation, by region:
//     - Negative bucket (idx ≤ nlen). Original-array 0-based index
//     within Negative is `nlen - idx`; absolute exp-bucket index is
//     `NegativeOffset + (nlen - idx)`. The bucket covers
//     `[-base^(idx+1), -base^idx)`; the cum-sum enters from the
//     more-negative edge and accumulates toward the less-negative
//     edge, so
//     `value = -pow(base, NegativeOffset + (nlen - idx) + 1 - fraction)`.
//     (fraction=0 → most-negative edge; fraction=1 → least-negative
//     edge.)
//     - Zero bucket (idx = nlen+1). Linear interpolation across the
//     zero band `[lower, upper]`:
//     `value = lower + (upper - lower) * fraction`.
//     The band is `[-ZeroThreshold, +ZeroThreshold]` only when the
//     distribution can hold observations on both sides of zero. A
//     one-sided distribution clamps the side that cannot
//     (quantile.go:263-273): with no negative buckets the band starts
//     at 0, with no positive buckets it ends at 0. Without the clamp a
//     positive-only histogram answers any phi in the lower half of its
//     zero bucket with a NEGATIVE value — a number no observation in it
//     could have produced. See zeroBandLower / zeroBandUpper.
//     - Positive bucket (idx > nlen+1). Position 0-based in
//     PositiveBucketCounts is `idx - nlen - 2`; absolute bucket
//     index is `PositiveOffset + (idx - nlen - 2)`. The bucket
//     covers `(base^idx, base^(idx+1)]`:
//     `value = pow(base, PositiveOffset + (idx - nlen - 2) + fraction)`.
//
// Positive-only parity: distributions with empty NegativeBucketCounts
// and ZeroCount = 0 reduce to the positive-only result (idx = 1 only
// fires when target = 0, which the phi<=0 / total=0 short-circuits
// already cover). The `idx = 1 → ZeroThreshold` branch is subsumed by
// the zero-bucket
// linear interpolation: with ZeroCount > 0 and target landing inside
// the zero band, the interpolation returns a value in the clamped band
// — `[0, +ZeroThreshold]` for a positive-only distribution — rather
// than the fixed upper edge.
package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

const (
	hqQuantileBucketsColumn        = "_cerb_hq_buckets"
	hqQuantileCumColumn            = "_cerb_hq_cum"
	hqQuantileIdxColumn            = "_cerb_hq_idx"
	hqQuantileValueIdxColumn       = "_cerb_hq_value_idx"
	hqQuantileFirstPopulatedColumn = "_cerb_hq_first_populated"
	hqQuantileLastPopulatedColumn  = "_cerb_hq_last_populated"
)

type hqNativeHelperColumns struct {
	buckets        string
	cum            string
	idx            string
	valueIdx       string
	firstPopulated string
	lastPopulated  string
}

// emitHistogramQuantileNative renders a chplan.HistogramQuantileNative
// against the OTel-CH exp_histogram schema. The outer QueryBuilder
// projects the GroupBy columns aliased per GroupByAliases, then the
// interpolated quantile as the `Value` column, matching the Sample
// contract the lowering's wrapping Project consumes.
func (e *emitter) emitHistogramQuantileNative(h *chplan.HistogramQuantileNative) error {
	if h.Input == nil {
		return fmt.Errorf("%w: HistogramQuantileNative.Input is nil", ErrUnsupported)
	}
	// ZeroThresholdColumn is intentionally NOT required: the upstream
	// OTel-CH exp-histogram DDL does not persist the OTLP
	// zero_threshold field, so the default schema leaves it empty and
	// the value fragment renders a constant 0 zero-bucket width
	// instead (see writeZt in histogramQuantileNativeValueFrag).
	if h.PositiveBucketCountsColumn == "" || h.PositiveOffsetColumn == "" ||
		h.ScaleColumn == "" || h.ZeroCountColumn == "" ||
		h.NegativeOffsetColumn == "" || h.NegativeBucketCountsColumn == "" ||
		h.CountColumn == "" || h.SumColumn == "" {
		return fmt.Errorf("%w: HistogramQuantileNative requires Scale / ZeroCount / PositiveOffset / PositiveBucketCounts / NegativeOffset / NegativeBucketCounts / Count / Sum column names", ErrUnsupported)
	}
	sub, err := e.subqueryFrag(h.Input)
	if err != nil {
		return err
	}

	// Native quantile interpolation reuses the bucket walk, cumulative
	// counts, stop index, value index, and the first/last populated-bucket
	// positions many times. Keep each one in its own typed derived-query
	// stage so ClickHouse evaluates it once per row. Expanding those
	// expressions at every use pushed the CH 24.8 compatibility floor past
	// its request deadline on shifting native histograms even though the
	// result stayed correct — and, worse, left firstPopulated/lastPopulated
	// (each an arrayFirstIndex/arrayLastIndex walk over the full bucket
	// array) re-derived at every one of their four use sites in
	// histogramQuantileNativeValueFrag instead of once: those two walks
	// were the only ones in this function NOT yet materialized this way,
	// so disabling ClickHouse's own CSE fold for this plan shape (to fix
	// the CH 24.8 timeout above) left their cost unbounded by row count on
	// a real high-cardinality histogram, independent of that setting — the
	// production memory-limit regression this materialization fixes.
	rawW := newHQNativeWriters(h, hqNativeHelperColumns{})
	bucketed := NewQuery().
		Select(Star(), As(rawW.buckets(), hqQuantileBucketsColumn)).
		From(sub)
	cumulated := NewQuery().
		Select(Star(), As(Call("arrayCumSum", Col(hqQuantileBucketsColumn)), hqQuantileCumColumn)).
		From(Subquery(bucketed))
	idxW := newHQNativeWriters(h, hqNativeHelperColumns{
		buckets: hqQuantileBucketsColumn,
		cum:     hqQuantileCumColumn,
	})
	indexed := NewQuery().
		Select(Star(), As(idxW.idx(), hqQuantileIdxColumn)).
		From(Subquery(cumulated))
	valueIdxW := newHQNativeWriters(h, hqNativeHelperColumns{
		buckets: hqQuantileBucketsColumn,
		cum:     hqQuantileCumColumn,
		idx:     hqQuantileIdxColumn,
	})
	withValueIdx := NewQuery().
		Select(Star(), As(valueIdxW.valueIdx(), hqQuantileValueIdxColumn)).
		From(Subquery(indexed))
	popW := newHQNativeWriters(h, hqNativeHelperColumns{
		buckets: hqQuantileBucketsColumn,
	})
	withPopulated := NewQuery().
		Select(Star(),
			As(popW.firstPopulated(), hqQuantileFirstPopulatedColumn),
			As(popW.lastPopulated(), hqQuantileLastPopulatedColumn)).
		From(Subquery(withValueIdx))
	helpers := hqNativeHelperColumns{
		buckets:        hqQuantileBucketsColumn,
		cum:            hqQuantileCumColumn,
		idx:            hqQuantileIdxColumn,
		valueIdx:       hqQuantileValueIdxColumn,
		firstPopulated: hqQuantileFirstPopulatedColumn,
		lastPopulated:  hqQuantileLastPopulatedColumn,
	}

	sb := NewQuery().From(Subquery(withPopulated))
	for i, g := range h.GroupBy {
		expr := g
		alias := ""
		if i < len(h.GroupByAliases) {
			alias = h.GroupByAliases[i]
		}
		sb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	sb.SelectAs(histogramQuantileNativeValueFrag(h, helpers), "Value")
	return e.emitSelect(sb)
}

// histogramQuantileNativeValueFrag returns the Frag rendering the
// per-row quantile-interpolation expression for the exp-histogram path.
// Structure parallels classic-histogram emission (nested if() chain so
// each edge case stays a flat branch with no CTE / CASE WHEN); a
// literal phi is rendered as an inline float literal — query-shape
// parameter, not user data — while a computed phi (h.PhiExpr != nil)
// renders the expression at every phi position with a leading
// `isNaN(phi) → nan` guard. Mirrors classic emission style.
func histogramQuantileNativeValueFrag(h *chplan.HistogramQuantileNative, helpers hqNativeHelperColumns) Frag {
	po := h.PositiveOffsetColumn
	no := h.NegativeOffsetColumn
	w := newHQNativeWriters(h, helpers)

	// `nan` / `-inf` / `inf` are CH-portable shape tokens, not data —
	// InlineLit would quote them, so they ride verbatim (the IfNonZero
	// precedent in builder.go).
	nan := verbatim("nan")
	negInf := verbatim("-inf")
	posInf := verbatim("inf")

	// phi == 0 → the lower edge of the lowest POPULATED bucket, and
	// phi == 1 → the upper edge of the highest populated one. Both walk
	// to a populated bucket rather than to an end of the array, because
	// reference Prometheus's rank walk skips zero-count buckets
	// (quantile.go: `if bucket.Count == 0 { continue }`). A bucket array
	// may carry zeros anywhere — the offsets describe only the leading
	// gap — so an array end can be an interval no observation fell in,
	// and answering from it reports a value the histogram never saw.
	//
	// Both edges are the interpolation formulas further down evaluated
	// at fraction = 0 (lower) / 1 (upper), so each bucket family's
	// geometry stays written down in exactly one place per edge.
	//
	//   phiLow  = if(first <= nlen,   -pow(base, no + (nlen - first) + 1),
	//               if(first = nlen+1, zeroBandLower,
	//                                  pow(base, po + (first - nlen - 2))))
	phiLow := If(
		Lte(w.firstPopulated(), w.nLen()),
		Neg(Call("pow", w.base(), Add(Add(Col(no), Paren(Sub(w.nLen(), w.firstPopulated()))), InlineLit(1)))),
		If(
			Eq(w.firstPopulated(), Add(w.nLen(), InlineLit(1))),
			w.zeroBandLower(),
			Call("pow", w.base(), Add(Col(po), Paren(Sub(Sub(w.firstPopulated(), w.nLen()), InlineLit(2))))),
		),
	)
	//   phiHigh = if(last <= nlen,    -pow(base, no + (nlen - last)),
	//               if(last = nlen+1,  zeroBandUpper,
	//                                  pow(base, po + (last - nlen - 1))))
	phiHigh := If(
		Lte(w.lastPopulated(), w.nLen()),
		Neg(Call("pow", w.base(), Add(Col(no), Paren(Sub(w.nLen(), w.lastPopulated()))))),
		If(
			Eq(w.lastPopulated(), Add(w.nLen(), InlineLit(1))),
			w.zeroBandUpper(),
			Call("pow", w.base(), Add(Col(po), Paren(Sub(Sub(w.lastPopulated(), w.nLen()), InlineLit(1))))),
		),
	)
	// Negative bucket interp (idx <= nlen):
	//   -pow(base, no + (nlen - idx) + 1 - fraction)
	negInterp := Neg(Call(
		"pow",
		w.base(),
		Sub(
			Add(Add(Col(no), Paren(Sub(w.nLen(), w.valueIdx()))), InlineLit(1)),
			w.fraction(),
		),
	))
	// Zero bucket interp (idx = nlen + 1):
	//   zeroBandLower + (zeroBandUpper - zeroBandLower) * fraction
	// The band is the clamped one, so a one-sided distribution never
	// interpolates across the half of `[-zt, +zt]` it cannot have
	// observed — see zeroBandLower / zeroBandUpper.
	zeroInterp := Add(
		w.zeroBandLower(),
		Mul(Paren(Sub(w.zeroBandUpper(), w.zeroBandLower())), w.fraction()),
	)
	// Positive bucket interp (idx > nlen + 1):
	//   pow(base, po + (idx - nlen - 2) + fraction)
	posInterp := Call(
		"pow",
		w.base(),
		Add(
			Add(Col(po), Paren(Sub(Sub(w.valueIdx(), w.nLen()), InlineLit(2)))),
			w.fraction(),
		),
	)

	// Outer chain (Prometheus quantile.go:222-232, 114-119). Out-of-domain
	// phi is checked OUTERMOST, above the empty-histogram case, matching
	// reference's own `q < 0` / `q > 1` / `h.Count == 0` order — cerberus
	// issue #2067. rankBase = 0 ranks against the histogram's STORED
	// Count rather than the bucket-derived total, so an empty histogram
	// answers NaN even when Sum is itself NaN (0 == 0). idx = 0 is the
	// index functions' "not found" sentinel: the walk exhausted every
	// bucket without reaching the rank, which only happens when Sum is
	// NaN and the forced-forward rank exceeds the buckets' combined
	// reach — cerberus issue #2072.
	//
	//   if(phi < 0, -inf,
	//     if(phi > 1, inf,
	//       if(rankBase = 0, nan,
	//         if(phi = 0, phiLow,
	//           if(phi = 1, phiHigh,
	//             if(idx = 0, nan,
	//               if(idx <= nlen, negInterp,
	//                 if(idx = nlen + 1, zeroInterp, posInterp))))))))
	//
	// phi < 0 / phi > 1 are OUT of domain (-Inf / +Inf). phi == 0 and
	// phi == 1 are IN domain and saturate to the smallest-bucket lower
	// edge / largest-bucket upper edge respectively (the same phiLow /
	// phiHigh edges the old saturating branches produced). ClickHouse
	// parses the bare `inf` / `-inf` tokens as Float64.
	core := If(Lt(w.phi(), InlineLit(0)), negInf,
		If(Gt(w.phi(), InlineLit(1)), posInf,
			If(Eq(w.rankBase(), InlineLit(0)), nan,
				If(Eq(w.phi(), InlineLit(0)), phiLow,
					If(Eq(w.phi(), InlineLit(1)), phiHigh,
						If(Eq(w.idx(), InlineLit(0)), nan,
							If(Lte(w.valueIdx(), w.nLen()), negInterp,
								If(Eq(w.valueIdx(), Add(w.nLen(), InlineLit(1))), zeroInterp, posInterp))))))))

	if h.PhiExpr == nil {
		return core
	}
	// Computed phi can be NaN at runtime (PromQL `scalar()` over zero /
	// many series); every comparison branch evaluates false on NaN and
	// the interpolation would walk cum[0] — guard with a leading isNaN →
	// nan branch (Prom's bucketQuantile NaN-phi contract). The literal
	// path skips the wrapper so existing fixtures stay byte-stable.
	return If(Call("isNaN", w.phi()), nan, core)
}

// hqNativeWriters bundles the per-row sub-expression Frag builders the
// native quantile value fragment composes. Each field is a closure
// returning a fresh Frag so a sub-expression re-rendered at multiple
// positions (base / total / idx / fraction — all CSE-folded by CH)
// re-emits its own `?` placeholders, matching the legacy emitter's
// per-position re-emission.
type hqNativeWriters struct {
	phi            func() Frag
	base           func() Frag
	buckets        func() Frag
	cum            func() Frag
	revCum         func() Frag
	rankBase       func() Frag
	sumIsNaN       func() Frag
	idx            func() Frag
	valueIdx       func() Frag
	cumAt          func(offsetMinusOne bool) Frag
	nLen           func() Frag
	pLen           func() Frag
	zt             func() Frag
	zeroBandLower  func() Frag
	zeroBandUpper  func() Frag
	fraction       func() Frag
	firstPopulated func() Frag
	lastPopulated  func() Frag
}

// zeroBandOrigin is the float-zero the one-sided zero-band clamp
// collapses an edge onto. `0.` rides verbatim rather than through
// InlineLit for the same reason w.zt() does when the schema persists no
// zero_threshold: InlineLit canonicalises the value to the integer `0`,
// and mixing it into the band arithmetic makes ClickHouse reconcile
// Int64 against Float64 inside a single if() branch pair. It is a
// CH-portable shape token, not data.
func zeroBandOrigin() Frag { return verbatim("0.") }

// reverseWalkPhi is the phi at which reference Prometheus's rank walk
// flips from the forward bucket iterator to the reverse one
// (promql/quantile.go:243-252, `if math.IsNaN(h.Sum) || q < 0.5`).
// Walking in from the nearer end keeps the accumulated rank small, so
// the subtraction that rebases it into the stop bucket loses fewer
// significant digits.
//
// Reference also takes the forward arm whenever the histogram's Sum is
// NaN, because NaN observations inflate h.Count without landing in any
// bucket — a reverse walk cannot find a percentile whose mass sits
// outside every bucket. See w.rankBase / w.sumIsNaN below: the rank
// walk ranks against the histogram's own stored Count (chplan's
// CountColumn) rather than a bucket-derived total, and w.idx()'s
// direction choice carries the same Sum-is-NaN override reference
// applies (cerberus issue #2072).
const reverseWalkPhi = 0.5

func newHQNativeWriters(h *chplan.HistogramQuantileNative, helpers hqNativeHelperColumns) hqNativeWriters {
	pbc := h.PositiveBucketCountsColumn
	nbc := h.NegativeBucketCountsColumn
	scale := h.ScaleColumn
	zc := h.ZeroCountColumn
	zt := h.ZeroThresholdColumn
	countCol := h.CountColumn
	sumCol := h.SumColumn
	var w hqNativeWriters

	// phi: the computed expression when PhiExpr is set (typically a
	// scalar subquery — CH folds it as a constant), the inline literal
	// (InlineLit float == formatFloat) otherwise.
	w.phi = func() Frag {
		if h.PhiExpr != nil {
			return func(b *Builder) { _ = b.Expr(h.PhiExpr) }
		}
		return InlineLit(h.Phi)
	}
	// base = pow(2, pow(2, -Scale)). Higher scale = finer buckets.
	w.base = func() Frag {
		return Call("pow", InlineLit(2), Call("pow", InlineLit(2), Neg(Col(scale))))
	}
	// buckets = arrayConcat(
	//             arrayReverse(NegativeBucketCounts),
	//             [ZeroCount],
	//             PositiveBucketCounts).
	// The single walk order every index in this file is expressed in.
	// arrayReverse on an empty array yields [], so the walk collapses to
	// the positive-only shape when NegativeBucketCounts is empty.
	w.buckets = func() Frag {
		if helpers.buckets != "" {
			return Col(helpers.buckets)
		}
		return Call("arrayConcat", Call("arrayReverse", Col(nbc)), Array(Col(zc)), Col(pbc))
	}
	w.cum = func() Frag {
		if helpers.cum != "" {
			return Col(helpers.cum)
		}
		return Call("arrayCumSum", w.buckets())
	}
	// revCum is the bucket array accumulated from the TOP:
	// revCum[i] = sum(buckets[i..n]), the count at or above position i.
	// It is the backward arm's running count, and it is ACCUMULATED
	// rather than derived from cum by subtraction so that its rounding
	// matches reference Prometheus's own reverse iterator bit for bit —
	// see revIdx below.
	w.revCum = func() Frag {
		return Call("arrayReverse", Call("arrayCumSum", Call("arrayReverse", w.buckets())))
	}
	// rankBase is the histogram's own stored Count — what reference
	// Prometheus's rank walk ranks against (quantile.go), as opposed to
	// a total re-derived from the bucket arrays. The two agree whenever
	// Sum is finite; they diverge only for a histogram that counted NaN
	// observations, which inflate Count without landing in any bucket
	// (cerberus issue #2072).
	w.rankBase = func() Frag { return Col(countCol) }
	// sumIsNaN reports whether this row's stored Sum is NaN — reference's
	// marker for "counted an observation that fell in no bucket" and the
	// condition that forces the forward rank walk regardless of phi.
	w.sumIsNaN = func() Frag { return Call("isNaN", Col(sumCol)) }
	// rankWalk renders `<fn>(c -> <cmp>, <running>)`, the shape both walk
	// arms share — they differ only in which running-count array they
	// scan, from which end, and with which per-element comparison.
	// Computed phi: wrap the lambda predicate as `(if(<cmp>, 1, 0) = 1)` —
	// CH 24.8 rejects a scalar subquery anywhere in the index function's
	// argument tree with ILLEGAL_COLUMN (see the classic emitter for the
	// full rationale). The literal path keeps the bare comparison
	// (byte-stable fixtures).
	rankWalk := func(fn string, running, cmp Frag) Frag {
		pred := cmp
		if h.PhiExpr != nil {
			pred = Paren(Eq(If(cmp, InlineLit(1), InlineLit(0)), InlineLit(1)))
		}
		return Call(fn, Lambda1("c", pred), running)
	}
	// Forward arm: stop on the first bucket whose cumulative count from
	// the BOTTOM reaches rank = phi * rankBase. Ranking against rankBase
	// (the stored Count) rather than the bucket-derived total is what lets
	// this arm answer "not found" (arrayFirstIndex returns 0) when a
	// NaN-observation histogram's rank exceeds every bucket's reach — see
	// the outer chain's idx = 0 branch.
	fwdIdx := func() Frag {
		return rankWalk("arrayFirstIndex", w.cum(),
			Gte(BareIdent("c"), Paren(Mul(w.phi(), w.rankBase()))))
	}
	// Backward arm: reference accumulates from the TOP and stops on the
	// first bucket at which that running count reaches
	// rank = (1 - phi) * rankBase. revCum[i] IS that running count once
	// the walk has descended to position i, so — revCum being
	// non-increasing in i — the bucket reference stops on is the LAST
	// position whose running count still reaches the rank.
	//
	// The running count is accumulated top-down rather than derived as
	// `total - cum[i-1]`. Those are equal in exact arithmetic, and while
	// the bucket counts are the table's own integers they are equal in
	// floating point too. They are NOT equal once a rate()/increase()
	// window fold has multiplied every bucket by an extrapolation factor:
	// the subtraction then cancels away low-order bits and lands a few
	// ULPs off reference's accumulated value, which moves an EXACT rank
	// tie one bucket down. Across the zero bucket that is a sign flip in
	// the answer, so this arm accumulates in reference's own direction
	// and order and reproduces its running count bit for bit.
	//
	// Keeping the result in the same walk coordinates as the forward arm
	// means every downstream region / interpolation branch reads one
	// index with no remapping.
	revIdx := func() Frag {
		return rankWalk("arrayLastIndex", w.revCum(),
			Gte(BareIdent("c"), Paren(Mul(Paren(Sub(InlineLit(1), w.phi())), w.rankBase()))))
	}
	// idx: the direction-selected stop index (see the file header,
	// step 4). Reference forces the forward arm whenever Sum is NaN,
	// regardless of phi, so that override is checked before the
	// phi-based choice on both the literal and computed paths — a literal
	// phi >= reverseWalkPhi still needs a runtime branch here because
	// Sum-is-NaN is per-ROW data, not query shape.
	w.idx = func() Frag {
		if helpers.idx != "" {
			return Col(helpers.idx)
		}
		if h.PhiExpr == nil {
			if h.Phi < reverseWalkPhi {
				return fwdIdx()
			}
			return If(w.sumIsNaN(), fwdIdx(), revIdx())
		}
		return If(
			Or(w.sumIsNaN(), Lt(w.phi(), InlineLit(reverseWalkPhi))),
			fwdIdx(),
			revIdx(),
		)
	}
	// cum[idx] (offsetMinusOne=false) / cum[idx - 1] (true). idx=1 with
	// the `- 1` form indexes cum[0], which CH evaluates to the array
	// element's default (0) — matches the "no bucket consumed yet"
	// semantics the fraction formula needs.
	w.cumAt = func(offsetMinusOne bool) Frag {
		key := w.idx()
		if offsetMinusOne {
			key = Sub(w.idx(), InlineLit(1))
		}
		return Subscript(w.cum(), key)
	}
	w.nLen = func() Frag { return Call("length", Col(nbc)) }
	w.pLen = func() Frag { return Call("length", Col(pbc)) }
	// valueIdx is the bucket whose geometry and count reference uses for
	// interpolation. Normally it is the rank walk's stop bucket. With a
	// NaN Sum, Prometheus continues the forward iterator after rebasing the
	// rank so it can detect NaN skew; that scan leaves the iterator on its
	// final bucket, which the subsequent interpolation observes.
	w.valueIdx = func() Frag {
		if helpers.valueIdx != "" {
			return Col(helpers.valueIdx)
		}
		lastIterated := If(
			Gt(w.pLen(), InlineLit(0)),
			Add(Add(w.nLen(), InlineLit(1)), w.pLen()),
			If(Gt(Col(zc), InlineLit(0)), Add(w.nLen(), InlineLit(1)), w.nLen()),
		)
		return If(w.sumIsNaN(), lastIterated, w.idx())
	}
	// first / last POPULATED position in the same concatenated walk order
	// `cum` uses, so they index into the identical bucket geometry the
	// interpolation branches read. Reference Prometheus's rank walk skips
	// empty buckets, so a saturating phi (0 / 1) must land on a bucket an
	// observation actually fell in — not on an array end that may be a
	// trailing or leading zero.
	populated := func(fn, helperCol string) func() Frag {
		return func() Frag {
			if helperCol != "" {
				return Col(helperCol)
			}
			return Call(fn, Lambda1("c", Gt(BareIdent("c"), InlineLit(0))), w.buckets())
		}
	}
	w.firstPopulated = populated("arrayFirstIndex", helpers.firstPopulated)
	w.lastPopulated = populated("arrayLastIndex", helpers.lastPopulated)
	// Zero-bucket upper edge. With a configured ZeroThreshold column the
	// edge is the stored per-row value; an empty column name means the
	// physical schema doesn't persist the OTLP zero_threshold (the
	// upstream OTel-CH DDL doesn't) and the zero bucket collapses to a
	// point at 0 — emitted as the CH-portable shape token `0.`.
	w.zt = func() Frag {
		if zt == "" {
			return zeroBandOrigin()
		}
		return Col(zt)
	}
	// The zero bucket's band edges, with reference Prometheus's
	// one-sided clamp applied (promql/quantile.go:263-273):
	//
	//	if !h.UsesCustomBuckets() && bucket.Lower < 0 && bucket.Upper > 0 {
	//	    case len(NegativeBuckets) == 0 && len(PositiveBuckets) > 0:
	//	        bucket.Lower = 0
	//	    case len(PositiveBuckets) == 0 && len(NegativeBuckets) > 0:
	//	        bucket.Upper = 0
	//	}
	//
	// The zero bucket nominally spans `[-ZeroThreshold, +ZeroThreshold]`,
	// but a distribution with no negative buckets recorded nothing below
	// zero, so its zero bucket starts AT zero; the mirror holds for a
	// distribution with no positive buckets. Without the clamp every phi
	// in the lower half of a positive-only histogram's zero bucket
	// answers negative.
	//
	// The predicates read the ARRAY LENGTHS, not "has a non-zero count",
	// because that is what the reference switch tests: an explicitly
	// stored run of zero counts is a bucket span the distribution
	// declares, and it keeps the band two-sided upstream too.
	//
	// When the schema persists no zero_threshold, w.zt() is already the
	// constant 0 and both clamps collapse to it — the band is a point at
	// zero either way, which is why this divergence is invisible on the
	// default OTel-CH exp-histogram DDL.
	w.zeroBandLower = func() Frag {
		return If(
			And(Eq(w.nLen(), InlineLit(0)), Gt(w.pLen(), InlineLit(0))),
			zeroBandOrigin(),
			Neg(w.zt()),
		)
	}
	w.zeroBandUpper = func() Frag {
		return If(
			And(Eq(w.pLen(), InlineLit(0)), Gt(w.nLen(), InlineLit(0))),
			zeroBandOrigin(),
			w.zt(),
		)
	}
	// fraction = (target - cum[idx-1]) / bucketCount(valueIdx), target =
	// phi * rankBase. The numerator is rebased in the rank-walk stop bucket,
	// while the denominator follows valueIdx: for a NaN Sum, reference's
	// trailing annotation scan leaves `bucket` on the final iterated bucket
	// before it divides by bucket.Count.
	w.fraction = func() Frag {
		return Div(
			Paren(Sub(Paren(Mul(w.phi(), w.rankBase())), w.cumAt(true))),
			Subscript(w.buckets(), w.valueIdx()),
		)
	}

	return w
}

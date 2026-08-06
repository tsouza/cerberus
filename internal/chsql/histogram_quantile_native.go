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
//  3. total = cum[length(cum)]; target = phi * total.
//  4. idx = arrayFirstIndex(c -> c >= target, cum) (1-based).
//     Regions: idx ∈ [1, nlen] is the negative-side walk;
//     idx = nlen+1 is the zero bucket; idx > nlen+1 is the positive
//     walk. (nlen = length(NegativeBucketCounts).)
//  5. fraction = (target - cum[idx-1]) / (cum[idx] - cum[idx-1]).
//     ClickHouse returns the array element's default (0) for
//     `cum[0]`, which matches the "no bucket consumed yet" semantics
//     when idx = 1, so the formula needs no explicit guard.
//  6. Edge cases (Prometheus quantile.go:114-119):
//     - total = 0 → NaN.
//     - phi < 0 → -Inf (out of domain).
//     - phi > 1 → +Inf (out of domain).
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
//     - Zero bucket (idx = nlen+1). Linear interpolation between
//     `-ZeroThreshold` and `+ZeroThreshold`:
//     `value = -ZeroThreshold + 2 * ZeroThreshold * fraction`.
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
// the zero band, the interpolation returns a value in
// `[-ZeroThreshold, +ZeroThreshold]` rather than the fixed upper edge.
package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

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
		h.NegativeOffsetColumn == "" || h.NegativeBucketCountsColumn == "" {
		return fmt.Errorf("%w: HistogramQuantileNative requires Scale / ZeroCount / PositiveOffset / PositiveBucketCounts / NegativeOffset / NegativeBucketCounts column names", ErrUnsupported)
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
	sb.SelectAs(histogramQuantileNativeValueFrag(h), "Value")
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
func histogramQuantileNativeValueFrag(h *chplan.HistogramQuantileNative) Frag {
	po := h.PositiveOffsetColumn
	no := h.NegativeOffsetColumn
	w := newHQNativeWriters(h)

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
	//               if(first = nlen+1, -zt,
	//                                  pow(base, po + (first - nlen - 2))))
	phiLow := If(
		Lte(w.firstPopulated(), w.nLen()),
		Neg(Call("pow", w.base(), Add(Add(Col(no), Paren(Sub(w.nLen(), w.firstPopulated()))), InlineLit(1)))),
		If(
			Eq(w.firstPopulated(), Add(w.nLen(), InlineLit(1))),
			Neg(w.zt()),
			Call("pow", w.base(), Add(Col(po), Paren(Sub(Sub(w.firstPopulated(), w.nLen()), InlineLit(2))))),
		),
	)
	//   phiHigh = if(last <= nlen,    -pow(base, no + (nlen - last)),
	//               if(last = nlen+1,  zt,
	//                                  pow(base, po + (last - nlen - 1))))
	phiHigh := If(
		Lte(w.lastPopulated(), w.nLen()),
		Neg(Call("pow", w.base(), Add(Col(no), Paren(Sub(w.nLen(), w.lastPopulated()))))),
		If(
			Eq(w.lastPopulated(), Add(w.nLen(), InlineLit(1))),
			w.zt(),
			Call("pow", w.base(), Add(Col(po), Paren(Sub(Sub(w.lastPopulated(), w.nLen()), InlineLit(1))))),
		),
	)
	// Negative bucket interp (idx <= nlen):
	//   -pow(base, no + (nlen - idx) + 1 - fraction)
	negInterp := Neg(Call(
		"pow",
		w.base(),
		Sub(
			Add(Add(Col(no), Paren(Sub(w.nLen(), w.idx()))), InlineLit(1)),
			w.fraction(),
		),
	))
	// Zero bucket interp (idx = nlen + 1):
	//   -ZeroThreshold + 2 * ZeroThreshold * fraction
	zeroInterp := Add(Neg(w.zt()), Mul(Mul(InlineLit(2), w.zt()), w.fraction()))
	// Positive bucket interp (idx > nlen + 1):
	//   pow(base, po + (idx - nlen - 2) + fraction)
	posInterp := Call(
		"pow",
		w.base(),
		Add(
			Add(Col(po), Paren(Sub(Sub(w.idx(), w.nLen()), InlineLit(2)))),
			w.fraction(),
		),
	)

	// Outer chain (Prometheus quantile.go:114-119):
	//   if(total = 0, nan,
	//     if(phi < 0, -inf,
	//       if(phi > 1, inf,
	//         if(phi = 0, phiLow,
	//           if(phi = 1, phiHigh,
	//             if(idx <= nlen, negInterp,
	//               if(idx = nlen + 1, zeroInterp, posInterp)))))))
	//
	// phi < 0 / phi > 1 are OUT of domain (-Inf / +Inf). phi == 0 and
	// phi == 1 are IN domain and saturate to the smallest-bucket lower
	// edge / largest-bucket upper edge respectively (the same phiLow /
	// phiHigh edges the old saturating branches produced). ClickHouse
	// parses the bare `inf` / `-inf` tokens as Float64.
	core := If(Eq(w.total(), InlineLit(0)), nan,
		If(Lt(w.phi(), InlineLit(0)), negInf,
			If(Gt(w.phi(), InlineLit(1)), posInf,
				If(Eq(w.phi(), InlineLit(0)), phiLow,
					If(Eq(w.phi(), InlineLit(1)), phiHigh,
						If(Lte(w.idx(), w.nLen()), negInterp,
							If(Eq(w.idx(), Add(w.nLen(), InlineLit(1))), zeroInterp, posInterp)))))))

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
	total          func() Frag
	idx            func() Frag
	cumAt          func(offsetMinusOne bool) Frag
	nLen           func() Frag
	zt             func() Frag
	fraction       func() Frag
	firstPopulated func() Frag
	lastPopulated  func() Frag
}

func newHQNativeWriters(h *chplan.HistogramQuantileNative) hqNativeWriters {
	pbc := h.PositiveBucketCountsColumn
	nbc := h.NegativeBucketCountsColumn
	scale := h.ScaleColumn
	zc := h.ZeroCountColumn
	zt := h.ZeroThresholdColumn
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
		return Call("arrayConcat", Call("arrayReverse", Col(nbc)), Array(Col(zc)), Col(pbc))
	}
	w.cum = func() Frag { return Call("arrayCumSum", w.buckets()) }
	// total = cum[length(cum)] — last element of cum.
	w.total = func() Frag {
		return Subscript(w.cum(), Call("length", w.cum()))
	}
	// idx = arrayFirstIndex(c -> c >= phi*total, cum). Computed phi:
	// wrap the lambda predicate as `(if(<cmp>, 1, 0) = 1)` — CH 24.8
	// rejects a scalar subquery anywhere in arrayFirstIndex's argument
	// tree with ILLEGAL_COLUMN (see the classic emitter for the full
	// rationale). The literal path keeps the bare comparison
	// (byte-stable fixtures).
	w.idx = func() Frag {
		cmp := Gte(BareIdent("c"), Paren(Mul(w.phi(), w.total())))
		pred := cmp
		if h.PhiExpr != nil {
			pred = Paren(Eq(If(cmp, InlineLit(1), InlineLit(0)), InlineLit(1)))
		}
		return Call("arrayFirstIndex", Lambda1("c", pred), w.cum())
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
	// first / last POPULATED position in the same concatenated walk order
	// `cum` uses, so they index into the identical bucket geometry the
	// interpolation branches read. Reference Prometheus's rank walk skips
	// empty buckets, so a saturating phi (0 / 1) must land on a bucket an
	// observation actually fell in — not on an array end that may be a
	// trailing or leading zero.
	populated := func(fn string) func() Frag {
		return func() Frag {
			return Call(fn, Lambda1("c", Gt(BareIdent("c"), InlineLit(0))), w.buckets())
		}
	}
	w.firstPopulated = populated("arrayFirstIndex")
	w.lastPopulated = populated("arrayLastIndex")
	// Zero-bucket upper edge. With a configured ZeroThreshold column the
	// edge is the stored per-row value; an empty column name means the
	// physical schema doesn't persist the OTLP zero_threshold (the
	// upstream OTel-CH DDL doesn't) and the zero bucket collapses to a
	// point at 0 — emitted as the CH-portable shape token `0.`.
	w.zt = func() Frag {
		if zt == "" {
			return verbatim("0.")
		}
		return Col(zt)
	}
	// fraction = (target - cum[idx-1]) / (cum[idx] - cum[idx-1]),
	// target = phi * total.
	w.fraction = func() Frag {
		return Div(
			Paren(Sub(Paren(Mul(w.phi(), w.total())), w.cumAt(true))),
			Paren(Sub(w.cumAt(false), w.cumAt(true))),
		)
	}

	return w
}

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
//     observations collapse to the Phase 1 walk
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
//  6. Edge cases:
//     - total = 0 → NaN.
//     - phi <= 0 → lower edge of the lowest bucket:
//     `-pow(base, NegativeOffset + length(Negative))` if any
//     negative observations exist; otherwise 0 (matches Phase 1
//     convention for non-negative distributions).
//     - phi >= 1 → upper edge of the highest bucket:
//     `pow(base, PositiveOffset + length(Positive))` when positive
//     observations exist; else `ZeroThreshold` when zero bucket is
//     non-empty; else `-pow(base, NegativeOffset)` (upper edge of
//     the least-negative bucket).
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
// Phase parity: distributions with empty NegativeBucketCounts and
// ZeroCount = 0 produce the same numeric output as the original
// positive-only emitter (idx = 1 only fires when target = 0, which
// the phi<=0 / total=0 short-circuits already cover). The Phase 1
// `idx = 1 → ZeroThreshold` branch is subsumed by the zero-bucket
// linear interpolation: with ZeroCount > 0 and target landing inside
// the zero band, the interpolation returns a value in
// `[-ZeroThreshold, +ZeroThreshold]` rather than the fixed upper
// edge. See docs/native-histogram-plan.md § Phase 4.
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
	if h.PositiveBucketCountsColumn == "" || h.PositiveOffsetColumn == "" ||
		h.ScaleColumn == "" || h.ZeroCountColumn == "" || h.ZeroThresholdColumn == "" ||
		h.NegativeOffsetColumn == "" || h.NegativeBucketCountsColumn == "" {
		return fmt.Errorf("%w: HistogramQuantileNative requires Scale / ZeroCount / ZeroThreshold / PositiveOffset / PositiveBucketCounts / NegativeOffset / NegativeBucketCounts column names", ErrUnsupported)
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
	sb.SelectAs(histogramQuantileNativeValueFrag(h), "Value")
	e.emitSelect(sb)
	return nil
}

// histogramQuantileNativeValueFrag returns the Frag rendering the
// per-row quantile-interpolation expression for the exp-histogram path.
// Structure parallels classic-histogram emission (nested if() chain so
// each edge case stays a flat branch with no CTE / CASE WHEN); phi is
// rendered as an inline float literal — query-shape parameter, not
// user data. Mirrors classic emission style.
func histogramQuantileNativeValueFrag(h *chplan.HistogramQuantileNative) Frag {
	pbc := h.PositiveBucketCountsColumn
	po := h.PositiveOffsetColumn
	nbc := h.NegativeBucketCountsColumn
	no := h.NegativeOffsetColumn
	scale := h.ScaleColumn
	zc := h.ZeroCountColumn
	zt := h.ZeroThresholdColumn

	return func(b *Builder) {
		phi := formatFloat(h.Phi)
		// base = pow(2, pow(2, -Scale)). Re-rendered inline at each
		// use; CH's planner CSEs. Inline literal `2` and array-literal
		// `[col]` have no typed Frag (no inline-int / array-literal
		// helpers), so the in-package b.writeSQL path is kept for the
		// shape's outer layout; the inner pow/arrayCumSum/arrayReverse
		// function shells use typed Call where they would otherwise
		// duplicate "fn(" + ... + ")" string fragments.
		writeBase := func() {
			b.writeSQL("pow(2, pow(2, -")
			b.Ident(scale)
			b.writeSQL("))")
		}
		// cum = arrayCumSum(arrayConcat(
		//         arrayReverse(NegativeBucketCounts),
		//         [ZeroCount],
		//         PositiveBucketCounts)).
		// arrayReverse on an empty array yields [], so the walk
		// collapses to the Phase 1 shape when NegativeBucketCounts
		// is empty.
		cumBody := func(b *Builder) {
			b.writeSQL("arrayConcat(")
			Call("arrayReverse", Col(nbc))(b)
			b.writeSQL(", [")
			b.Ident(zc)
			b.writeSQL("], ")
			b.Ident(pbc)
			b.writeSQL(")")
		}
		writeCum := func() {
			Call("arrayCumSum", cumBody)(b)
		}
		// total = cum[length(cum)] — last element of cum.
		writeTotal := func() {
			writeCum()
			b.writeSQL("[")
			Call("length", func(b *Builder) { writeCum() })(b)
			b.writeSQL("]")
		}
		// idx = arrayFirstIndex(c -> c >= phi*total, cum).
		// Lambda body uses bound var `c`; Builder.Lambda emits "(c) ->"
		// which drifts vs. "c ->" output, so the in-package writeSQL
		// path is kept here.
		writeIdx := func() {
			b.writeSQL("arrayFirstIndex(c -> c >= (")
			b.writeSQL(phi)
			b.writeSQL(" * ")
			writeTotal()
			b.writeSQL("), ")
			writeCum()
			b.writeSQL(")")
		}
		// cum[idx + offset]. offset is a string fragment like " - 1"
		// or "" — caller-supplied so the same helper covers cum[idx]
		// (offset="") and cum[idx-1] (offset=" - 1"). idx=1 with
		// offset=" - 1" indexes cum[0], which CH evaluates to the
		// array element's default (0) — matches the
		// "no bucket consumed yet" semantics the fraction formula
		// needs.
		writeCumAt := func(offset string) {
			writeCum()
			b.writeSQL("[")
			writeIdx()
			b.writeSQL(offset)
			b.writeSQL("]")
		}
		writeNLen := func() {
			Call("length", Col(nbc))(b)
		}
		writePLen := func() {
			Call("length", Col(pbc))(b)
		}
		// fraction = (target - cum[idx-1]) / (cum[idx] - cum[idx-1]).
		// target = phi * total.
		writeFraction := func() {
			b.writeSQL("((")
			b.writeSQL(phi)
			b.writeSQL(" * ")
			writeTotal()
			b.writeSQL(") - ")
			writeCumAt(" - 1")
			b.writeSQL(") / (")
			writeCumAt("")
			b.writeSQL(" - ")
			writeCumAt(" - 1")
			b.writeSQL(")")
		}

		// Outer chain:
		//   if(total = 0, nan,
		//     if(phi <= 0, <smallest-bucket lower edge>,
		//       if(phi >= 1, <largest-bucket upper edge>,
		//         if(idx <= nlen, <negative interp>,
		//           if(idx = nlen + 1, <zero interp>,
		//             <positive interp>)))))

		// if(total = 0, nan, ...
		b.writeSQL("if(")
		writeTotal()
		b.writeSQL(" = 0, nan, ")
		// if(phi <= 0, if(nlen > 0, -pow(base, no + nlen), 0.0), ...
		b.writeSQL("if(")
		b.writeSQL(phi)
		b.writeSQL(" <= 0, if(")
		writeNLen()
		b.writeSQL(" > 0, -pow(")
		writeBase()
		b.writeSQL(", ")
		b.Ident(no)
		b.writeSQL(" + ")
		writeNLen()
		b.writeSQL("), 0.0), ")
		// if(phi >= 1, <upper edge>, ...
		// upper edge:
		//   if(plen > 0, pow(base, po + plen),
		//      if(zc > 0, zt, -pow(base, no)))
		b.writeSQL("if(")
		b.writeSQL(phi)
		b.writeSQL(" >= 1, if(")
		writePLen()
		b.writeSQL(" > 0, pow(")
		writeBase()
		b.writeSQL(", ")
		b.Ident(po)
		b.writeSQL(" + ")
		writePLen()
		b.writeSQL("), if(")
		b.Ident(zc)
		b.writeSQL(" > 0, ")
		b.Ident(zt)
		b.writeSQL(", -pow(")
		writeBase()
		b.writeSQL(", ")
		b.Ident(no)
		b.writeSQL("))), ")
		// if(idx <= nlen, <negative interp>, ...
		// negative interp:
		//   -pow(base, no + (nlen - idx) + 1 - fraction)
		b.writeSQL("if(")
		writeIdx()
		b.writeSQL(" <= ")
		writeNLen()
		b.writeSQL(", -pow(")
		writeBase()
		b.writeSQL(", ")
		b.Ident(no)
		b.writeSQL(" + (")
		writeNLen()
		b.writeSQL(" - ")
		writeIdx()
		b.writeSQL(") + 1 - ")
		writeFraction()
		b.writeSQL("), ")
		// if(idx = nlen + 1, <zero interp>, <positive interp>)
		// zero interp:
		//   -ZeroThreshold + 2 * ZeroThreshold * fraction
		// positive interp:
		//   pow(base, po + (idx - nlen - 2) + fraction)
		b.writeSQL("if(")
		writeIdx()
		b.writeSQL(" = ")
		writeNLen()
		b.writeSQL(" + 1, -")
		b.Ident(zt)
		b.writeSQL(" + 2 * ")
		b.Ident(zt)
		b.writeSQL(" * ")
		writeFraction()
		b.writeSQL(", pow(")
		writeBase()
		b.writeSQL(", ")
		b.Ident(po)
		b.writeSQL(" + (")
		writeIdx()
		b.writeSQL(" - ")
		writeNLen()
		b.writeSQL(" - 2) + ")
		writeFraction()
		b.writeSQL("))")
		// Close: if(idx=nlen+1), if(idx<=nlen), if(phi>=1), if(phi<=0), if(total=0)
		b.writeSQL("))))")
	}
}

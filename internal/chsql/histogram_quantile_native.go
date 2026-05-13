// histogram_quantile_native.go emits the SQL for
// chplan.HistogramQuantileNative against the OTel-CH exponential
// (native) histogram table.
//
// Algorithm (positive-only):
//
//  1. base = pow(2, pow(2, -Scale)). Higher scale = finer buckets.
//  2. cum = arrayCumSum(arrayConcat([ZeroCount], PositiveBucketCounts)).
//     Prepending ZeroCount means cum[1] = ZeroCount and cum[i>=2] is
//     the running total through the (i-1)-th positive bucket.
//  3. total = cum[length(cum)]; target = phi * total.
//  4. idx = arrayFirstIndex(c -> c >= target, cum) (1-based).
//  5. Edge cases:
//     - total = 0 → NaN.
//     - phi <= 0 → 0 (non-negative observations; matches Prom's
//     classic-histogram p0 convention).
//     - phi >= 1 → pow(base, PositiveOffset + length(PositiveBucketCounts))
//     i.e. the upper edge of the largest positive bucket.
//     - idx = 1 → the quantile lands in the zero bucket; return
//     ZeroThreshold as the safest summary of "we know it's small,
//     we don't know exactly how small".
//  6. Otherwise: bucket position is idx - 2 (0-based offset into
//     PositiveBucketCounts), absolute bucket index is
//     PositiveOffset + (idx - 2). Log-scale linear interpolation
//     inside the bucket:
//     fraction = (target - cum[idx-1]) / (cum[idx] - cum[idx-1])
//     value    = pow(base, PositiveOffset + (idx - 2) + fraction)
//     Identity used: upper / lower = base, so a log-linear walk
//     across the bucket reduces to a single pow() of the
//     fractional bucket index.
//
// Positive-only limitation: distributions with negative observations
// have their negative-side buckets ignored. The result is a quantile
// over the non-negative subset of the distribution, which matches
// the common case (latency / size) and matches Prom's behaviour on
// classic histograms whose buckets are non-negative by convention.
// Extending the emitter to a full positive+zero+negative walk is a
// follow-up; the IR node already carries the Negative* columns so
// the change is local to this file.
package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitHistogramQuantileNative renders a chplan.HistogramQuantileNative
// against the OTel-CH exp_histogram schema. The outer SelectBuilder
// projects the GroupBy columns aliased per GroupByAliases, then the
// interpolated quantile as the `Value` column, matching the Sample
// contract the lowering's wrapping Project consumes.
func (e *emitter) emitHistogramQuantileNative(h *chplan.HistogramQuantileNative) error {
	if h.Input == nil {
		return fmt.Errorf("%w: HistogramQuantileNative.Input is nil", ErrUnsupported)
	}
	if h.PositiveBucketCountsColumn == "" || h.PositiveOffsetColumn == "" ||
		h.ScaleColumn == "" || h.ZeroCountColumn == "" || h.ZeroThresholdColumn == "" {
		return fmt.Errorf("%w: HistogramQuantileNative requires Scale / ZeroCount / ZeroThreshold / PositiveOffset / PositiveBucketCounts column names", ErrUnsupported)
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

	sb := NewSelect().From(sub)
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
	scale := h.ScaleColumn
	zc := h.ZeroCountColumn
	zt := h.ZeroThresholdColumn

	return func(b *Builder) {
		phi := formatFloat(h.Phi)
		// base = pow(2, pow(2, -Scale)). Re-rendered inline at each
		// use; CH's planner CSEs.
		writeBase := func() {
			b.WriteSQL("pow(2, pow(2, -")
			b.Ident(scale)
			b.WriteSQL("))")
		}
		// cum = arrayCumSum(arrayConcat([ZeroCount], PositiveBucketCounts)).
		writeCum := func() {
			b.WriteSQL("arrayCumSum(arrayConcat([")
			b.Ident(zc)
			b.WriteSQL("], ")
			b.Ident(pbc)
			b.WriteSQL("))")
		}
		// total = cum[length(cum)] — last element of cum.
		writeTotal := func() {
			writeCum()
			b.WriteSQL("[length(")
			writeCum()
			b.WriteSQL(")]")
		}
		// idx = arrayFirstIndex(c -> c >= phi*total, cum)
		writeIdx := func() {
			b.WriteSQL("arrayFirstIndex(c -> c >= (")
			b.WriteSQL(phi)
			b.WriteSQL(" * ")
			writeTotal()
			b.WriteSQL("), ")
			writeCum()
			b.WriteSQL(")")
		}
		writeCumAt := func(offset string) {
			writeCum()
			b.WriteSQL("[")
			writeIdx()
			b.WriteSQL(offset)
			b.WriteSQL("]")
		}

		// Outer chain:
		//   if(total = 0, nan,
		//     if(phi <= 0, 0.0,
		//       if(phi >= 1, pow(base, PositiveOffset + length(pbc)),
		//         if(idx = 1, ZeroThreshold,
		//           pow(base, PositiveOffset + (idx - 2) + fraction)))))
		// where fraction = (target - cum[idx-1]) / (cum[idx] - cum[idx-1])
		// and target = phi * total.

		// if(total = 0, nan, ...
		b.WriteSQL("if(")
		writeTotal()
		b.WriteSQL(" = 0, nan, ")
		// if(phi <= 0, 0.0, ...
		b.WriteSQL("if(")
		b.WriteSQL(phi)
		b.WriteSQL(" <= 0, 0.0, ")
		// if(phi >= 1, pow(base, po + length(pbc)), ...
		b.WriteSQL("if(")
		b.WriteSQL(phi)
		b.WriteSQL(" >= 1, pow(")
		writeBase()
		b.WriteSQL(", ")
		b.Ident(po)
		b.WriteSQL(" + length(")
		b.Ident(pbc)
		b.WriteSQL(")), ")
		// if(idx = 1, ZeroThreshold, ...
		b.WriteSQL("if(")
		writeIdx()
		b.WriteSQL(" = 1, ")
		b.Ident(zt)
		b.WriteSQL(", ")
		// Interpolated case: pow(base, po + (idx - 2) + fraction)
		// where fraction = (target - cum[idx-1]) / (cum[idx] - cum[idx-1])
		// and target = phi * total.
		b.WriteSQL("pow(")
		writeBase()
		b.WriteSQL(", ")
		b.Ident(po)
		b.WriteSQL(" + (")
		writeIdx()
		b.WriteSQL(" - 2) + ((")
		b.WriteSQL(phi)
		b.WriteSQL(" * ")
		writeTotal()
		b.WriteSQL(") - ")
		writeCumAt(" - 1")
		b.WriteSQL(") / (")
		writeCumAt("")
		b.WriteSQL(" - ")
		writeCumAt(" - 1")
		b.WriteSQL("))")
		// Close: if(idx=1), if(phi>=1), if(phi<=0), if(total=0)
		b.WriteSQL("))))")
	}
}

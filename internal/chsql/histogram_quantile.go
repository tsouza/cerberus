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
// quantile-interpolation expression. phi is rendered as an inline float
// literal (query-shape parameter, mirrors holtWintersValueExpr's sf / tf
// treatment); bound user data lives on the wrapping Filter / Project,
// not on the per-row arithmetic.
//
// The expression is structured as nested if(...) clauses so the
// edge-case branches stay inlined (no CASE WHEN, no CTEs) — keeps the
// query shape stable for CH's planner.
func histogramQuantileValueFrag(h *chplan.HistogramQuantile) Frag {
	bc := h.BucketCountsColumn
	eb := h.ExplicitBoundsColumn
	// Reusable typed Frags for the function-call shapes; closures below
	// inline these into the if() chain. The non-Call structure (if()
	// nesting, "[idx]" array indexing, inline phi formatFloat literal)
	// keeps the in-package b.writeSQL path — no typed Frag covers those
	// shapes yet, flagged for R6.12.f.
	lengthBC := Call("length", Col(bc))
	lengthEB := Call("length", Col(eb))
	arraySumBC := Call("arraySum", Col(bc))
	arrayCumSumBC := Call("arrayCumSum", Col(bc))
	return func(b *Builder) {
		// Empty histogram → NaN. arrayCumSum on an empty array is empty;
		// `length(cum) = 0` and `total = 0`. Guard the divide-by-zero +
		// the empty path with a single outer if() on total.
		//
		// We emit one binding of phi for the multiply-by-total branch;
		// the phi-bounds short-circuits below bind it again so each
		// branch carries its own `?` placeholder.
		b.writeSQL("if(")
		lengthBC(b)
		b.writeSQL(" = 0, nan, ")
		// total = cum[length(cum)]; cum = arrayCumSum(bc).
		// Outer if: total = 0 → NaN. We materialise `cum` and `total`
		// inline via subexpression rather than CTE to keep the shape flat.
		//
		// Structure:
		//   if(arraySum(bc) = 0, nan,
		//      if(phi <= 0, lowest_bound,
		//         if(phi >= 1, highest_bound,
		//            <interpolation>)))
		//
		// `lowest_bound` for OTel-CH classic histograms is 0 (the lower
		// edge of bucket 1 is conventionally 0 for non-negative
		// observations; matches upstream Prom's p0 behavior).
		// `highest_bound` is the last entry in ExplicitBounds.
		b.writeSQL("if(")
		arraySumBC(b)
		b.writeSQL(" = 0, nan, ")
		b.writeSQL("if(")
		b.writeSQL(formatFloat(h.Phi))
		b.writeSQL(" <= 0, 0.0, ")
		b.writeSQL("if(")
		b.writeSQL(formatFloat(h.Phi))
		b.writeSQL(" >= 1, ")
		b.Ident(eb)
		b.writeSQL("[")
		lengthEB(b)
		b.writeSQL("], ")
		// Interpolation branch.
		// Bind phi for the target multiplier and build the lookup.
		// Using CH's let-like binding by re-evaluating subexprs is
		// cheap (CH's planner CSEs) and keeps the SQL self-contained
		// inside the if() — no CTE needed.
		//
		// target = phi * arraySum(bc)
		// cum = arrayCumSum(bc)
		// idx = arrayFirstIndex(c -> c >= target, cum)
		// If idx = length(cum) (only the +Inf bucket crosses target),
		//   return ExplicitBounds[length(ExplicitBounds)] (highest bound).
		// Otherwise:
		//   bound_lo = idx = 1 ? 0 : ExplicitBounds[idx-1]
		//   bound_hi = ExplicitBounds[idx]
		//   cum_lo   = idx = 1 ? 0 : cum[idx-1]
		//   cum_hi   = cum[idx]
		//   result   = bound_lo + (bound_hi - bound_lo) * (target - cum_lo) / (cum_hi - cum_lo)
		//
		// `idx` is rendered three times in the sub-expression; we
		// recompute it each time rather than CTE-ing. CH CSE folds it.
		// arrayFirstIndex(c -> ..., arrayCumSum(bc)) — the lambda body
		// uses a bound var `c`; Builder.Lambda emits "(c) -> ..." which
		// drifts vs. the existing "c -> ..." output, so the in-package
		// b.writeSQL path is kept for the lambda. Flagged for R6.12.f
		// if/when a parens-less lambda Frag lands. The outer
		// arrayFirstIndex call wraps the cum frag via Call once the
		// lambda is emitted.
		writeIdx := func() {
			b.writeSQL("arrayFirstIndex(c -> c >= (")
			b.writeSQL(formatFloat(h.Phi))
			b.writeSQL(" * ")
			arraySumBC(b)
			b.writeSQL("), ")
			arrayCumSumBC(b)
			b.writeSQL(")")
		}
		writeCumAt := func(offset string) {
			arrayCumSumBC(b)
			b.writeSQL("[")
			writeIdx()
			b.writeSQL(offset)
			b.writeSQL("]")
		}
		writeBoundAt := func(offset string) {
			b.Ident(eb)
			b.writeSQL("[")
			writeIdx()
			b.writeSQL(offset)
			b.writeSQL("]")
		}
		// if(idx = length(cum), highest_bound, interpolate)
		b.writeSQL("if(")
		writeIdx()
		b.writeSQL(" = ")
		Call("length", arrayCumSumBC)(b)
		b.writeSQL(", ")
		b.Ident(eb)
		b.writeSQL("[")
		lengthEB(b)
		b.writeSQL("], ")
		// Interpolate. bound_lo / cum_lo branch on idx = 1.
		// bound_hi = ExplicitBounds[idx]; cum_hi = cum[idx].
		// bound_lo = if(idx = 1, 0, ExplicitBounds[idx - 1]);
		// cum_lo   = if(idx = 1, 0, cum[idx - 1]).
		b.writeSQL("(if(")
		writeIdx()
		b.writeSQL(" = 1, 0.0, ")
		writeBoundAt(" - 1")
		b.writeSQL(") + (")
		writeBoundAt("")
		b.writeSQL(" - if(")
		writeIdx()
		b.writeSQL(" = 1, 0.0, ")
		writeBoundAt(" - 1")
		b.writeSQL(")) * ((")
		b.writeSQL(formatFloat(h.Phi))
		b.writeSQL(" * ")
		arraySumBC(b)
		b.writeSQL(") - if(")
		writeIdx()
		b.writeSQL(" = 1, 0.0, ")
		writeCumAt(" - 1")
		b.writeSQL(")) / (")
		writeCumAt("")
		b.writeSQL(" - if(")
		writeIdx()
		b.writeSQL(" = 1, 0.0, ")
		writeCumAt(" - 1")
		// Three closes: close the `if(idx=1, ...)`, close the
		// `(cum_hi - cum_lo)` paren, and close the outer `(if(idx=1, ..., bl) + ...)`
		// expression wrapper.
		b.writeSQL(")))")
		// Close the if(idx = length(cum), highest, <interp>)
		b.writeSQL(")")
		// Close the if(phi >= 1, …, interpolation)
		b.writeSQL(")")
		// Close the if(phi <= 0, 0.0, …)
		b.writeSQL(")")
		// Close the if(arraySum = 0, nan, …)
		b.writeSQL(")")
		// Close the if(length(bc) = 0, nan, …)
		b.writeSQL(")")
	}
}

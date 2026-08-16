package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file implements the FUSED MULTI-ARM emitter for
// chplan.RangeWindow.Variants — the shape LogQL's `variants(m0, m1, …) of
// ({selector}[r])` lowers to (internal/logql/variants.go).
//
// Without fusion each arm carries its own Scan/Filter subtree, so an N-arm
// query reads the log table N times. That cost is not recoverable at the SQL
// level by sharing a subquery: ClickHouse INLINES a multiply-referenced CTE
// and re-reads the table per reference, so `WITH src AS (…)` measures
// identically to the duplicated form (10.0M read_rows / 794 MiB / 1224 marks
// on a 5M-row table, against the same figures for two independent scans). The
// only shape that reads once is a single aggregation pass that computes every
// arm's value side by side and unpivots afterwards — measured at 5.0M rows /
// 510 MiB / 612 marks for both two and three arms, i.e. flat in N. See
// issue #1501.
//
// The fused shape therefore keeps ONE grouped pass and reduces it once per
// arm:
//
//	SELECT <series>, [anchor_ts, <tsCol>,] `Value`, `__variant__`
//	FROM (
//	  SELECT <series>, [anchor_ts,] window_vals_0, …, window_vals_{N-1}
//	  FROM ( … one groupArray of (ts, distinct_v_0, …) per (series[, anchor]) … )
//	)
//	ARRAY JOIN [<reduce_0>, …, <reduce_{N-1}>] AS `Value`,
//	           [<label_0>, …, <label_{N-1}>] AS `__variant__`
//	WHERE length(window_pairs) >= 1
//
// Result-equivalence (not byte-equivalence) to the per-arm shape is the
// contract, and it holds arm by arm: arm i maps to its distinct-value slot s
// and reads `arrayMap(p -> p.{s+2}, arraySort(p -> (p.1, p.{s+2}),
// window_pairs))`, which is element-for-element the `arrayMap(p -> p.2,
// arraySort(groupArray((ts, v_i))))` the unfused arm builds — same
// membership (one shared window predicate), same ordering key (ts, then that
// arm's own value), and ties under that key hold equal v_i so the projected
// array cannot differ whichever tied element sorts first. The reducer itself
// is the SHARED overTimeArrayValueFrag the single-arm path drives, so no arm
// gets a second implementation of its own semantics.

// variantWindowValsAlias names the per-arm values array in the fused mid
// layer. Arm i reads `window_vals_<i>`.
func variantWindowValsAlias(i int) string {
	return fmt.Sprintf("window_vals_%d", i)
}

// variantMinWindowSamples is the number of samples an arm's window must hold
// before it emits a row. Every *_over_time function drops empty windows
// (Prom's funcSumOverTime / funcCountOverTime and friends all short-circuit
// on zero samples), which is the same `1` the single-arm path passes to
// emitWindowedArray. Every arm shares one window membership, so the guard is
// expressed once over the shared pairs array rather than once per arm.
const variantMinWindowSamples = 1

// checkFusedVariants validates the fused-shape preconditions, returning the
// error the emitter fails closed with. The gate is deliberately strict: a
// shape this emitter cannot render correctly must surface as an error rather
// than silently emit one arm's answer for every arm.
func checkFusedVariants(r *chplan.RangeWindow) error {
	// A single arm has nothing to share a pass with, so the lowering must
	// leave it on the ordinary path rather than route a degenerate fused
	// window here.
	const minFusedArms = 2
	if len(r.Variants) < minFusedArms {
		return fmt.Errorf(
			"%w: RangeWindow.Variants holds %d arm(s), fusion needs %d",
			ErrUnsupported, len(r.Variants), minFusedArms,
		)
	}
	if r.VariantColumn == "" {
		return fmt.Errorf("%w: fused RangeWindow.VariantColumn unset", ErrUnsupported)
	}
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: fused RangeWindow.TimestampColumn unset", ErrUnsupported)
	}
	if r.ValueColumn == "" {
		return fmt.Errorf("%w: fused RangeWindow.ValueColumn unset", ErrUnsupported)
	}
	if r.Identity {
		return fmt.Errorf("%w: fused RangeWindow with Identity", ErrUnsupported)
	}
	// The fused emitter drives only the *_over_time array reducers, which
	// take no scalar parameters and consult no temporality column. A window
	// carrying either describes a shape this emitter does not render, so
	// refuse it rather than emit an answer that silently ignores it.
	if len(r.Scalars) > 0 || len(r.ScalarExprs) > 0 {
		return fmt.Errorf("%w: fused RangeWindow with scalar arguments", ErrUnsupported)
	}
	if r.TemporalityColumn != "" {
		return fmt.Errorf("%w: fused RangeWindow with a temporality column", ErrUnsupported)
	}
	for i, v := range r.Variants {
		if v.ValueColumn == "" {
			return fmt.Errorf("%w: fused RangeWindow arm %d has no ValueColumn", ErrUnsupported, i)
		}
	}
	return nil
}

// emitRangeWindowVariants renders the fused multi-arm window. It routes to
// the matrix shape when the window fans across an anchor grid (OuterRange >
// 0) and to the instant shape otherwise, mirroring emitWindowedArray's own
// discriminator.
func (e *emitter) emitRangeWindowVariants(r *chplan.RangeWindow) error {
	if err := checkFusedVariants(r); err != nil {
		return err
	}
	if r.OuterRange > 0 {
		if r.Step <= 0 {
			return fmt.Errorf("%w: RangeWindow.OuterRange > 0 requires Step > 0", ErrUnsupported)
		}
		return e.emitRangeWindowVariantsMatrix(r)
	}
	return e.emitRangeWindowVariantsInstant(r)
}

// groupArrayVariantTupleFrag renders
// `arraySort(groupArray((<tsCol>, <distinctValCols...>)))` — the shared
// per-window sample array carrying every distinct value alongside the
// timestamp, so one grouped pass feeds all N reducers. The single-arm
// groupArrayPairFrag is the N=1 case of this shape.
func groupArrayVariantTupleFrag(tsCol string, valCols []string) Frag {
	parts := make([]Frag, 0, len(valCols)+1)
	parts = append(parts, Col(tsCol))
	for _, c := range valCols {
		parts = append(parts, Col(c))
	}
	return Call("arraySort", Call("groupArray", Tuple(parts...)))
}

// variantValsFrag renders one value slot's array out of the shared
// `window_pairs` tuple array:
//
//	arrayMap(p -> tupleElement(p, <slot+2>),
//	         arraySort(p -> (tupleElement(p, 1), tupleElement(p, <slot+2>)),
//	                   window_pairs))
//
// The re-sort is what makes the arm equivalent to its unfused self: the
// single-arm path sorts (ts, value) PAIRS. Re-keying on the selected value
// slot reproduces that ordering exactly, including when several arms share
// the slot, which matters for first_over_time / last_over_time.
func variantValsFrag(pairs Frag, valueSlot int) Frag {
	// +2: tuple slot 1 is the timestamp, so value slot i is tuple slot i+2.
	const firstValueSlot = 2
	slot := int64(valueSlot + firstValueSlot)
	valOf := func(p Frag) Frag { return Call("tupleElement", p, InlineLit(slot)) }
	tsOf := func(p Frag) Frag { return Call("tupleElement", p, InlineLit(int64(1))) }
	sorted := Call(
		"arraySort",
		Lambda1("p", Tuple(tsOf(BareIdent("p")), valOf(BareIdent("p")))),
		pairs,
	)
	return Call("arrayMap", Lambda1("p", valOf(BareIdent("p"))), sorted)
}

// variantArmValueFrags renders each arm's per-window scalar over its own
// values-array alias, reusing the single-arm overTimeArrayValueFrag so the
// two paths cannot drift on any function's semantics.
func variantArmValueFrags(r *chplan.RangeWindow) ([]Frag, error) {
	out := make([]Frag, 0, len(r.Variants))
	for i, v := range r.Variants {
		val, err := overTimeArrayValueFrag(v.Func, BareIdent(variantWindowValsAlias(i)))
		if err != nil {
			return nil, fmt.Errorf("fused RangeWindow arm %d: %w", i, err)
		}
		out = append(out, val)
	}
	return out, nil
}

// fusedVariantValueLayout deduplicates per-sample value columns in first-use
// order and maps every arm to its tuple slot. This is the many-to-one seam
// that lets max_over_time and min_over_time share one projected value.
func fusedVariantValueLayout(r *chplan.RangeWindow) ([]string, []int) {
	cols := make([]string, 0, len(r.Variants))
	slots := make([]int, 0, len(r.Variants))
	seen := make(map[string]int, len(r.Variants))
	for _, v := range r.Variants {
		slot, ok := seen[v.ValueColumn]
		if !ok {
			slot = len(cols)
			seen[v.ValueColumn] = slot
			cols = append(cols, v.ValueColumn)
		}
		slots = append(slots, slot)
	}
	return cols, slots
}

// selectVariantVals projects one `window_vals_<i>` alias per arm off the
// shared `window_pairs` tuple array.
func selectVariantVals(q *QueryBuilder, r *chplan.RangeWindow, armSlots []int) {
	for i := range r.Variants {
		q.Select(As(
			variantValsFrag(BareIdent("window_pairs"), armSlots[i]),
			variantWindowValsAlias(i),
		))
	}
}

// arrayJoinVariants attaches the lockstep unpivot: the i-th element of the
// values array and the i-th element of the labels array land on the same
// output row, turning one row per (series[, anchor]) carrying N values into N
// rows each carrying one value and its arm's label. This is the clause form
// (contrast the `arrayJoin(...)` scalar function, which cross-products
// independent arrays) — the same shape range_window_grid_native.go uses to unpivot
// its grid.
func arrayJoinVariants(q *QueryBuilder, r *chplan.RangeWindow, armValues []Frag) {
	labels := make([]Frag, 0, len(r.Variants))
	for _, v := range r.Variants {
		labels = append(labels, Lit(v.Label))
	}
	q.ArrayJoin(
		As(Array(armValues...), r.ValueColumn),
		As(Array(labels...), r.VariantColumn),
	)
}

// emitRangeWindowVariantsInstant renders the fused shape for a single
// evaluation anchor, mirroring emitWindowedArray's four layers with the
// per-arm value arrays replacing its single `window_vals`.
func (e *emitter) emitRangeWindowVariantsInstant(r *chplan.RangeWindow) error {
	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}
	armValues, err := variantArmValueFrags(r)
	if err != nil {
		return err
	}
	valueColumns, armSlots := fusedVariantValueLayout(r)

	// Innermost SELECT — one sorted (ts, distinct_v_0, …) array per series.
	innermost := NewQuery()
	innermost.Select(groupFrags...)
	innermost.Select(As(
		groupArrayVariantTupleFrag(r.TimestampColumn, valueColumns),
		"series_array",
	))
	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innermost.From(innerSub)
	innermost.GroupBy(groupFrags...)
	// Same fail-closed scan bound the single-arm path takes: without it the
	// groupArray materialises the full per-series retention.
	if err := pushInstantScanBound(innermost, r, end, rangeNS); err != nil {
		return err
	}

	// Inner-middle SELECT — arrayFilter to the (end-range, end] window. The
	// filter reads only the tuple's timestamp slot, so it is arity-agnostic.
	innerMid := NewQuery().From(innermost.Frag())
	innerMid.Select(groupFrags...)
	innerMid.Select(As(windowFilterPairsFrag(end, rangeNS), "window_pairs"))

	// Middle SELECT — one values array per arm, plus the shared pairs array
	// the empty-window guard reads.
	mid := NewQuery().From(innerMid.Frag())
	mid.Select(groupFrags...)
	mid.Select(Col("window_pairs"))
	selectVariantVals(mid, r, armSlots)

	// Outer SELECT — reduce once per arm and unpivot.
	outer := NewQuery().From(mid.Frag())
	outer.Select(groupFrags...)
	outer.Select(Col(r.ValueColumn))
	outer.Select(Col(r.VariantColumn))
	arrayJoinVariants(outer, r, armValues)
	outer.Where(windowLenAtLeastFrag("window_pairs", variantMinWindowSamples))

	return e.emitSelect(outer)
}

// emitRangeWindowVariantsMatrix renders the fused shape across the anchor
// grid, mirroring emitWindowedArrayMatrix: the sample-side fanout puts each
// row in only the windows that cover it, the regroup rebuilds the shared
// per-(series, anchor) tuple array, and the outer layer reduces once per arm
// and unpivots.
func (e *emitter) emitRangeWindowVariantsMatrix(r *chplan.RangeWindow) error {
	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	stepNS := r.Step.Nanoseconds()
	numAnchors := r.OuterRange.Nanoseconds()/stepNS + 1
	end, numAnchors = stepAlignGrid(r, end, stepNS, numAnchors)

	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}
	armValues, err := variantArmValueFrags(r)
	if err != nil {
		return err
	}
	valueColumns, armSlots := fusedVariantValueLayout(r)
	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innerSub, srcTs := fanoutTsSource(innerSub, r.TimestampColumn)

	// Sample-fanout SELECT — one row per (sample, covered anchor), carrying
	// every distinct value so the regroup can build one shared tuple array.
	fanout := NewQuery().From(innerSub)
	fanout.Select(groupFrags...)
	fanout.Select(Col(srcTs))
	for _, c := range valueColumns {
		fanout.Select(Col(c))
	}
	fanout.Select(As(
		sampleAnchorFanoutFrag(end, Col(srcTs), stepNS, rangeNS, numAnchors),
		"anchor_ts",
	))
	maybePushInnerScanTimeBounds(fanout, r, srcTs, rangeNS)

	// Regroup SELECT — the shared per-(series, anchor) sample array.
	regroup := NewQuery().From(fanout.Frag())
	regroup.Select(groupFrags...)
	regroup.Select(Col("anchor_ts"))
	regroup.Select(As(
		groupArrayVariantTupleFrag(srcTs, valueColumns),
		"window_pairs",
	))
	regroupKeys := make([]Frag, 0, len(groupFrags)+1)
	regroupKeys = append(regroupKeys, groupFrags...)
	regroupKeys = append(regroupKeys, Col("anchor_ts"))
	regroup.GroupBy(regroupKeys...)

	// Middle SELECT — per-arm values arrays per (series, anchor).
	mid := NewQuery().From(regroup.Frag())
	mid.Select(groupFrags...)
	mid.Select(Col("anchor_ts"))
	mid.Select(Col("window_pairs"))
	selectVariantVals(mid, r, armSlots)

	// Outer SELECT — reduce once per arm and unpivot. anchor_ts is also
	// surfaced under the schema timestamp column so a wrapping Aggregate's
	// per-step GROUP BY resolves, exactly as the single-arm matrix does.
	outer := NewQuery().From(mid.Frag())
	outer.Select(groupFrags...)
	outer.Select(Col("anchor_ts"))
	if r.TimestampColumn != "anchor_ts" {
		outer.Select(As(gridAnchorFrag(r), r.TimestampColumn))
	}
	outer.Select(Col(r.ValueColumn))
	outer.Select(Col(r.VariantColumn))
	arrayJoinVariants(outer, r, armValues)
	outer.Where(windowLenAtLeastFrag("window_pairs", variantMinWindowSamples))

	return e.emitSelect(outer)
}

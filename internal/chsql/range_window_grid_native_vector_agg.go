package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// nativeGridVectorAggFn maps the five element-wise-correct PromQL vector
// aggregations chplan.RangeWindowGridNativeVectorAgg supports to ClickHouse's
// `-ForEach` combinator form. Empirically verified (not merely documented —
// the CH docs do not specify this) against a real ClickHouse 26.5.1.1
// substrate; see range_window_grid_native_vector_agg_chdb_test.go and the
// chplan node's own doc for the six specific behaviours proven.
//
// The plain (non-OrNull) forms are used throughout, deliberately: a probe
// comparing sumOrNullForEach against sumForEachOrNull found the two
// combinators answer IDENTICALLY for every case this emitter needs (an
// all-NULL position, a zero-row group) because the array's own Nullable
// element type already makes plain sumForEach report NULL wherever OrNull
// would. Composing an OrNull combinator on top would add no observable
// behaviour, so the simpler form is what ships.
var nativeGridVectorAggFn = map[chplan.Fn]string{
	chplan.FnSum:   "sumForEach",
	chplan.FnMin:   "minForEach",
	chplan.FnMax:   "maxForEach",
	chplan.FnAvg:   "avgForEach",
	chplan.FnCount: "countForEach",
}

// countForEachAbsentSentinel is the literal countForEach reports at a
// position no contributing series has a value at — empirically confirmed
// (range_window_grid_native_vector_agg_chdb_test.go,
// TestForEachCombinator_CountAllNullPositionIsZeroNotNull) to be a real
// Array(UInt64) 0, never a NULL, because countForEach's result carries no
// Nullable wrapper at all. Named because it means something specific (the
// family's absent-point sentinel wearing a different type than the other
// four Fns' NULL) rather than being read as a literal "zero count".
const countForEachAbsentSentinel = 0

// emitRangeWindowGridNativeVectorAgg renders a
// chplan.RangeWindowGridNativeVectorAgg — the ForEach-combinator narrowing
// that folds an eligible outer PromQL vector aggregation directly into an
// eligible RangeWindowGridNative's own pre-explode per-series grid, so only
// the ALREADY-COMBINED per-output-series grid is exploded, once, at the very
// end (cerberus issue #2763). Shown for `sum by (job) (rate(m[5m]))`; min/
// max/avg swap only the combinator name, and count additionally swaps its
// value expression and its explode filter (see below):
//
//	SELECT job AS gkey_0, anchor_ts, anchor_ts AS TimeUnix,
//	       toFloat64(assumeNotNull(grid_val)) AS Value
//	FROM (
//	  SELECT job AS gkey_0,
//	         sumForEach(grid) AS grid,
//	         timeSeriesRange(<start>, <end>, <step_s>) AS grid_ts
//	  FROM (
//	    SELECT Attributes, timeSeriesRateToGrid(<start>, <end>, <step_s>, <window_s>)(<ts>, <val>) AS grid,
//	           timeSeriesRange(<start>, <end>, <step_s>) AS grid_ts
//	    FROM (<inner Scan/Filter>)
//	    GROUP BY Attributes
//	  )
//	  GROUP BY job
//	)
//	ARRAY JOIN grid AS grid_val, grid_ts AS anchor_ts
//	WHERE grid_val IS NOT NULL
//
// The bottom two levels are [emitter.nativeGridArrayLevel] — byte-for-byte
// the SAME per-series grid assembly [emitRangeWindowGridNative] itself
// builds on, two-level or three-level (Recollapse) exactly as there. The
// middle level is this node's own addition: GROUP BY the OUTER by/without
// key (r.GroupBy) directly over that per-series (grid, grid_ts) row,
// combining every contributing series' still-array-shaped grid with the
// `-ForEach` combinator instead of exploding to (series, anchor) rows first.
// grid_ts is projected by RECOMPUTING the identical pure timeSeriesRange(...)
// expression the array level itself computed — a function of the query's
// fixed (Start, End, Step), not of any row — rather than reading it back out
// of a combined row, which is what makes "every combined row shares one
// grid" a structural proof (nothing to assert at the SQL level) rather than
// a runtime check; see the chplan node's own doc for the fuller argument.
//
// Composition with Input.Func's post-processing (e.g. increase's
// multiply-by-range-seconds) happens AFTER the ForEach combine, at this
// node's own outer level — exactly where [emitRangeWindowGridNative]'s own
// outer level applies it — which is exact, not approximate, because Range
// seconds is a positive constant shared by every combined row: see the
// chplan node's own doc for the per-Fn distributivity argument.
//
// Fn == FnCount is the one shape needing a DIFFERENT value expression and
// filter than the other four. countForEach's result is Array(UInt64) — no
// Nullable wrapper — so an absent point (every contributing series NULL at
// that position) renders as a literal 0, never NULL. A real Prometheus
// count() is always >= 1 when a point is present at all, so this emitter
// filters that 0 to an absent row itself (`grid_val != 0`) rather than
// reusing the `IS NOT NULL` filter the other four Fns share — an all-present
// but genuinely-zero-valued rate/min/max/avg cell is a real, keepable
// answer, but a count() cell can never legitimately be 0, so the two filters
// are not interchangeable.
func (e *emitter) emitRangeWindowGridNativeVectorAgg(r *chplan.RangeWindowGridNativeVectorAgg) error {
	nativeGrid, ok := r.Input.(*chplan.RangeWindowGridNative)
	if !ok {
		return fmt.Errorf("%w: RangeWindowGridNativeVectorAgg.Input must be *RangeWindowGridNative, got %T", ErrUnsupported, r.Input)
	}
	foreachFn, ok := nativeGridVectorAggFn[r.Fn]
	if !ok {
		return fmt.Errorf("%w: RangeWindowGridNativeVectorAgg.Fn %q (supported: sum, min, max, avg, count)", ErrUnsupported, r.Fn)
	}
	if len(r.GroupBy) != len(r.GroupByAliases) {
		return fmt.Errorf("%w: RangeWindowGridNativeVectorAgg.GroupBy has %d expr(s) but GroupByAliases has %d",
			ErrUnsupported, len(r.GroupBy), len(r.GroupByAliases))
	}
	if r.AnchorAlias == "" {
		return fmt.Errorf("%w: RangeWindowGridNativeVectorAgg requires AnchorAlias", ErrUnsupported)
	}

	arrayLevel, levelKeyFrags, _, gridTS, err := e.nativeGridArrayLevel(nativeGrid)
	if err != nil {
		return err
	}

	// When nativeGrid carries a Recollapse (cerberus issue #2888),
	// arrayLevel is the three-level merge query, whose own SELECT list no
	// longer carries a column under the shaping tower's ORIGINAL name (e.g.
	// "Attributes") — it was hoisted and renamed to
	// nativeShapedKeyAlias(i) so the merge GROUP BY runs at the pre-hoist
	// cost. r.GroupBy is the OUTER PromQL by/without key expression set
	// this node itself evaluates (e.g. Attributes['job']), written against
	// that ORIGINAL name, so rendering it directly over arrayLevel would
	// resolve against a row that no longer carries the column —
	// ClickHouse rejects it as UNKNOWN_IDENTIFIER. levelKeyFrags is
	// exactly the restoration [emitRangeWindowGridNative] itself already
	// relies on: every Recollapse-hoisted key renamed back to its original
	// alias, every non-shaped key passed through verbatim. Interposing one
	// extra level that projects levelKeyFrags alongside the still-array-
	// shaped grid column reconstructs a row r.GroupBy can be evaluated
	// against. In the non-Recollapse case levelKeyFrags IS the same
	// groupFrags arrayLevel's own inner SELECT already rendered, so the
	// extra level is skipped and the emitted SQL is byte-for-byte
	// unchanged from before this fix.
	groupBySource := arrayLevel
	if len(nativeGrid.Recollapse) != 0 {
		renamed := NewQuery().From(arrayLevel.Frag())
		renamed.Select(levelKeyFrags...)
		renamed.Select(Col(nativeGridArrayAlias))
		groupBySource = renamed
	}

	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	// Vector-agg SELECT — see the doc above for why grid_ts is recomputed
	// rather than aggregated. GROUP BY references each key's OWN alias
	// rather than re-rendering groupFrags a second time: a groupFrags
	// element that binds an arg (e.g. Attributes['dc'], a map-key lookup —
	// unlike RangeWindowGridNative's own typically-bare-ColumnRef GroupBy)
	// is a [verbatim] Frag whose captured SQL text was already emitted, in
	// the SELECT list above, with its arg appended to e.args exactly once;
	// rendering that same Frag a second time (mirrors groupKeyFrags's own
	// alias-over-re-render choice, internal/chsql/range_window.go) would
	// repeat the literal `?` in the output SQL with no second arg behind
	// it — go-sqlbuilder's own "not enough args when interpolating".
	// Referencing the alias is also cheaper SQL: ClickHouse groups on a
	// SELECT-list alias natively, so the key expression is evaluated once.
	groupByAliasFrags := make([]Frag, len(r.GroupByAliases))
	outerKeyFrags := make([]Frag, len(r.GroupByAliases))
	for i, alias := range r.GroupByAliases {
		groupByAliasFrags[i] = Col(alias)
		outerKeyFrags[i] = As(Col(alias), alias)
	}

	vecAgg := NewQuery().From(groupBySource.Frag())
	for i, gf := range groupFrags {
		vecAgg.SelectAs(gf, r.GroupByAliases[i])
	}
	vecAgg.Select(As(Call(foreachFn, Col(nativeGridArrayAlias)), nativeGridArrayAlias))
	vecAgg.Select(As(gridTS, nativeGridTSAlias))
	vecAgg.GroupBy(groupByAliasFrags...)

	// Outer SELECT — explode the ALREADY-COMBINED grid once, drop the
	// per-Fn "absent" sentinel (NULL for sum/min/max/avg, literal 0 for
	// count), and surface the anchor under both the bare alias and
	// r.AnchorAlias (mirrors emitRangeWindowGridNative's own dual anchor
	// projection, substituting r.AnchorAlias for the schema TimestampColumn
	// — this node has no TimestampColumn of its own since its caller,
	// internal/promql's lowerAggregate, reads the anchor back under
	// AnchorAlias exactly as it reads an ordinary Aggregate's own per-step
	// bucket-alias group key).
	outer := NewQuery().From(vecAgg.Frag())
	outer.Select(outerKeyFrags...)
	outer.Select(As(nativeAnchorTimestampFrag(), RangeWindowAnchorAlias))
	if r.AnchorAlias != RangeWindowAnchorAlias {
		outer.Select(As(nativeAnchorTimestampFrag(), r.AnchorAlias))
	}

	var filter Frag
	if r.Fn == chplan.FnCount {
		outer.Select(As(Call("toFloat64", Col(nativeGridValAlias)), nativeGrid.ValueColumn))
		filter = Neq(Col(nativeGridValAlias), InlineLit(int64(countForEachAbsentSentinel)))
	} else {
		outer.Select(As(nativeGridValueExprFor(nativeGrid.Func, nativeGrid.Range.Seconds()), nativeGrid.ValueColumn))
		filter = IsNotNull(Col(nativeGridValAlias))
	}
	outer.ArrayJoin(
		As(Col(nativeGridArrayAlias), nativeGridValAlias),
		As(Col(nativeGridTSAlias), RangeWindowAnchorAlias),
	)
	outer.Where(filter)

	return e.emitSelect(outer)
}

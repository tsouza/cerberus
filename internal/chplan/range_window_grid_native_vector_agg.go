package chplan

// RangeWindowGridNativeVectorAgg folds an element-wise-correct outer PromQL
// vector aggregation (`sum`/`min`/`max`/`avg`/`count` by/without) INTO an
// eligible native per-series grid (RangeWindowGridNative), instead of
// exploding that grid to (series, anchor) rows first and re-aggregating with
// a second, blocking GROUP BY — the shape cerberus issue #2763 describes as
// the second axis of the #2651 hard cliff.
//
// Background (the shape this node replaces). `sum by (job) (rate(m[5m]))`
// over the native grid path lowers Input to a RangeWindowGridNative, then
// wraps it in a plain [Aggregate] whose GROUP BY is (the user's by/without
// keys, the per-step anchor) and whose AggFunc is `sum(Value)`. Emitting that
// shape ARRAY JOINs every per-series grid into (series x anchors) rows FIRST
// (RangeWindowGridNative's own emit), then GROUPs BY (shaped-labels,
// anchor_ts) SECOND — so the blocking GROUP BY consumes series x anchors
// rows even though the query only ever asks for outSeries x anchors answers.
//
// The insight this node acts on: summing (or min/max/avg/count-ing) already-
// FINISHED per-series rates across series is exactly element-wise — unlike
// pooling raw samples, which is why this narrowing applies only to sum/min/
// max/avg/count and never to a non-element-wise reducer (stddev/stdvar/
// quantile/topk/bottomk/group, all of which keep lowering through the
// ordinary Aggregate path over the exploded rows). GROUP BY the OUTER
// by/without key against RangeWindowGridNative's own pre-explode per-series
// grid, apply `<Fn>ForEach` (sumForEach / minForEach / maxForEach /
// avgForEach / countForEach — the CH `-ForEach` combinator, AlwaysAvailable,
// already proven in-tree by the classic-histogram bucket-array-sum path,
// internal/chplan.FnSumForEach) to the still-array-shaped grid, and explode
// only the resulting ALREADY-AGGREGATED per-output-series grid once at the
// very end. Rows entering that final explode drop from (series x anchors) to
// (outSeries x anchors).
//
// Empirically verified against a real ClickHouse 26.5.1.1 substrate (chDB;
// see internal/chsql/range_window_grid_native_vector_agg_chdb_test.go) before
// this node existed, because the CH docs do not specify any of it:
//
//   - sumForEach / minForEach / maxForEach / avgForEach skip a NULL array
//     element per POSITION (not per row): a position where SOME rows are
//     NULL and others aren't combines only the non-NULL contributors.
//   - A position that is NULL across EVERY row in the group answers NULL
//     (absent), never 0 — sum/min/max/avg never manufacture a point
//     Prometheus would have dropped.
//   - NaN is NOT skipped like NULL: it propagates through sumForEach exactly
//     like Prometheus arithmetic requires (a NaN sample poisons the sum, it
//     is never silently treated as absent).
//   - avgForEach divides each position by the COUNT OF PRESENT (non-NULL)
//     values AT THAT POSITION, not by the group's total row count — so a
//     position where only some series report a value still averages
//     correctly over just those.
//   - countForEach answers a literal 0 (not NULL) at an all-NULL position,
//     because its Array(UInt64) result carries no Nullable wrapper at all.
//     Since a real Prometheus count() is always >= 1 when a point is present
//     (an absent point never sample-counts as 0), this node's emitter must
//     filter that 0 to an absent row itself — see the emitter's own doc for
//     the resulting per-Fn filter split (`grid_val IS NOT NULL` for
//     sum/min/max/avg vs `grid_val != 0` for count).
//   - sumOrNullForEach and sumForEachOrNull answered IDENTICALLY on every
//     probed case (all-NULL position, zero-row group): plain sumForEach
//     already reports NULL wherever OrNull would, because the array
//     element type is already Nullable. The combinator adds no observable
//     behaviour for this shape, so the emitter uses the PLAIN forms
//     (sumForEach, not sumOrNullForEach / sumForEachOrNull) — the simpler
//     choice the KISS invariant asks for once the two are proven equivalent
//     here, not a claim that the two combinators are equivalent in general.
//
// Composition with Input.Func's own post-processing. `increase` renders
// Input's grid as an UN-multiplied rate array (the x-range multiply happens
// at Input's own OUTER explode level, nativeGridValueExprFor); this node
// combines that same un-multiplied array and defers the multiply to ITS OWN
// final explode. That reordering is exact, not approximate: Range seconds is
// a positive constant shared by every row in the query, and
// sum(x_i)*c == sum(x_i*c), min(x_i)*c == min(x_i*c), max(x_i)*c ==
// max(x_i*c), and avg(x_i)*c == avg(x_i*c) all hold for any positive
// constant c. Count is untouched by the multiply either way (scaling changes
// no element's null-ness). Every timeSeries*ToGrid family member Input can
// carry (rate/increase/changes/resets/deriv/predict_linear/delta/irate/
// idelta) is therefore safe to combine here — the choice of Fn (sum/min/max/
// avg/count) is what a non-element-wise PromQL aggregation excludes, not
// which native range function fed Input.
//
// Ragged grids cannot occur: every row this node's GROUP BY combines shares
// ONE query-level (Start, End, Step) — Input's own grid parameters, which
// are fixed per statement, never per row — so the grid-timestamp axis is not
// data-dependent and every combined row's grid array has the identical
// length in the identical anchor order by construction. The emitter asserts
// this by RECOMPUTING the anchor axis directly (the same pure
// timeSeriesRange(start, end, step) call Input's own array level already
// issued) rather than reading it back out of a combined row — there is
// nothing to assert at the SQL level because the value literally does not
// depend on any row.
//
// Only fires when RangeWindowGridNative (Input's own kind) is ALREADY the
// active lowering: registered as chopt.FeatureTSGridVectorAgg, a pure
// narrowing of chopt.FeatureTSGridRange, hand-wired at the lowering call site
// the same way chopt.FeatureTSGridRecollapse narrows it (internal/promql's
// RangeLowerers.VectorAgg field, consulted directly by lowerAggregate — see
// that field's own doc for why one flag governs every timeSeries*ToGrid
// member rather than nine per-function siblings). The ordinary
// exploded-then-grouped [Aggregate] shape remains the PERMANENT fallback:
// every aggregation this node's Fn set excludes (stddev/stdvar/quantile/
// group/topk/bottomk/count_values/limitk/limit_ratio), every
// non-native-grid Input, and a disabled/unsupported-server feature all keep
// lowering through it unchanged.
//
// Composes with Input.Recollapse (cerberus issue #2888, closed): the shared
// array-assembly level this node's emitter reads Input's per-series row
// from (nativeGridArrayLevel) stops one level short of
// RangeWindowGridNative's own outer explode — the level that restores a
// Recollapse-hoisted shaped key back to its original column name (e.g.
// "Attributes"). The emitter interposes exactly that restoration itself, as
// one extra projection level over the array-assembly level, before
// evaluating GroupBy against it, so a GroupBy expression built against the
// original column resolves correctly whether or not Input carries a
// Recollapse. See the emitter's own doc (internal/chsql) for the query
// shape.
type RangeWindowGridNativeVectorAgg struct {
	// Input is the per-series native grid this node folds an outer vector
	// aggregation into. MUST be a *RangeWindowGridNative — typed as the
	// generic Node interface (matching every other single-child wrapper node
	// in this package, chplan.Aggregate/Filter/Project included) rather than
	// the concrete type, so CloneNode / ReanchorRange / the rewrite-children
	// walk need no special-cased field type; the emitter asserts the
	// concrete type at emit time and fails loudly (ErrUnsupported) rather
	// than emitting a shape that silently reads the wrong columns.
	Input Node

	// Fn is the outer PromQL vector aggregation: FnSum, FnMin, FnMax, FnAvg,
	// or FnCount — the only aggregations proven element-wise-correct over an
	// already-finished per-series grid (see the type doc). The emitter
	// rejects any other value.
	Fn Fn

	// GroupBy / GroupByAliases are the OUTER by/without keys — the exact
	// expressions the now-elided [Aggregate] would have carried as its own
	// GroupBy (chplan.Aggregate.GroupBy / .GroupByAliases), evaluated
	// against Input's own per-series output row (its GroupBy columns, or —
	// with Input.Recollapse set — the Recollapse aliases). They may name
	// FEWER columns than that per-series identity (that coarsening IS the
	// vector aggregation), but every expression must resolve against
	// columns Input's own row already exposes: the SAME expressions the
	// exploded-row Aggregate would have evaluated, one level earlier.
	GroupBy        []Expr
	GroupByAliases []string

	// AnchorAlias names the column this node's own final explode projects
	// the per-row anchor timestamp under, mirroring
	// chplan.RangeBucketFanout.AnchorAlias. It stands in for the elided
	// Aggregate's own per-step GROUP BY key alias (internal/promql's
	// rangeBucketAlias) so the wrapping Sample-shape Project
	// (wrapAggregateForSample) can reference this node's output the same
	// way it references the ordinary Aggregate's.
	AnchorAlias string
}

func (*RangeWindowGridNativeVectorAgg) planNode() {}

func (r *RangeWindowGridNativeVectorAgg) Children() []Node { return []Node{r.Input} }

func (r *RangeWindowGridNativeVectorAgg) Equal(other Node) bool {
	o, ok := other.(*RangeWindowGridNativeVectorAgg)
	if !ok {
		return false
	}
	if r.Fn != o.Fn || r.AnchorAlias != o.AnchorAlias {
		return false
	}
	if len(r.GroupBy) != len(o.GroupBy) {
		return false
	}
	for i := range r.GroupBy {
		if !r.GroupBy[i].Equal(o.GroupBy[i]) {
			return false
		}
	}
	if len(r.GroupByAliases) != len(o.GroupByAliases) {
		return false
	}
	for i := range r.GroupByAliases {
		if r.GroupByAliases[i] != o.GroupByAliases[i] {
			return false
		}
	}
	if r.Input == nil || o.Input == nil {
		return r.Input == o.Input
	}
	return r.Input.Equal(o.Input)
}

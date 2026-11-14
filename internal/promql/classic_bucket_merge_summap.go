package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// classic_bucket_merge_summap.go implements the SUM-fold-only alternative to
// classicBucketMergeShaping's groupArray merge: one ClickHouse sumMap
// aggregate over every row's (bound, that row's CUMULATIVE count at that
// bound) entries, whose key-wise sums ARE the merged per-`le` ladder. It is
// selected ONLY for the SUM fold — see ClassicBucketMergeLowerer
// (lower_strategy.go) and chopt.FeatureClassicBucketMergeSumMap.
//
// # Why each ROW cumulates before the merge, not the GROUP after it
//
// Reference Prometheus never sees a bucket layout: a classic histogram
// reaches it as one already-cumulative float series per `le`, so
// `sum by(le)` sums, at each bound u, ONLY the series that actually report
// `{le=u}` — a producer whose own layout has no u contributes nothing to
// rung u, however close its neighbouring bounds sit. The groupArray merge
// (classicBucketMergeRunsExpr) reproduces that by folding, per distinct
// bound, only the entries of the rows that carry it, and this file has to
// answer identically or it is a second, quieter answer to the same query.
//
// Cumulating each ROW over its own buckets first and keying the result by
// that row's OWN bounds is exactly that semantic, expressed as a key-wise
// sum: the row offers its cumulative count at u to key u for every u IT
// carries, and nothing at a union bound it does not carry. So
//
//	rung(u) = sum over the rows carrying u of that row's cumulative count at u
//
// for ANY mix of layouts. Cumulating the GROUP afterward instead — sumMap
// over per-BUCKET counts, then arrayCumSum along the union — sums, at each
// union bound u, EVERY row's count over its own buckets <= u, including rows
// with no u at all; the two agree only when every row carries every union
// bound (cerberus issue #2817's counterexample: bounds [1,2,3]/counts
// [10,5,0] merged with bounds [1,5]/counts [7,0] answers 1.78 where
// Prometheus answers 5.0).
//
// The per-row reduction is classicBucketRowEntriesExpr — the one the
// groupArray merge and the per-series window stage apply — so a row whose
// stored bounds repeat, are not ascending, or carry a non-finite entry is
// read identically on every path: one entry per distinct finite bound,
// holding every bucket at or below it.
//
// # Keys sumMap would drop
//
// sumMap drops a key only when EVERY value column sums to zero there. The
// cumulative count is exactly zero at every bound below a row's smallest
// observation, which real traffic produces constantly, so each entry also
// carries a count of one (classicBucketSumMapRowEntriesExpr): a key some row
// carries sums to at least one in that column and is never dropped. sumMap's
// keys are therefore the merged layout and its first value column the merged
// ladder, with no separate union construction or lookup.
//
// # The merged ladder still owes the monotonic repair
//
// Rungs folded independently per bound can DIP — a bound only one narrow
// producer carries sits below the rung beneath it — so this ladder, exactly
// like the groupArray merge's, is handed to classicBucketMonotonicExpr
// (Prometheus's ensureMonotonicAndIgnoreSmallDeltas) by
// classicBucketShaping.reshape.
//
// # -0.0 / 0.0 and NaN
//
// sumMap merges -0.0 and 0.0 as ONE key, as the groupArray merge's runs do,
// and the quantile emitter coalesces equal adjacent bounds anyway. A NaN
// count poisons the row's cumulative counts at and above its bucket through
// arrayCumSum — the same reach the groupArray merge's per-row reduction
// gives it — and the repair answers poisoned rungs identically on both
// paths.
//
// # Cost (real ClickHouse 26.6.8.7 and 26.8.36.3)
//
// Measured exactly as classic_bucket_merge_bound.go's calibration —
// `histogram_quantile(0.5, sum by (le) (sum_over_time(<bucket>[5m])))`
// through the production handler, guard lifted, 6 GiB query cap, peak from
// system.query_log — with this feature enabled:
//
//	layout    series x W     entries     peak 26.8    peak 26.6
//	disjoint  25,000 x  50   1,275,000    448.1 MiB    379.4 MiB
//	disjoint  50,000 x  50   2,550,000    603.3 MiB    599.0 MiB
//	disjoint 100,000 x  50   5,100,000   1201.3 MiB   1197.8 MiB
//	shared   100,000 x  50   5,100,000    149.9 MiB    149.8 MiB
//	disjoint  10,000 x 250   2,510,000    602.1 MiB    601.3 MiB
//
// Against the groupArray merge's calibration (classic_bucket_merge_bound.go)
// the two are within 0.1% of each other at 25,000 x 50 disjoint on both
// builds (448.1 against 448.4 MiB on 26.8, 379.4 against 379.8 MiB on
// 26.6). On the larger disjoint layouts sumMap costs 3-7% less per entry on
// 26.8 and 11-16% more on 26.6 (599.0 against 517.5 MiB at 50,000 x 50,
// 1197.8 against 1028.8 MiB at 100,000 x 50, 601.3 against 539.7 MiB at
// 10,000 x 250), and far less on a
// shared layout, whose sumMap state holds one key per distinct bound. Under
// the 1 GiB cap with the default guard, 58,000 x 50 disjoint completes at
// 747.0 / 746.4 MiB (26.8 / 26.6), inside the cap share the ceiling is sized
// for, so [maxClassicBucketMergeEntries] bounds this path too and no second
// ceiling exists. The guard counts the entries before the sumMap aggregate
// reads a row (classicBucketMergeInputGate), so 60,000 x 50 is refused at
// 128.1 / 127.7 MiB and 100,000 x 50 at 246.0 / 245.4 MiB, before the
// aggregate runs.
//
// The feature keeps `AutoSelect: false`; it stays reachable through
// `CERBERUS_CH_OPTIMIZATIONS=classic_bucket_merge_summap`.
const (
	// hqAggSumMapAlias holds the group's sumMap(bounds, counts) result — a
	// Tuple(Array(Float64) keys sorted ascending, Array(Float64) values),
	// with any zero-summed key dropped (see this file's header).
	hqAggSumMapAlias = "_hq_summap"
	// hqAggInfTotalAlias holds the group's +Inf rung: the plain SUM of every
	// contributing row's own full BucketCounts total (paired buckets plus any
	// trailing overflow element), unconditional over every row — mirrors
	// classicBucketRowTotalExpr's fold(infRungs) exactly, just computed as a
	// genuine SQL aggregate over the raw column instead of arraySum over a
	// groupArray of per-row totals.
	hqAggInfTotalAlias = "_hq_inf_total"
)

// paramBucketIndex indexes one row's own ExplicitBounds / BucketCounts.
const paramBucketIndex = "i"

// classicBucketFiniteBoundKeptIndicesExpr returns the 1-based positions in
// eb (an ExplicitBounds-shaped expression) whose bound is finite (see
// classicBucketFiniteExpr), in ascending order. Shared by
// classicBucketFiniteBoundsRestriction (the bare-selector path) and
// classicBucketSumMapRowArgs (this file): dropping index i from
// ExplicitBounds must drop BucketCounts[i] in lockstep, and both paths pair
// the two arrays by this same index set.
func classicBucketFiniteBoundKeptIndicesExpr(eb chplan.Expr) chplan.Expr {
	n := &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{eb}}
	return &chplan.FuncCall{Fn: chplan.FnArrayFilter, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramBucketIndex},
			Body: classicBucketFiniteExpr(&chplan.Subscript{
				Container: eb, Key: &chplan.BareIdent{Name: paramBucketIndex},
			}),
		},
		&chplan.FuncCall{Fn: chplan.FnRange, Args: []chplan.Expr{&chplan.LitInt{V: 1}, addExpr(n, &chplan.LitInt{V: 1})}},
	}}
}

// classicBucketSumMapRowEntriesExpr renders the per-row argument of the
// sumMap aggregate: tuple(bounds, cumulative counts, ones), one entry per
// distinct finite bound the raw row carries — classicBucketRowEntriesExpr's
// reduction, the same one the groupArray fold and the per-series window
// stage apply, so all three read a row identically — plus a count of one per
// entry.
//
// The count column is what keeps the merged keys equal to the union layout.
// sumMap drops a key only when EVERY value column sums to zero there, and a
// key some row carries always counts at least that row, so no carried bound
// is dropped — not even one below the smallest observation, whose cumulative
// count is exactly zero. sumMap's key-wise result is therefore the merged
// layout and its first value column the merged ladder, with no separate
// union construction or per-bound lookup.
func classicBucketSumMapRowEntriesExpr(s schema.Metrics) chplan.Expr {
	ones := func(n chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnArrayWithConstant, Args: []chplan.Expr{n, &chplan.LitInt{V: 1}}}
	}
	return classicBucketRowEntriesExpr(
		&chplan.ColumnRef{Name: s.ExplicitBoundsColumn},
		&chplan.ColumnRef{Name: s.BucketCountsColumn},
		ones,
	)
}

// classicBucketRowTotalColumnExpr renders ONE row's +Inf rung — its total
// observation count, paired buckets plus any trailing overflow — straight
// off the raw BucketCountsColumn, mirroring classicBucketRowTotalExpr's
// arraySum(paramRowCounts) but reading the column directly rather than a
// groupArray-bound lambda parameter: hqAggInfTotalAlias is a plain sum()
// over this per-row expression, not a fold over a collected array.
func classicBucketRowTotalColumnExpr(s schema.Metrics) chplan.Expr {
	return toFloat64Expr(&chplan.FuncCall{
		Fn:   chplan.FnArraySum,
		Args: []chplan.Expr{&chplan.ColumnRef{Name: s.BucketCountsColumn}},
	})
}

// classicBucketSumMapMergedExpr renders the group's merged layout and its
// not-yet-repaired cumulative ladder as tuple(bounds, ladder), straight off
// the sumMap result: its keys are the union layout (see
// classicBucketSumMapRowEntriesExpr for why no carried key is dropped), its
// first value column the per-bound ladder, and the trailing +Inf rung is
// hqAggInfTotalAlias, an independent unconditional sum.
//
// This is classicBucketMergeRunsExpr's counterpart and stands in exactly the
// same place: classicBucketShaping.reshape aliases it to hqAggMergedAlias and
// hands its ladder to classicBucketMonotonicExpr, because per-bound rungs
// folded independently can dip. See this file's header.
func classicBucketSumMapMergedExpr() chplan.Expr {
	sm := chplan.Expr(&chplan.ColumnRef{Name: hqAggSumMapAlias})
	keys := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{sm, &chplan.LitInt{V: runsTupleBounds}}}
	vals := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{sm, &chplan.LitInt{V: runsTupleValues}}}
	ladder := &chplan.FuncCall{Fn: chplan.FnArrayConcat, Args: []chplan.Expr{
		vals,
		&chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: hqAggInfTotalAlias}}},
	}}
	return &chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{keys, ladder}}
}

// classicBucketMergeShapingSumMap is the sumMap-based shaping for the
// SUM fold only — see this file's header. Selected by
// NativeClassicBucketMergeLowerer (lower_strategy.go) when
// chopt.FeatureClassicBucketMergeSumMap is enabled; every other fold keeps
// using classicBucketMergeShaping unconditionally.
func classicBucketMergeShapingSumMap(s schema.Metrics) classicBucketShaping {
	return classicBucketShaping{
		aggs: []chplan.AggFunc{
			{
				Fn:    chplan.FnSumMap,
				Args:  []chplan.Expr{classicBucketSumMapRowEntriesExpr(s)},
				Alias: hqAggSumMapAlias,
			},
			{
				Fn:    chplan.FnSum,
				Args:  []chplan.Expr{classicBucketRowTotalColumnExpr(s)},
				Alias: hqAggInfTotalAlias,
			},
		},
		// fold is never invoked when sumMap is set — classicBucketShaping
		// .reshape swaps in classicBucketSumMapMergedExpr for the ladder it
		// would otherwise build. It is set purely so that reshape's nil
		// check (the argMax newest-row path, which has no layouts to merge)
		// keeps routing this shaping through the merge branch, exactly as
		// the groupArray-fold shaping does.
		fold:   arrayFoldSum,
		sumMap: true,
	}
}

// arrayFoldSum is the SUM classicBucketRungFold — the same reduction
// classicBucketLadderFold returns for parser.SUM / the nil-agg bare-rate
// case. classicBucketMergeShapingSumMap only ever carries the sum fold (the
// dispatcher never selects this shaping for another operator), so this is a
// placeholder satisfying classicBucketShaping.reshape's nil check, never
// actually invoked — reshape builds this shaping's ladder from the sumMap
// aggregate instead of folding collected rungs.
func arrayFoldSum(rungs chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnArraySum, Args: []chplan.Expr{rungs}}
}

// ClassicBucketMergeLowerer decides how the aggregated classic-histogram
// merge stage (both call sites classicBucketMergeShaping / this file's
// classicBucketMergeShapingSumMap build — histogram_quantile.go's
// lowerHistogramQuantileAgg and histogram_quantile_range.go's
// lowerHistogramQuantileClassicAggRange) collects and merges every
// contributing row's bucket layout: the groupArray + per-rung
// arrayFilter-rescan fold (every fold operator), or this file's linear
// sumMap reshape over per-row cumulative counts (SUM fold only,
// chopt.FeatureClassicBucketMergeSumMap).
//
// isSum (histogramAggShape.classicFoldIsSum) is intrinsic query SHAPE, not
// feature state — passing it is how the interface method decides WHETHER
// the sumMap path is even eligible, mirroring how [ClassicHistogramWindowLowerer]
// and its siblings read intrinsic shape off their own input structs. It
// never returns nil.
type ClassicBucketMergeLowerer interface {
	// LowerClassicBucketMerge returns the classicBucketShaping the merge
	// Aggregate node and its reshape use — the groupArray-fold shaping for
	// fold (every non-sum operator, and the sum fold when the sumMap
	// feature is off), or classicBucketMergeShapingSumMap's shaping when
	// isSum and the feature is on.
	LowerClassicBucketMerge(fold classicBucketRungFold, isSum bool, s schema.Metrics) classicBucketShaping
}

// FanoutClassicBucketMergeLowerer is the concrete DEFAULT
// ClassicBucketMergeLowerer: it always returns the groupArray-fold shaping
// (classicBucketMergeShaping), regardless of isSum.
type FanoutClassicBucketMergeLowerer struct{}

// LowerClassicBucketMerge returns classicBucketMergeShaping(fold, s).
func (FanoutClassicBucketMergeLowerer) LowerClassicBucketMerge(
	fold classicBucketRungFold, _ bool, s schema.Metrics,
) classicBucketShaping {
	return classicBucketMergeShaping(fold, s)
}

// NativeClassicBucketMergeLowerer is the boot-wired ClassicBucketMergeLowerer
// that routes the SUM fold onto classicBucketMergeShapingSumMap. cmd/cerberus
// wires it ONLY when chopt resolved the classic_bucket_merge_summap feature
// at boot. A non-sum fold always delegates to the embedded Fallback — the
// sumMap reshape is provably correct only for the sum reduction (see this
// file's header); every other operator's groupArray-fold shaping is
// byte-for-byte unchanged.
type NativeClassicBucketMergeLowerer struct {
	// Fallback is the concrete lowerer for a non-sum fold. Boot wires it to
	// FanoutClassicBucketMergeLowerer{}.
	Fallback ClassicBucketMergeLowerer
}

// LowerClassicBucketMerge returns classicBucketMergeShapingSumMap(s) when
// isSum, or delegates to n.Fallback otherwise.
func (n NativeClassicBucketMergeLowerer) LowerClassicBucketMerge(
	fold classicBucketRungFold, isSum bool, s schema.Metrics,
) classicBucketShaping {
	if !isSum {
		return n.Fallback.LowerClassicBucketMerge(fold, isSum, s)
	}
	return classicBucketMergeShapingSumMap(s)
}

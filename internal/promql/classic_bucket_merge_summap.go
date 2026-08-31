package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// classic_bucket_merge_summap.go implements issue #2756's SUM-fold-only
// alternative to classicBucketMergeShaping's groupArray + per-rung
// arrayFilter rescan (classic_bucket_merge_bound.go's own audited cost
// finding — see that file's header). It replaces the per-rung rescan with
// one ClickHouse sumMap aggregate: a single linear key-wise pass over every
// row's (bound, per-bucket count) pairs, plus arrayCumSum to turn the
// resulting per-bucket sums into the cumulative ladder HistogramQuantile
// expects. It is selected ONLY for the SUM fold — see
// ClassicBucketMergeLowerer (lower_strategy.go) and chopt.FeatureClassicBucketMergeSumMap.
//
// # A real ClickHouse quirk this design works around
//
// sumMap(keys, values) DROPS any key whose summed value across the group is
// exactly zero — confirmed against real chDB execution (sumMap([1,2,3],
// [10,0,5]) over a single row returns keys=[1,3], NOT [1,2,3]; bound 2's
// zero-valued bucket silently vanishes). This is disqualifying on its own:
// ExplicitBounds defines the interpolation GEOMETRY HistogramQuantile walks,
// and any bucket with a zero net count in the merge window — extremely
// common in real traffic — would otherwise silently disappear from the
// node's own output layout.
//
// The fix keeps a SEPARATE, independent union-of-bounds construction (the
// existing linear groupArray + arrayFlatten + arrayDistinct + arraySort —
// classicBucketUnionBoundsExpr, UNCHANGED, and never the quadratic part of
// the old cost: the quadratic term was always the per-rung RESCAN, not this
// one-pass flatten) and reconstructs each union bound's value with an
// indexOf lookup into sumMap's own (compact, W-sized) key/value pair,
// defaulting to zero for a key sumMap dropped:
//
//	arrayConcat([0.0], sm.values)[indexOf(sm.keys, u) + 1]
//
// indexOf returns 0 (not found), and the leading zero pad shifts every
// found position by one, so a miss reads the pad's 0.0 and a hit reads
// sm.values at its true position — one indexOf call, no branch. This costs
// O(W) per union bound (W = merged bucket-ladder width) against sumMap's own
// compact output, never against the T contributing rows — the quadratic
// T-row rescan classic_bucket_merge_bound.go's own doc audited is gone.
//
// # -0.0 / 0.0 key identity
//
// sumMap itself already merges -0.0 and 0.0 as one aggregate key (confirmed:
// sumMap([-0.0,1],[3,4]) unioned with a same-group 0.0-keyed row's own entry
// rather than keeping two). arrayDistinct — the union-bounds construction's
// own dedup step — does NOT (arrayDistinct([-0.0, 0.0]) keeps BOTH,
// confirmed against chDB), so a group whose rows report -0.0 from one row
// and 0.0 from another would otherwise surface as two distinct union rungs.
// indexOf treats -0.0 and 0.0 as equal for lookup purposes either way, so
// BOTH of those rungs would resolve to the SAME sumMap entry — double-
// counting it once per duplicate rung after arrayCumSum. Canonicalising
// every row's zero bound to +0.0 BEFORE either aggregate sees it
// (classicBucketZeroCanonicalExpr) removes the duplicate at the source
// rather than trying to de-duplicate the union afterward.
//
// # NaN propagation (documented, not a bug)
//
// arrayCumSum propagates a NaN forward to EVERY higher rung once it appears
// (confirmed: arrayCumSum([1, nan, 2, 3]) = [1, nan, nan, nan]), whereas the
// old has-filter fold only poisons the rungs a NaN row's own layout
// contains. This is issue #2756's own documented, accepted risk — pinned by
// TestClassicBucketMergeSumMapDifferential's NaN case, not glossed over.
//
// # Heterogeneous bucket layouts (scoped OUT of AutoSelect)
//
// For a HOMOGENEOUS group (every contributing row shares the same
// ExplicitBounds — the overwhelmingly common real shape, and the one this
// issue's own ~50x estimate is calibrated on) this construction is provably
// identical to the has-filter fold: every row carries every union bound, so
// the has-filter is a no-op and both reduce to the same per-bound sum. For a
// HETEROGENEOUS group (rows reporting genuinely different bucket
// boundaries) the two constructions diverge for real: sumMap+arrayCumSum
// sums, at each union bound u, EVERY row's own sub-cumulative count over its
// OWN buckets <= u, while reference Prometheus's sum by(le) — and the
// has-filter fold that reproduces it — only sums rows whose OWN layout
// contains u exactly. A chDB differential probe confirmed a measurable
// divergence on a two-row disjoint-layout fixture. See
// https://github.com/tsouza/cerberus/issues/2817, filed to investigate
// restricting the sumMap path to provably-homogeneous groups. Until that
// lands, chopt.FeatureClassicBucketMergeSumMap ships AutoSelect: false —
// opt-in only, mirroring quantile_prom_histogram's and ts_grid_changes'
// posture for a feature with a proven, real divergence on a specific input
// shape.
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

// classicBucketZeroCanonicalExpr normalises a float bound to +0.0 when it
// compares equal to zero, folding -0.0 into +0.0. See this file's header for
// why: sumMap merges -0.0/0.0 as one key but arrayDistinct (the union-bounds
// dedup) does not, so an unnormalised zero bound can double-count.
func classicBucketZeroCanonicalExpr(v chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{
		&chplan.Binary{Op: chplan.OpEq, Left: v, Right: &chplan.LitFloat{V: 0}},
		&chplan.LitFloat{V: 0},
		v,
	}}
}

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
			Params: []string{"i"},
			Body: classicBucketFiniteExpr(&chplan.Subscript{
				Container: eb, Key: &chplan.BareIdent{Name: "i"},
			}),
		},
		&chplan.FuncCall{Fn: chplan.FnRange, Args: []chplan.Expr{&chplan.LitInt{V: 1}, addExpr(n, &chplan.LitInt{V: 1})}},
	}}
}

// classicBucketSumMapRowArgs renders the per-row (bounds, counts) pair fed
// to BOTH the sumMap aggregate and the union-bounds groupArray: every
// INTERIOR non-finite bound dropped (its paired count dropped in lockstep,
// preserving the #2495 -Inf guard the has-filter path applies at read time
// instead), every finite bound zero-canonicalised, and counts cast to
// Float64 (the ladder is float-domain throughout — see
// classicBucketRowCumulativeExpr's identical reasoning). The trailing
// overflow element BucketCounts may carry (one longer than ExplicitBounds)
// is excluded here — sumMap requires equal-length key/value arrays per row —
// and folded separately into hqAggInfTotalAlias via the RAW, unfiltered
// column instead.
func classicBucketSumMapRowArgs(s schema.Metrics) (bounds, counts chplan.Expr) {
	eb := chplan.Expr(&chplan.ColumnRef{Name: s.ExplicitBoundsColumn})
	bc := chplan.Expr(&chplan.ColumnRef{Name: s.BucketCountsColumn})
	keptIdx := classicBucketFiniteBoundKeptIndicesExpr(eb)

	bounds = &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{"i"},
			Body: classicBucketZeroCanonicalExpr(&chplan.Subscript{
				Container: eb, Key: &chplan.BareIdent{Name: "i"},
			}),
		},
		keptIdx,
	}}
	counts = &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{"i"},
			Body: toFloat64Expr(&chplan.Subscript{
				Container: bc, Key: &chplan.BareIdent{Name: "i"},
			}),
		},
		keptIdx,
	}}
	return bounds, counts
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

// classicBucketSumMapLookupExpr reconstructs the merged per-bucket value at
// union bound u from the group's sumMap result: sm.1 (keys) / sm.2 (values),
// via arrayConcat([0.0], sm.2)[indexOf(sm.1, u) + 1] — see this file's
// header for why a plain subscript into sm.2 at indexOf(sm.1, u) is unsafe
// (indexOf returns 0, an invalid subscript, on a miss — which sumMap's own
// zero-value key drop makes a real, expected case, not a defensive-only
// branch).
func classicBucketSumMapLookupExpr(u chplan.Expr) chplan.Expr {
	sm := chplan.Expr(&chplan.ColumnRef{Name: hqAggSumMapAlias})
	keys := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{sm, &chplan.LitInt{V: 1}}}
	vals := &chplan.FuncCall{Fn: chplan.FnTupleElement, Args: []chplan.Expr{sm, &chplan.LitInt{V: 2}}}
	paddedVals := &chplan.FuncCall{Fn: chplan.FnArrayConcat, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{&chplan.LitFloat{V: 0}}},
		vals,
	}}
	pos := addExpr(
		&chplan.FuncCall{Fn: chplan.FnIndexOf, Args: []chplan.Expr{keys, u}},
		&chplan.LitInt{V: 1},
	)
	return &chplan.Subscript{Container: paddedVals, Key: pos}
}

// classicBucketSumMapLadderExpr renders the group's cumulative per-`le`
// ladder over the union layout via arrayCumSum, plus the trailing +Inf rung
// (hqAggInfTotalAlias, an independent unconditional sum — see that alias'
// doc comment for why it needs no separate "add the last cumulative rung"
// step). Every element the cumsum walks is a non-negative per-bucket delta
// count (or NaN — see this file's header), so the result is monotonically
// non-decreasing by construction: unlike classicBucketMergedLadderExpr's
// independently-folded-per-rung ladder, this one needs no
// classicBucketMonotonicLadderExpr repair pass.
func classicBucketSumMapLadderExpr() chplan.Expr {
	unionBounds := classicBucketUnionBoundsExpr()
	perBucket := &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{Params: []string{paramUnionBound}, Body: classicBucketSumMapLookupExpr(&chplan.BareIdent{Name: paramUnionBound})},
		unionBounds,
	}}
	cumulative := &chplan.FuncCall{Fn: chplan.FnArrayCumSum, Args: []chplan.Expr{perBucket}}
	return &chplan.FuncCall{Fn: chplan.FnArrayConcat, Args: []chplan.Expr{
		cumulative,
		&chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: hqAggInfTotalAlias}}},
	}}
}

// classicBucketMergeShapingSumMap is the sumMap-based shaping for the
// SUM fold only — see this file's header. Selected by
// NativeClassicBucketMergeLowerer (lower_strategy.go) when
// chopt.FeatureClassicBucketMergeSumMap is enabled; every other fold keeps
// using classicBucketMergeShaping unconditionally.
func classicBucketMergeShapingSumMap(s schema.Metrics) classicBucketShaping {
	bounds, counts := classicBucketSumMapRowArgs(s)
	return classicBucketShaping{
		aggs: []chplan.AggFunc{
			{
				Fn:    chplan.FnGroupArray,
				Args:  []chplan.Expr{bounds},
				Alias: hqAggBoundsListAlias,
			},
			{
				Fn:    chplan.FnSumMap,
				Args:  []chplan.Expr{bounds, counts},
				Alias: hqAggSumMapAlias,
			},
			{
				Fn:    chplan.FnSum,
				Args:  []chplan.Expr{classicBucketRowTotalColumnExpr(s)},
				Alias: hqAggInfTotalAlias,
			},
		},
		// fold is never invoked by the sumMap reshape branch — set purely so
		// classicBucketShaping.reshape's nil check (the argMax newest-row
		// path, which has no layouts to merge) keeps routing this shaping
		// through the merge branch, exactly as the groupArray-fold shaping
		// does.
		fold:   arrayFoldSum,
		sumMap: true,
	}
}

// arrayFoldSum is the SUM classicBucketRungFold — the same reduction
// classicBucketLadderFold returns for parser.SUM / the nil-agg bare-rate
// case. classicBucketMergeShapingSumMap only ever carries the sum fold (the
// dispatcher never selects this shaping for another operator), so this is a
// placeholder satisfying classicBucketShaping.reshape's nil check, never
// actually invoked — the sumMap reshape branch computes the ladder itself.
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
// sumMap + arrayCumSum reshape (SUM fold only, chopt.FeatureClassicBucketMergeSumMap).
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

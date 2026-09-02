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
// row's (bound, that row's CUMULATIVE count at that bound) pairs, whose
// key-wise sums ARE the merged per-`le` ladder. It is selected ONLY for the
// SUM fold — see ClassicBucketMergeLowerer (lower_strategy.go) and
// chopt.FeatureClassicBucketMergeSumMap.
//
// # Why each ROW cumulates before the merge, not the GROUP after it
//
// Reference Prometheus never sees a bucket layout: a classic histogram
// reaches it as one already-cumulative float series per `le`, so
// `sum by(le)` sums, at each bound u, ONLY the series that actually report
// `{le=u}` — a producer whose own layout has no u contributes nothing to
// rung u, however close its neighbouring bounds sit.
// classicBucketMergedLadderExpr reproduces that with its `has(pbs, u)`
// filter, and this file has to answer identically or it is a second, quieter
// answer to the same query.
//
// Cumulating each ROW over its own buckets first and keying the result by
// that row's OWN bounds is exactly that semantic, expressed as a key-wise
// sum: the row offers its cumulative count at u to key u for every u IT
// carries, and offers nothing at all at a union bound it does not carry. So
//
//	rung(u) = sum over the rows carrying u of that row's cumulative count at u
//
// which is classicBucketMergedLadderExpr's own definition, term for term,
// for ANY mix of layouts — homogeneous or not.
//
// Cumulating the GROUP afterward instead — sumMap over per-BUCKET counts,
// then arrayCumSum along the union, which is how this file first shipped —
// is a different quantity: it sums, at each union bound u, EVERY row's
// sub-cumulative count over its own buckets <= u, including rows with no u
// at all. The two agree only when every row carries every union bound.
// Cerberus issue #2817's worked case is the counterexample: bounds
// [1,2,3]/counts [10,5,0] merged with bounds [1,5]/counts [7,0] answered
// 1.78 where Prometheus (and the has-filter fold) answers 5.0.
//
// The quadratic term the old fold pays is its per-RUNG rescan of every
// contributing row; a per-row prefix sum is one pass over that row's own
// buckets, so moving the cumulation here keeps the linear cost this feature
// exists for.
//
// # The merged ladder still owes the monotonic repair
//
// Rungs folded independently per bound can DIP — a bound only one narrow
// producer carries sits below the rung beneath it — so this ladder, exactly
// like classicBucketMergedLadderExpr's, is handed to
// classicBucketMonotonicLadderExpr (Prometheus's own
// ensureMonotonicAndIgnoreSmallDeltas). classicBucketShaping.reshape runs
// BOTH ladders through that one repair layer rather than either path keeping
// its own copy.
//
// # Bound ORDER and repeated bounds inside one row
//
// The has-filter fold defines a row's contribution by `b <= u` over that
// row's raw (bound, count) pairs: independent of the order the row stores
// its bounds in, and counting a repeated bound's buckets exactly once. A
// prefix sum is neither, so the row's kept indices are ordered BY BOUND
// (classicBucketSumMapRowOrderedIndicesExpr) before the prefix sum, and
// every position but the LAST of an equal-bound run contributes 0
// (classicBucketSumMapRowCumulativeExpr). sumMap merges a row's repeated
// keys by SUMMING them (confirmed against chDB: sumMap([1,1,2],[3,4,5]) =
// ([1,2],[7,5])), so zeroing all but the last of a run is what makes the
// merged key hold that row's cumulative count at the repeated bound rather
// than a prefix of it counted several times.
//
// Neither step is defensive: test/spec/promql/histogram_quantile_classic_
// duplicate_bounds.txtar is a Prometheus-parity-enrolled fixture whose row
// reports the bound 1.0 twice, and the merge's own output layout is
// arraySort-ed (classicBucketUnionBoundsExpr), which is what lets the fold
// answer a row whose stored bounds are not ascending.
//
// # A real ClickHouse quirk this design works around
//
// sumMap(keys, values) DROPS any key whose summed value across the group is
// exactly zero — confirmed against real chDB execution (sumMap([1,2,3],
// [0,0,5]) over a single row returns keys=[3], NOT [1,2,3]). In the
// cumulative domain that is a LEADING run of empty buckets, which real
// traffic produces constantly: every bound below the smallest observation
// carries a cumulative count of exactly zero. Left alone the bound would
// vanish from the node's own output layout, and ExplicitBounds is the
// interpolation GEOMETRY HistogramQuantile walks.
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
// sm.values at its true position — one indexOf call, no branch. Zero is also
// the RIGHT value for a dropped key, not merely a safe one: sumMap drops it
// only when every row carrying that bound reports a cumulative count of
// zero there, which is precisely what the has-filter fold sums to.
//
// This costs O(W) per union bound (W = merged bucket-ladder width) against
// sumMap's own compact output, never against the T contributing rows — the
// quadratic T-row rescan classic_bucket_merge_bound.go's own doc audited is
// gone.
//
// # -0.0 / 0.0 key identity
//
// sumMap merges -0.0 and 0.0 as ONE aggregate key while arrayDistinct — the
// union-bounds dedup — keeps both (both confirmed against chDB by
// TestClassicBucketMergeSumMap_ZeroKeyIdentity), so a group whose rows
// report -0.0 from one row and 0.0 from another surfaces two union rungs
// resolving, via indexOf's own -0.0/0.0 equality, to the SAME sumMap entry.
// In the cumulative domain that is not a defect and needs no normalisation:
// two rungs at the same bound carrying the same cumulative count is exactly
// what the has-filter fold produces for that input (`has` matches both), and
// emitHistogramQuantile's own adjacent-duplicate-bound dedup collapses them
// downstream. It WAS a defect for the per-bucket-then-arrayCumSum shape this
// file first shipped, where the shared entry was ADDED once per duplicate
// rung — which is why the zero-canonicalising step that guarded it went away
// with the arrayCumSum it guarded.
//
// # NaN
//
// arrayCumSum propagates a NaN forward through the rest of the ROW it
// appears in (confirmed: arrayCumSum([1, nan, 2, 3]) = [1, nan, nan, nan]),
// which is the same reach classicBucketRowCumulativeExpr's arraySum gives it
// on the fold path: both poison that row's rungs at and above the NaN
// bucket, and neither reaches a bound the row does not carry. The repair
// layer both paths now share then answers the poisoned rungs identically
// (arrayMax ignores NaN — confirmed: arrayMax([1, nan, 2]) = 2). The
// asymmetric NaN reach #2756 documented as an accepted risk belonged to the
// union-wide arrayCumSum this file no longer performs.
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

// Lambda parameter names for this file's per-row expressions. `i` indexes a
// row's own ExplicitBounds / BucketCounts (the role paramLadderIdx plays over
// the merged ladder, one scope down); `smb` / `smc` are the
// single-evaluation bindings of one row's bound-ordered bounds and its
// prefix-summed counts; `smp` indexes those two in lockstep.
const (
	paramBucketIndex     = "i"
	paramSumMapRowBounds = "smb"
	paramSumMapRowCum    = "smc"
	paramSumMapRowPos    = "smp"
)

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

// classicBucketSumMapRowOrderedIndicesExpr returns the same kept (finite-
// bound) 1-based positions classicBucketFiniteBoundKeptIndicesExpr selects,
// permuted into ASCENDING BOUND order rather than stored-array order.
//
// The has-filter fold reads a row through `b <= u`, which does not care what
// order the row stores its bounds in; a prefix sum does, so this is what
// makes the two agree for a row whose ExplicitBounds is not ascending. It is
// also what puts an equal-bound run in ADJACENT positions, which is the
// precondition classicBucketSumMapRowCumulativeExpr's run collapse reads.
func classicBucketSumMapRowOrderedIndicesExpr(eb chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnArraySort, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramBucketIndex},
			Body: &chplan.Subscript{
				Container: eb, Key: &chplan.BareIdent{Name: paramBucketIndex},
			},
		},
		classicBucketFiniteBoundKeptIndicesExpr(eb),
	}}
}

// classicBucketSumMapRowCumulativeExpr renders ONE row's per-bound
// CUMULATIVE counts — the values sumMap keys by that row's own bounds — from
// its already bound-ordered bounds and per-bucket counts.
//
// It is a plain prefix sum with one correction: every position but the LAST
// of an equal-bound run contributes 0 instead of its own prefix. sumMap sums
// a row's repeated keys, so a run whose entries sum to the run's FINAL
// prefix is what makes the merged key hold "this row's count at or below
// that bound" once, rather than a prefix of it counted once per repeat. See
// this file's header.
//
// Both arrays are bound once (hqLet) because the run test reads the bounds
// twice and the prefix sum is read once per position: inlining either inside
// the arrayMap lambda would re-derive the whole per-row expression tree per
// element.
func classicBucketSumMapRowCumulativeExpr(orderedBounds, orderedCounts chplan.Expr) chplan.Expr {
	return hqLet(paramSumMapRowBounds, orderedBounds, func(bounds chplan.Expr) chplan.Expr {
		prefix := &chplan.FuncCall{Fn: chplan.FnArrayCumSum, Args: []chplan.Expr{orderedCounts}}
		return hqLet(paramSumMapRowCum, prefix, func(cum chplan.Expr) chplan.Expr {
			pos := chplan.Expr(&chplan.BareIdent{Name: paramSumMapRowPos})
			// pos < length(bounds) guards the lookahead: at the last
			// position bounds[pos+1] reads past the end, which ClickHouse
			// answers with the element type's default rather than an error
			// (confirmed against chDB), and that default is 0 — a real bound
			// value a row's highest bucket can legitimately carry.
			repeatsNext := &chplan.Binary{
				Op: chplan.OpAnd,
				Left: &chplan.Binary{
					Op:    chplan.OpLt,
					Left:  pos,
					Right: &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{bounds}},
				},
				Right: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.Subscript{Container: bounds, Key: pos},
					Right: &chplan.Subscript{Container: bounds, Key: addExpr(pos, &chplan.LitInt{V: 1})},
				},
			}
			return &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
				&chplan.Lambda{
					Params: []string{paramSumMapRowPos},
					Body: &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{
						repeatsNext,
						&chplan.LitFloat{V: 0},
						&chplan.Subscript{Container: cum, Key: pos},
					}},
				},
				&chplan.FuncCall{Fn: chplan.FnArrayEnumerate, Args: []chplan.Expr{bounds}},
			}}
		})
	})
}

// classicBucketSumMapRowArgs renders the per-row (bounds, cumulative counts)
// pair fed to the sumMap aggregate — the bounds half also feeding the
// union-bounds groupArray. Every INTERIOR non-finite bound is dropped (its
// paired count dropped in lockstep, preserving the #2495 -Inf guard the
// has-filter path applies at read time instead), the surviving pairs are put
// in ascending-bound order, and counts are cast to Float64 (the ladder is
// float-domain throughout — see classicBucketRowCumulativeExpr's identical
// reasoning) before the per-row prefix sum. The trailing overflow element
// BucketCounts may carry (one longer than ExplicitBounds) is excluded here —
// sumMap requires equal-length key/value arrays per row — and folded
// separately into hqAggInfTotalAlias via the RAW, unfiltered column instead.
func classicBucketSumMapRowArgs(s schema.Metrics) (bounds, counts chplan.Expr) {
	eb := chplan.Expr(&chplan.ColumnRef{Name: s.ExplicitBoundsColumn})
	bc := chplan.Expr(&chplan.ColumnRef{Name: s.BucketCountsColumn})
	orderedIdx := classicBucketSumMapRowOrderedIndicesExpr(eb)

	bounds = &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramBucketIndex},
			Body: &chplan.Subscript{
				Container: eb, Key: &chplan.BareIdent{Name: paramBucketIndex},
			},
		},
		orderedIdx,
	}}
	orderedCounts := &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramBucketIndex},
			Body: toFloat64Expr(&chplan.Subscript{
				Container: bc, Key: &chplan.BareIdent{Name: paramBucketIndex},
			}),
		},
		orderedIdx,
	}}
	return bounds, classicBucketSumMapRowCumulativeExpr(bounds, orderedCounts)
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

// classicBucketSumMapLookupExpr reconstructs the merged ladder's rung at
// union bound u from the group's sumMap result: sm.1 (keys) / sm.2 (values),
// via arrayConcat([0.0], sm.2)[indexOf(sm.1, u) + 1] — see this file's
// header for why a plain subscript into sm.2 at indexOf(sm.1, u) is unsafe
// (indexOf returns 0, an invalid subscript, on a miss — which sumMap's own
// zero-value key drop makes a real, expected case, not a defensive-only
// branch) and why 0.0 is the rung's correct value on such a miss.
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

// classicBucketSumMapLadderExpr renders the group's merged, NOT-yet-repaired
// cumulative per-`le` ladder over the union layout — one rung per union
// bound, read straight out of the sumMap result, plus the trailing +Inf rung
// (hqAggInfTotalAlias, an independent unconditional sum — see that alias'
// doc comment for why it needs no separate "add the last cumulative rung"
// step).
//
// This is classicBucketMergedLadderExpr's counterpart and stands in exactly
// the same place: classicBucketShaping.reshape aliases it to
// hqAggLadderAlias and hands it to classicBucketMonotonicLadderExpr, because
// per-bound rungs folded independently can dip. See this file's header.
func classicBucketSumMapLadderExpr() chplan.Expr {
	unionBounds := classicBucketUnionBoundsExpr()
	perBound := &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{Params: []string{paramUnionBound}, Body: classicBucketSumMapLookupExpr(&chplan.BareIdent{Name: paramUnionBound})},
		unionBounds,
	}}
	return &chplan.FuncCall{Fn: chplan.FnArrayConcat, Args: []chplan.Expr{
		perBound,
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
		// fold is never invoked when sumMap is set — classicBucketShaping
		// .reshape swaps in classicBucketSumMapLadderExpr for the ladder it
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

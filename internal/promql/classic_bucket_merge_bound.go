package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
)

// classic_bucket_merge_bound.go closes issue #2408's own audited "Proposed
// next step": classicBucketMergeShaping's cross-series groupArray-based
// merge (histogram_quantile.go / histogram_quantile_range.go — the shaping
// classicBucketMergeShaping builds, consumed by classicBucketUnionBoundsExpr
// and classicBucketMergedLadderExpr) runs identically regardless of which
// per-series lowering mechanism feeds it — fan-out or the native rate ladder
// (#2401) — and had NO resource bound of its own. A real, independently
// audited benchmark (real ClickHouse 25.9-alpine via testcontainers,
// production-shaped data) found this stage is why memory barely moves
// before/after either shipped per-series fix at realistic query width: the
// per-series mechanism's own savings are invisible once this shared merge's
// fixed cost dominates.
//
// # What the merge actually costs
//
// classicBucketMergeShaping's Aggregate collects two groupArrays per output
// group — hqAggBoundsListAlias (one ExplicitBounds array per row) and
// hqAggCountsListAlias (the paired BucketCounts) — one row per series (the
// instant-mode path, histogram_quantile.go) or per (anchor, series) window
// (the range-mode path, histogram_quantile_range.go's
// lowerHistogramQuantileClassicAggRange). classicBucketUnionBoundsExpr then
// flattens every row's bounds into one merged layout, and
// classicBucketMergedLadderExpr re-scans EVERY row's own bounds/counts pair
// once PER merged rung to fold a cumulative count at that rung — the same
// "per-target-element rescan of every contributing row" shape
// histogram_merge_bound.go's own doc found for the exponential-histogram
// merge, just over classic (bucket-array) histograms instead of
// exponential-scale ladders.
//
// Reading the emitted expression tree alone suggests a
// `(merged width) x (total bucket-element volume)` cost — quadratic in the
// fully-disjoint-layout case, since the merged width itself then equals the
// total volume. Real measurement (below) shows that is NOT what the real
// cost tracks, mirroring exactly the lesson histogram_merge_bound.go's own
// header draws for the exponential-histogram case ("NOT the (width) x
// (series) product #2385 assumed"): here too, the real driver is one factor
// short of the naive expression-tree read.
//
// # Calibration (real ClickHouse 25.9-alpine, 1 GiB cap — the
// # CERBERUS_CH_QUERY_MAX_MEMORY default)
//
// A throwaway harness (deleted before this fix merged, per
// rate_window_fanout_bound.go's own precedent) lowered + emitted the real
// `histogram_quantile(0.5, sum by(le)(sum_over_time(<bucket>[5m])))` SQL and
// ran it against a real `clickhouse/clickhouse-server:25.9-alpine` container
// with `max_memory_usage` set to the 1 GiB default, seeding series with
// DISJOINT ExplicitBounds layouts (series i's bounds are `[i*W+1 ..
// i*W+W]`) — the worst case for the merge, where every rung the union
// produces is contributed by exactly one row, maximising both the merged
// width and the per-rung rescan cost at once:
//
//	series(G)  width(W)  T=GxW    GxW^2       peak memory (real server)
//	    1000        20    20,000       400,000    28.8 MB
//	    2000        20    40,000       800,000    56.0 MB
//	    3741        12    44,892       538,704    49.4 MB  (this issue's own
//	                                                         reference series
//	                                                         count/rung width)
//	    2000        50   100,000     5,000,000   376.0 MB
//	    4000        50   200,000    10,000,000   749.0 MB  (70% of cap)
//	    6000        50   300,000    15,000,000   892.0 MB  (83% of cap)
//	    8000        50   400,000    20,000,000   REJECTED: real ClickHouse
//	                                              code-241 MEMORY_LIMIT_EXCEEDED
//	    3000       100   300,000    30,000,000   REJECTED (same code)
//	    5000       100   500,000    50,000,000   REJECTED (same code)
//	    8000       100   800,000    80,000,000   REJECTED (same code)
//
// Every completed (non-rejected) point above fits `G x W^2 x ~75 bytes/unit`
// to within ~5%: doubling G alone (2000->4000 series, W fixed at 50) almost
// exactly doubles peak memory (376.0 -> 749.0 MB), not the 4x a
// `G^2 x W^2` (equivalently `(merged width) x (total volume)`) model would
// predict — the real cost is LINEAR in series count at fixed width, one
// factor short of the naive expression-tree reading, exactly the "read the
// real emitted SQL and measure, don't guess from the shape" lesson this
// repo's own resource-bound family keeps re-learning (see
// histogram_merge_bound.go's identical caveat for the exponential-histogram
// case, and range_bucket_grid_native_bound.go's for the native ladder).
//
// The rejected points' own recorded memory_usage (offered by ClickHouse's
// exception at the exact allocation chunk that finally tripped the cap) sit
// BELOW what the query would have needed to complete — an aborted query's
// memory reading is a lower bound on the true peak, not the peak itself —
// so they are consistent with, not contradicting, the same `G x W^2` model
// extrapolated past 1 GiB.
//
// 15,000,000 units (6000 series x 50-width, 83% of cap) is the last
// confirmed-safe point measured; 20,000,000 (8000 series x 50-width) is the
// first confirmed rejection. [maxClassicBucketMergeCostUnits] is set well
// below that boundary — at 10,000,000 units, landing almost exactly on the
// 4000-series x 50-width point's own real 749 MB (70% of cap) — rather than
// at the edge of the measured-safe range, leaving real margin for concurrent
// load and for the guard's own generalisation to heterogeneous per-row
// widths (below) rather than the calibration's uniform-width seed.
//
// # Generalising to heterogeneous per-row widths
//
// The calibration above seeds every row in a group with the SAME width W,
// so `G x W^2` and `(total volume T) x (widest single row's own width)` are
// identical (T = GxW, so TxW = GxW^2). Real production data need not share
// one width across every row in a group — a `by(route)` group can mix rows
// from producers with different bucket configurations. [totalBucketVolumeExpr]
// / [widestRowBucketWidthExpr] compute T and the ACTUAL per-group maximum row
// width directly from the same hqAggBoundsListAlias groupArray the merge
// itself reads, so `T x maxRowWidth` is the real calibrated formula whenever
// widths are uniform, and a safe (never smaller) generalisation otherwise:
// `T x maxRowWidth >= T x avgRowWidth = G x avgRowWidth^2`, the same
// per-row-average form the calibration measured, for any actual distribution
// of per-row widths — so a heterogeneous group can only be judged MORE
// costly than its average-width equivalent, never less, by this formula.
//
// # Overflow safety
//
// [maxClassicBucketMergeClampedComponent] clamps BOTH T and maxRowWidth
// before the multiply — unlike histogram_merge_bound.go's rows x width^3
// formula, this one has only ONE data-derived shape (a product of two
// components, each individually clamped), so no separate overflow-only
// disjunct is needed: clamping every operand that reaches the multiply
// already bounds the product on its own. 100,000,000 x 100,000,000 = 10^16,
// three orders of magnitude below Int64's ~9.2x10^18 ceiling, while sitting
// many orders of magnitude above any real T or maxRowWidth this calibration
// or any legitimate production workload reaches — a query anywhere near that
// scale is already rejected by [maxClassicBucketMergeCostUnits] long before
// the clamp would matter.
//
// # Placement
//
// Wired via [wrapClassicBucketMergeBudgetGuard] as a [chplan.Filter]
// wrapping the raw groupArray-collecting Aggregate — BEFORE
// classicBucketShaping.reshape's own Projects render the union/ladder
// expressions this guard is protecting — at both call sites that build a
// classicBucketMergeShaping-shaped Aggregate: lowerHistogramQuantileAgg
// (histogram_quantile.go, instant mode) and
// lowerHistogramQuantileClassicAggRange (histogram_quantile_range.go, range
// mode — the exact function issue #2408's own audit names). The
// newest-row/argMax bare-selector shaping (classicBucketShaping{fold: nil})
// is never wrapped: its accumulator is fixed-size (one row per group), with
// no groupArray merge to bound.
const (
	// maxClassicBucketMergeCostUnits bounds `totalBucketVolume x
	// widestRowBucketWidth` — see this file's header doc for the real
	// ClickHouse 25.9-alpine calibration this was picked against.
	// Recalibrate by binary search against a real ClickHouse server (not
	// chDB — see rate_window_fanout_bound.go and histogram_merge_bound.go's
	// own notes on why an in-process engine does not reliably surface a
	// real max_memory_usage abort) if this drifts; preserve the `T x
	// maxRowWidth` model unless a new measurement shows the real cost
	// driver has moved again.
	maxClassicBucketMergeCostUnits = 10_000_000

	// maxClassicBucketMergeClampedComponent is NOT a behavioral threshold —
	// see this file's header doc's Overflow safety section. It exists
	// purely so the `T x maxRowWidth` multiply can never overflow Int64
	// arithmetic; a query anywhere near this scale is already rejected by
	// maxClassicBucketMergeCostUnits long before this clamp matters.
	maxClassicBucketMergeClampedComponent = 100_000_000
)

// classicBucketMergeRowWidthParam names the lambda parameter
// classicBucketMergeRowWidthsExpr binds while reducing hqAggBoundsListAlias's
// per-row arrays down to their lengths — distinct from histogram_quantile.go's
// own paramRowBounds (a per-ROW bounds ARRAY, bound while folding the merge
// itself) since this file's lambda binds a per-row LENGTH, a different value
// at a different scope.
const classicBucketMergeRowWidthParam = "cbmw"

// classicBucketMergeRowWidthsExpr renders one array holding every row's own
// ExplicitBounds length in the group — the same per-row width
// classicBucketRowCumulativeExpr's O(row width) rescan pays for once per
// merged rung. Called fresh (a new Go expression tree) by both
// totalBucketVolumeExpr and widestRowBucketWidthExpr below: unlike
// histogram_native_count_values.go's hqLet users, this reduction is only
// O(group size) — reading array lengths, not re-deriving anything — so
// rendering it twice costs nothing worth a let-binding for.
func classicBucketMergeRowWidthsExpr() chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{classicBucketMergeRowWidthParam},
			Body: &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{
				&chplan.BareIdent{Name: classicBucketMergeRowWidthParam},
			}},
		},
		&chplan.ColumnRef{Name: hqAggBoundsListAlias},
	}}
}

// totalBucketVolumeExpr renders T: the sum, across every row in the group,
// of that row's own ExplicitBounds length — the total bucket-element volume
// classicBucketUnionBoundsExpr's flatten reads and classicBucketMergedLadderExpr's
// per-rung rescan pays for repeatedly.
func totalBucketVolumeExpr() chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnArraySum, Args: []chplan.Expr{classicBucketMergeRowWidthsExpr()}}
}

// widestRowBucketWidthExpr renders the group's single widest row (by
// ExplicitBounds length) — see this file's header doc for why `T x
// widestRowWidth`, not `T` alone or `T^2`, is the calibrated cost model.
func widestRowBucketWidthExpr() chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnArrayMax, Args: []chplan.Expr{classicBucketMergeRowWidthsExpr()}}
}

// classicBucketMergeCostOverBudgetExpr renders the `<cost> >
// maxClassicBucketMergeCostUnits` condition this file's header doc's
// calibration backs: `totalBucketVolume x widestRowBucketWidth`. Both
// operands are clamped to maxClassicBucketMergeClampedComponent first —
// see that constant's own doc and this file's Overflow safety section for
// why clamping both components (rather than a separate business-threshold
// disjunct, as histogram_merge_bound.go's rows x width^3 model needs) is
// enough to keep the multiply Int64-safe here.
func classicBucketMergeCostOverBudgetExpr() chplan.Expr {
	clampedTotal := &chplan.FuncCall{Fn: chplan.FnLeast, Args: []chplan.Expr{
		totalBucketVolumeExpr(), &chplan.LitInt{V: maxClassicBucketMergeClampedComponent},
	}}
	clampedWidest := &chplan.FuncCall{Fn: chplan.FnLeast, Args: []chplan.Expr{
		widestRowBucketWidthExpr(), &chplan.LitInt{V: maxClassicBucketMergeClampedComponent},
	}}
	cost := mulExpr(clampedTotal, clampedWidest)
	return gtLit(cost, maxClassicBucketMergeCostUnits)
}

// classicBucketMergeBudgetGuardExpr renders the throwIf(...) = 0 predicate
// that bounds the shared classic-histogram cross-series merge. It reads the
// SAME hqAggBoundsListAlias groupArray classicBucketMergeShaping collects,
// so it must be attached directly above the Aggregate that produces it —
// see wrapClassicBucketMergeBudgetGuard.
func classicBucketMergeBudgetGuardExpr() chplan.Expr {
	return &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.FuncCall{
			Fn: chplan.FnThrowIf,
			Args: []chplan.Expr{
				classicBucketMergeCostOverBudgetExpr(),
				&chplan.InlineString{V: chplan.ClassicBucketMergeBudgetMessage},
			},
		},
		Right: &chplan.LitInt{V: 0},
	}
}

// wrapClassicBucketMergeBudgetGuard wraps agg — the raw
// classicBucketMergeShaping Aggregate (collecting hqAggBoundsListAlias /
// hqAggCountsListAlias) — in the budget guard's Filter. Both callers that
// build a classicBucketMergeShaping-shaped Aggregate
// (lowerHistogramQuantileAgg, histogram_quantile.go; and
// lowerHistogramQuantileClassicAggRange, histogram_quantile_range.go) call
// this before handing the Aggregate to classicBucketShaping.reshape, so a
// group this guard rejects never pays for the union/ladder Projects
// reshape adds either.
func wrapClassicBucketMergeBudgetGuard(agg chplan.Node) chplan.Node {
	return &chplan.Filter{Input: agg, Predicate: classicBucketMergeBudgetGuardExpr()}
}
